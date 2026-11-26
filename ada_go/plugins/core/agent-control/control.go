// Package controlgo 是 control 的 Go 实现:把 agent.runner 与 approval 接到
// 语言中立的 control.Invoker 上。
//
// 它不导入具体 provider,只消费 seam;TS 实现则通过 bridge 提供同名的
// control 服务。两种实现不能同时装配(kernel 同名服务冲突)。
package controlgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// PluginName 是插件 id。
const PluginName = "agent-control"

type managed struct {
	sess      session.Session
	createdAt time.Time
	updatedAt time.Time
	cancel    context.CancelFunc
	seq       uint64
}

// Service 是 Go 侧的控制层实现。
type Service struct {
	runner    seamagent.Runner
	approval  *approval.Registry
	workspace *workspace.Chain
	bus       *kernel.Context

	mu       sync.Mutex
	sessions map[string]*managed
}

// New 构造控制层。
func New(runner seamagent.Runner, approvalReg *approval.Registry) *Service {
	return &Service{runner: runner, approval: approvalReg, sessions: make(map[string]*managed)}
}

// Invoke 实现 control.Invoker。
func (s *Service) Invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case control.MethodSessionCreate:
		return s.create(ctx, params)
	case control.MethodSessionLoad:
		return s.load(ctx, params)
	case control.MethodSessionList:
		return s.list()
	case control.MethodSessionPrompt:
		return s.prompt(ctx, params)
	case control.MethodSessionHistory:
		return s.history(params)
	case control.MethodSessionCancel:
		return s.cancel(params)
	case control.MethodSessionPermission:
		return s.permission(ctx, params)
	case control.MethodSessionClose:
		return s.close(params)
	default:
		return nil, control.Unimplemented("未知控制方法: " + method)
	}
}

func (s *Service) create(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req control.CreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, control.Invalid("session.create 参数非法: " + err.Error())
	}
	if s.runner == nil {
		return nil, control.Unimplemented("没有 agent.runner")
	}
	meta := session.Meta{Title: req.Title}
	if req.Cwd != "" {
		id, err := s.bindWorkspace(ctx, req.Cwd)
		if err != nil {
			return nil, err
		}
		meta.Workspace = id
	}
	sess, err := s.runner.Open(ctx, meta)
	if err != nil {
		return nil, control.Unavailable("创建会话失败: " + err.Error())
	}
	now := time.Now()
	m := &managed{sess: sess, createdAt: now, updatedAt: now}
	s.mu.Lock()
	s.sessions[sess.ID()] = m
	s.mu.Unlock()
	return json.Marshal(s.info(sess.ID(), m))
}

func (s *Service) load(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.SessionID == "" {
		return nil, control.Invalid("session.load 需要 sessionId")
	}
	s.mu.Lock()
	m := s.sessions[req.SessionID]
	s.mu.Unlock()
	if m == nil {
		// 内存表里没有时尝试让 runner 按 id 恢复;恢复失败保持「会话不存在」。
		m = s.resume(ctx, req.SessionID)
		if m == nil {
			return nil, control.Invalid("会话不存在: " + req.SessionID)
		}
	}
	return json.Marshal(s.info(req.SessionID, m))
}

// bindWorkspace 把 ACP 的 cwd 映射到已配置工作区。
//
// 没有 workspace 服务或 provider 时保持 provider 的默认根(旧 preset 不受影响);
// 有 provider 但 cwd 不在任何工作区根下时拒绝创建,避免会话悄悄编辑另一个目录。
func (s *Service) bindWorkspace(ctx context.Context, cwd string) (string, error) {
	if s.workspace == nil || cwd == "" {
		return "", nil
	}
	svc, err := s.workspace.Get()
	if err != nil {
		return "", nil
	}
	loc, ok := svc.(workspace.Locator)
	if !ok {
		return "", nil
	}
	info, found, err := loc.Locate(ctx, cwd)
	if err != nil {
		return "", control.Invalid("定位 cwd 工作区失败: " + err.Error())
	}
	if !found {
		return "", control.Invalid("cwd 不属于任何已配置工作区: " + cwd)
	}
	return string(info.ID), nil
}

