// Package controlgo 是 control 的 Go 实现:把 agent.runner 与 approval 接到
// 语言中立的 control.Invoker 上。
//
// 它不导入具体 provider,只消费 seam;TS 实现则通过 bridge 提供同名的
// control 服务。两种实现不能同时装配(kernel 同名服务冲突)。
package controlgo

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
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
	runner   seamagent.Runner
	approval *approval.Registry

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
		return s.load(params)
	case control.MethodSessionList:
		return s.list()
	case control.MethodSessionPrompt:
		return s.prompt(ctx, params)
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
	sess, err := s.runner.Open(ctx, session.Meta{Title: req.Title})
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

func (s *Service) load(raw json.RawMessage) (json.RawMessage, error) {
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
		return nil, control.Invalid("会话不存在: " + req.SessionID)
	}
	return json.Marshal(s.info(req.SessionID, m))
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
	m.seq++
	seq := m.seq
	m.cancel = cancel
	m.updatedAt = time.Now()
	s.mu.Unlock()
	res, err := s.runner.Run(runCtx, m.sess, req.Text)
	cancel()
	s.mu.Lock()
	if m.seq == seq {
		m.cancel = nil
	}
	m.updatedAt = time.Now()
	s.mu.Unlock()
	if err != nil {
		return nil, control.Unavailable("prompt 失败: " + err.Error())
	}
	return json.Marshal(control.TurnResult{ID: res.TurnID, Text: res.Text, StopReason: "end_turn"})
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
	decision, err := approval.Ask(ctx, s.approval, "", approval.Request{
		Source: "control", Op: req.Tool, Target: req.Title, Reason: req.Detail,
		Details: map[string]any{"sessionId": req.SessionID},
	})
	if err != nil {
		return nil, control.Denied("权限请求失败: " + err.Error())
	}
	out := control.PermissionDecision{OptionID: "deny"}
	if decision.Allow {
		out.OptionID = "allow"
		if len(req.Options) > 0 {
			out.OptionID = req.Options[0].ID
		}
	}
	return json.Marshal(out)
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
		_, err = ctx.Provide(control.ServiceName, svc, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithInject("agent.runner").WithOptional("approval").WithProvide(control.ServiceName)
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