// resume 让 runner 按 id 恢复会话并登记;不实现 Resumer 或恢复失败都返回 nil。
func (s *Service) resume(ctx context.Context, id string) *managed {
	r, ok := s.runner.(seamagent.Resumer)
	if !ok {
		return nil
	}
	sess, err := r.Resume(ctx, id)
	if err != nil {
		return nil
	}
	now := time.Now()
	m := &managed{sess: sess, createdAt: now, updatedAt: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sessions[id]; ok {
		return existing
	}
	s.sessions[id] = m
	return m
}

// history 返回会话的既有事件,投影成控制层更新;顺序与存储一致。
func (s *Service) history(raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.SessionID == "" {
		return nil, control.Invalid("session.history 需要 sessionId")
	}
	s.mu.Lock()
	m := s.sessions[req.SessionID]
	s.mu.Unlock()
	if m == nil || m.sess == nil {
		return nil, control.Invalid("会话不存在: " + req.SessionID)
	}
	events, err := m.sess.Events(0, 0)
	if err != nil {
		return nil, control.Unavailable("读取会话历史失败: " + err.Error())
	}
	out := make([]control.Update, 0, len(events))
	for _, ev := range events {
		out = append(out, control.Update{
			SessionID: req.SessionID, TurnID: ev.TurnID, Kind: ev.Type, Data: ev.Data,
		})
	}
	return json.Marshal(out)
}

func (s *Service) list() (json.RawMessage, error) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]control.SessionInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.info(id, s.sessions[id]))
	}
	s.mu.Unlock()
	return json.Marshal(out)
}

func (s *Service) prompt(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req control.PromptRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.SessionID == "" {
		return nil, control.Invalid("session.prompt 需要 sessionId")
	}
	if req.Text == "" {
		return nil, control.Invalid("session.prompt 需要 text")
	}
	s.mu.Lock()
	m := s.sessions[req.SessionID]
	s.mu.Unlock()
	if m == nil {
		return nil, control.Invalid("会话不存在: " + req.SessionID)
	}
	if s.runner == nil {
		return nil, control.Unimplemented("没有 agent.runner")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if m.cancel != nil {
		// 同一会话同时只允许一轮;并发 prompt 会让事件、取消与历史互相踩踏。
		s.mu.Unlock()
		cancel()
		return nil, control.Invalid("会话已有进行中的 prompt: " + req.SessionID)
	}
	m.seq++
	seq := m.seq
	m.cancel = cancel
	m.updatedAt = time.Now()
	s.mu.Unlock()

	// 本轮更新分两路进入 out:session 事件(工具/消息)与 agent/delta(token 增量)。
	// 只有一个 forwarder 往总线写,保证 control/update 顺序稳定。
	emitCtx := context.WithoutCancel(ctx)
	out := make(chan control.Update, 256)
	forwardDone := make(chan struct{})
	go func() {
		defer close(forwardDone)
		for u := range out {
			s.emitUpdate(emitCtx, u)
		}
	}()

	type streamKey struct{ turnID, stepID string }
	var (
		streamMu sync.Mutex
		streamed = make(map[streamKey]bool)
	)
	// assistantStreamed 判断 assistant/message 是否已有 token 增量:
	// 有则跳过整条消息,避免客户端重复显示同一段文本。
	assistantStreamed := func(ev session.Event) bool {
		if ev.Type != session.TypeAssistantMessage {
			return false
		}
		streamMu.Lock()
		skip := streamed[streamKey{ev.TurnID, ev.StepID}]
		streamMu.Unlock()
		return skip
	}

	var updates <-chan session.Event
	var dispose kernel.Dispose
	if s.bus != nil && m.sess != nil {
		from := uint64(1)
		if events, eerr := m.sess.Events(0, 0); eerr == nil && len(events) > 0 {
			from = events[len(events)-1].Seq + 1
		}
		updates, dispose = m.sess.Subscribe(from)
	}
	stop := make(chan struct{})
	sessionDone := make(chan struct{})
	if updates != nil {
		go func() {
			defer close(sessionDone)
			push := func(ev session.Event) {
				if assistantStreamed(ev) {
					return
				}
				out <- control.Update{
					SessionID: req.SessionID, TurnID: ev.TurnID, Kind: ev.Type, Data: ev.Data,
				}
			}
			for {
				select {
				case ev, ok := <-updates:
					if !ok {
						return
					}
					push(ev)
				case <-stop:
					// Run 已返回,把订阅队列里剩余的事件排空后退出。
					for {
						select {
						case ev, ok := <-updates:
							if !ok {
								return
							}
							push(ev)
						default:
							return
						}
					}
				}
			}
		}()
	}

	var disposeDelta kernel.Dispose
	if s.bus != nil {
		disposeDelta = s.bus.On(seamagent.EventDelta, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
			if len(args) == 0 {
				return nil, nil
			}
			d, ok := args[0].(seamagent.DeltaEvent)
			if !ok || d.SessionID != req.SessionID || d.Text == "" {
				return nil, nil
			}
			kind := ""
			switch d.Kind {
			case string(llm.DeltaText):
				kind = control.UpdateAssistantDelta
			case string(llm.DeltaReasoning):
				kind = control.UpdateAssistantReasoning
			default:
				return nil, nil
			}
			if kind == control.UpdateAssistantDelta {
				// 只有正文增量会抑制整段 assistant/message;推理段不能。
				streamMu.Lock()
				streamed[streamKey{d.TurnID, d.StepID}] = true
				streamMu.Unlock()
			}
			data, _ := json.Marshal(map[string]string{"text": d.Text})
			out <- control.Update{
				SessionID: d.SessionID, TurnID: d.TurnID, Kind: kind, Data: data,
			}
			return nil, nil
		})
	}

	res, err := s.runner.Run(runCtx, m.sess, req.Text)
	cancel()
	if updates != nil {
		close(stop)
		<-sessionDone
	}
	if disposeDelta != nil {
		_ = disposeDelta(context.Background())
	}
	close(out)
	<-forwardDone
	if dispose != nil {
		_ = dispose(context.Background())
	}
	s.mu.Lock()
	if m.seq == seq {
		m.cancel = nil
	}
	m.updatedAt = time.Now()
	s.mu.Unlock()
	if err != nil {
		// 用户取消不是失败:返回 cancelled,协议适配器据此回 ACP stopReason。
		if errors.Is(err, context.Canceled) || seamerr.Is(err, seamerr.ErrCanceled) {
			return json.Marshal(control.TurnResult{ID: res.TurnID, StopReason: "cancelled"})
		}
		return nil, control.Unavailable("prompt 失败: " + err.Error())
	}
	return json.Marshal(control.TurnResult{ID: res.TurnID, Text: res.Text, StopReason: "end_turn"})
}

// emitUpdate 把一条控制更新广播到总线;输出顺序由调用方保证。
func (s *Service) emitUpdate(ctx context.Context, u control.Update) {
	if s.bus == nil {
		return
	}
	s.bus.EmitContext(ctx, control.EventUpdate, u)
}

func (s *Service) cancel(raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.SessionID == "" {
		return nil, control.Invalid("session.cancel 需要 sessionId")
	}
	s.mu.Lock()
	m := s.sessions[req.SessionID]
	if m != nil && m.cancel != nil {
		m.cancel()
	}
	s.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

func (s *Service) permission(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var req control.PermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, control.Invalid("session.permission 参数非法: " + err.Error())
	}
	if s.approval == nil {
		return nil, control.Unimplemented("没有 approval 服务")
	}
	details := map[string]any{"sessionId": req.SessionID}
	if req.ToolCallID != "" {
		details["toolCallId"] = req.ToolCallID
	}
	decision, err := approval.Ask(ctx, s.approval, req.Backend, approval.Request{
		Source: "control", Op: req.Tool, Target: req.Title, Reason: req.Detail,
		Details: details,
	})
	if err != nil {
		return nil, control.Denied("权限请求失败: " + err.Error())
	}
	out := control.PermissionDecision{OptionID: control.OptionRejectOnce}
	if decision.Allow {
		out.OptionID = control.OptionAllowOnce
		for _, o := range req.Options {
			if optionAllows(o) {
				out.OptionID = o.ID
				break
			}
		}
	}
	return json.Marshal(out)
}

// optionAllows 判断选项是否表示放行;Kind 缺失时回退到 ID。
func optionAllows(o control.PermissionOption) bool {
	switch o.Kind {
	case control.OptionAllowOnce, control.OptionAllowAlways:
		return true
	}
	switch o.ID {
	case control.OptionAllowOnce, control.OptionAllowAlways:
		return true
	}
	return false
}

func (s *Service) close(raw json.RawMessage) (json.RawMessage, error) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.SessionID == "" {
		return nil, control.Invalid("session.close 需要 sessionId")
	}
	s.mu.Lock()
	m := s.sessions[req.SessionID]
	delete(s.sessions, req.SessionID)
	if m != nil && m.cancel != nil {
		m.cancel()
	}
	s.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

func (s *Service) info(id string, m *managed) control.SessionInfo {
	title := ""
	if m != nil && m.sess != nil {
		title = m.sess.Meta().Title
	}
	info := control.SessionInfo{ID: id, Title: title}
	if m != nil {
		info.CreatedAt = m.createdAt
		info.UpdatedAt = m.updatedAt
	}
	return info
}

// Plugin 返回 Go 控制层插件。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		runner, err := kernel.Service[seamagent.Runner](ctx, "agent.runner")
		if err != nil {
			return nil, err
		}
		var approvalReg *approval.Registry
		if v, ok := ctx.TryService("approval"); ok {
			approvalReg, _ = v.(*approval.Registry)
		}
		svc := New(runner, approvalReg)
		svc.bus = ctx
		if v, ok := ctx.TryService(workspace.ServiceName); ok {
			svc.workspace, _ = v.(*workspace.Chain)
		}
		_, err = ctx.Provide(control.ServiceName, svc, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithInject("agent.runner").WithOptional("approval", workspace.ServiceName).WithProvide(control.ServiceName)
}

// 编译期确认实现。
var _ control.Invoker = (*Service)(nil)

// String 便于诊断。
func (s *Service) String() string {
	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	return fmt.Sprintf("control-go(%d sessions)", n)
}
