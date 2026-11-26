// Package agentloop 是 `core/agent-loop`:最小的一轮 agent 循环。
//
// 它只消费 session/prompts/llm/tools 四个 seam;模型调用、工具执行、会话存储
// 都不在这里实现(seam-api §6)。
package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/prompts"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// PluginName 是插件 id。
const PluginName = "agent-loop"

// DefaultMaxParallelTools 是同一步内并行工具的上限。
const DefaultMaxParallelTools = 4

type config struct {
	Model         string `json:"model,omitempty"`
	MaxTokens     int    `json:"maxTokens,omitempty"`
	PromptBudget  int    `json:"promptBudget,omitempty"`
	ContextBudget int    `json:"contextBudget,omitempty"`
	MaxParallel   int    `json:"maxParallelTools,omitempty"`
}

// Agent 是对外服务值:创建会话并跑一轮。
type Agent struct {
	bus      *kernel.Context
	sessions *session.Registry
	prompts  *prompts.Engine
	llms     *llm.Registry
	tools    *tools.Registry
	conf     config
}

// TurnResult 是一轮的结果。
type TurnResult struct {
	TurnID    string    `json:"turnId"`
	Text      string    `json:"text"`
	Steps     int       `json:"steps"`
	ToolCalls int       `json:"toolCalls"`
	Usage     llm.Usage `json:"usage"`
}

// seamRunner 把 Agent 适配成 seam/agent.Runner,供控制层使用。
type seamRunner struct{ *Agent }

func (r seamRunner) Run(ctx context.Context, sess session.Session, userText string) (seamagent.Result, error) {
	res, err := r.Agent.Run(ctx, sess, userText)
	if err != nil {
		return seamagent.Result{}, err
	}
	return seamagent.Result{
		TurnID: res.TurnID, Text: res.Text, Steps: res.Steps, ToolCalls: res.ToolCalls,
	}, nil
}

var _ seamagent.Runner = seamRunner{}

// Open 创建一个新会话。
func (a *Agent) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return a.sessions.Open(ctx, "", meta)
}

// Run 跑一轮:append user → assemble → stream → tools → append turn/end。
func (a *Agent) Run(ctx context.Context, sess session.Session, userText string) (TurnResult, error) {
	if sess == nil {
		return TurnResult{}, fmt.Errorf("agent-loop: session 不能为 nil")
	}
	if userText == "" {
		return TurnResult{}, fmt.Errorf("agent-loop: 用户消息不能为空")
	}
	// 工作区绑定来自 session.Meta,不另存会话→工作区映射;没有绑定时由
	// workspace provider 的 Current 决定,旧 preset 完全不受影响。
	if id := sess.Meta().Workspace; id != "" {
		ctx = workspace.WithBinding(ctx, workspace.Binding{
			SessionID: sess.ID(), Workspace: workspace.ID(id),
		})
	}
	model, err := a.model(ctx)
	if err != nil {
		return TurnResult{}, err
	}

	prev := sess.Turn()
	turn := session.TurnInfo{
		ID:      fmt.Sprintf("turn-%d", prev.Index+1),
		Index:   prev.Index + 1,
		State:   session.TurnRunning,
		Started: time.Now().UTC(),
	}
	if _, err := sess.Append(session.Event{
		Type: session.TypeTurnStart, TurnID: turn.ID,
		Data: mustJSON(turn), Source: "core/agent-loop",
	}); err != nil {
		return TurnResult{}, err
	}
	user := llm.Message{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: userText}}}
	if _, err := sess.Append(session.Event{
		Type: session.TypeUserMessage, TurnID: turn.ID,
		Data: mustJSON(user), Source: "core/agent-loop",
	}); err != nil {
		return TurnResult{}, err
	}

	res := TurnResult{TurnID: turn.ID}
	// 不设步数上限:由模型「不再调用工具」、工具返回 Terminate、上下文取消/报错等
	// 自然结束。调用方必须通过 ctx 超时或取消承担兜底责任。
	for step := 1; ; step++ {
		if err := ctx.Err(); err != nil {
			return a.abort(sess, turn, res, err)
		}
		res.Steps = step
		stepID := fmt.Sprintf("%s/step-%d", turn.ID, step)
		if _, err := sess.Append(session.Event{
			Type: session.TypeStepStart, TurnID: turn.ID, StepID: stepID,
			Data: mustJSON(map[string]any{"index": step}), Source: "core/agent-loop",
		}); err != nil {
			return TurnResult{}, err
		}

		asm, err := a.prompts.Assemble(ctx, prompts.AssembleInput{
			SessionID: sess.ID(), TurnID: turn.ID, StepID: stepID,
			Model: model, Budget: a.conf.PromptBudget,
		})
		if err != nil {
			return a.abort(sess, turn, res, err)
		}

		if err := a.checkContext(ctx, sess); err != nil {
			return a.abort(sess, turn, res, err)
		}
		req := llm.Request{
			Model: model.ID, Messages: a.render(sess),
			Tools: a.toolSchemas(ctx), SessionID: sess.ID(), TurnID: turn.ID, StepID: stepID,
			MaxTokens: a.conf.MaxTokens,
		}
		req.Messages = append([]llm.Message{{Role: llm.RoleSystem, Content: []llm.Block{{Kind: llm.BlockText, Text: asm.Text}}}}, req.Messages...)

		if a.bus != nil {
			if v, err := a.bus.Waterfall(ctx, llm.EventRequest, &req); err != nil {
				return a.abort(sess, turn, res, err)
			} else if r, ok := v.(*llm.Request); ok && r != nil {
				req = *r
			}
		}
		provider, err := a.llms.Get("")
		if err != nil {
			return a.abort(sess, turn, res, err)
		}
		resp, err := provider.Stream(ctx, req, func(d llm.Delta) error {
			if a.bus != nil {
				delta := d
				if _, werr := a.bus.Waterfall(ctx, llm.EventStream, &delta); werr != nil {
					return werr
				}
			}
			return nil
		})
		if err != nil {
			if a.bus != nil {
				a.bus.EmitContext(ctx, llm.EventError, err)
			}
			return a.abort(sess, turn, res, err)
		}
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		if a.bus != nil {
			a.bus.EmitContext(ctx, llm.EventResponse, &resp)
		}

		payload := assistantPayload{Message: resp.Message, ToolCalls: resp.ToolCalls, Usage: resp.Usage}
		payload.Message.ToolCalls = resp.ToolCalls
		if _, err := sess.Append(session.Event{
			Type: session.TypeAssistantMessage, TurnID: turn.ID, StepID: stepID,
			Data: mustJSON(payload), Source: "core/agent-loop",
		}); err != nil {
			return TurnResult{}, err
		}
		res.Text = resp.Message.Text()

		if len(resp.ToolCalls) == 0 {
			if _, err := sess.Append(session.Event{
				Type: session.TypeStepEnd, TurnID: turn.ID, StepID: stepID, Source: "core/agent-loop",
			}); err != nil {
				return TurnResult{}, err
			}
			return a.finish(sess, turn, res)
		}

		terminate := false
		count, terminated, err := a.runTools(ctx, sess, turn, stepID, resp.ToolCalls)
		if err != nil {
			return TurnResult{}, err
		}
		res.ToolCalls += count
		terminate = terminated
		if _, err := sess.Append(session.Event{
			Type: session.TypeStepEnd, TurnID: turn.ID, StepID: stepID, Source: "core/agent-loop",
		}); err != nil {
			return TurnResult{}, err
		}
		if terminate {
			return a.finish(sess, turn, res)
		}
	}
}

type toolOutcome struct {
	call   tools.CallInfo
	result tools.Result
	err    error
}

// runTools 先按调用顺序写 tool/call,再并发执行,最后仍按调用顺序写 tool/result。
//
// 事件顺序固定,模型看到的结果顺序与 tool_calls 顺序一致;并发只发生在执行阶段。
func (a *Agent) runTools(ctx context.Context, sess session.Session, turn session.TurnInfo, stepID string, calls []llm.ToolCall) (int, bool, error) {
	infos := make([]tools.CallInfo, len(calls))
	for i, call := range calls {
		infos[i] = tools.CallInfo{
			ID: call.ID, Name: call.Name, Args: call.Args,
			SessionID: sess.ID(), TurnID: turn.ID, StepID: stepID, Origin: "model",
		}
		if _, err := sess.Append(session.Event{
			Type: session.TypeToolCall, TurnID: turn.ID, StepID: stepID,
			Data: mustJSON(infos[i]), Source: "core/agent-loop",
		}); err != nil {
			return 0, false, err
		}
	}

	limit := a.conf.MaxParallel
	if limit <= 0 {
		limit = DefaultMaxParallelTools
	}
	outcomes := make([]toolOutcome, len(calls))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range infos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			outcomes[i].call = infos[i]
			outcomes[i].result, outcomes[i].err = a.tools.Invoke(ctx, infos[i])
		}(i)
	}
	wg.Wait()

	terminate := false
	for i := range outcomes {
		tr := outcomes[i].result
		if outcomes[i].err != nil {
			tr = tools.Result{Status: tools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: outcomes[i].err.Error()}}}
		}
		if tr.Terminate {
			terminate = true
		}
		if _, err := sess.Append(session.Event{
			Type: session.TypeToolResult, TurnID: turn.ID, StepID: stepID,
			Data: mustJSON(toolPayload{Call: outcomes[i].call, Result: tr}), Source: "core/agent-loop",
		}); err != nil {
			return i, terminate, err
		}
	}
	return len(outcomes), terminate, nil
}

// checkContext 用 turn-stats 投影估算上下文;超预算时尝试 compaction 服务。
// 投影未挂载时保持旧行为。
func (a *Agent) checkContext(ctx context.Context, sess session.Session) error {
	if a.conf.ContextBudget <= 0 {
		return nil
	}
	ts, err := a.stats(sess)
	if err != nil {
		return err
	}
	if ts == nil || ts.TokenCount() <= a.conf.ContextBudget {
		return nil
	}
	if a.bus != nil {
		if raw, ok := a.bus.TryService("compaction"); ok {
			if c, ok := raw.(interface {
				Compact(context.Context, session.Session, any) error
			}); ok {
				if err := c.Compact(ctx, sess, ts); err != nil {
					return err
				}
				ts, err = a.stats(sess)
				if err != nil {
					return err
				}
				if ts != nil && ts.TokenCount() <= a.conf.ContextBudget {
					return nil
				}
			}
		}
	}
	tokens := 0
	if ts != nil {
		tokens = ts.TokenCount()
	}
	return seamerr.New(seamerr.ErrContext, "agent-loop",
		fmt.Sprintf("上下文估算 %d tokens 超过预算 %d", tokens, a.conf.ContextBudget), false)
}

func (a *Agent) stats(sess session.Session) (interface{ TokenCount() int }, error) {
	v, err := sess.Project(session.ProjectionTurnStats)
	if err != nil {
		if seamerr.Is(err, seamerr.ErrUnimplemented) {
			return nil, nil
		}
		return nil, err
	}
	ts, ok := v.(interface{ TokenCount() int })
	if !ok {
		return nil, nil
	}
	return ts, nil
}

func (a *Agent) compactedTo(sess session.Session) uint64 {
	v, err := sess.Project(session.ProjectionTurnStats)
	if err != nil {
		return 0
	}
	if s, ok := v.(interface{ CompactedToSeq() uint64 }); ok {
		return s.CompactedToSeq()
	}
	return 0
}

func (a *Agent) finish(sess session.Session, turn session.TurnInfo, res TurnResult) (TurnResult, error) {
	turn.State = session.TurnDone
	if _, err := sess.Append(session.Event{
		Type: session.TypeTurnEnd, TurnID: turn.ID,
		Data: mustJSON(turn), Source: "core/agent-loop",
	}); err != nil {
		return TurnResult{}, err
	}
	return res, nil
}

func (a *Agent) abort(sess session.Session, turn session.TurnInfo, res TurnResult, cause error) (TurnResult, error) {
	turn.State = session.TurnAborted
	_, _ = sess.Append(session.Event{
		Type: session.TypeTurnEnd, TurnID: turn.ID,
		Data: mustJSON(turn), Source: "core/agent-loop",
	})
	return res, cause
}

func (a *Agent) model(ctx context.Context) (llm.ModelInfo, error) {
	models, err := a.llms.Models(ctx, "")
	if err != nil {
		return llm.ModelInfo{}, err
	}
	for _, m := range models {
		if a.conf.Model == "" || m.ID == a.conf.Model {
			return m, nil
		}
	}
	if a.conf.Model != "" {
		return llm.ModelInfo{ID: a.conf.Model, ContextWindow: 8192}, nil
	}
	if len(models) > 0 {
		return models[0], nil
	}
	return llm.ModelInfo{}, fmt.Errorf("agent-loop: 没有任何模型")
}

func (a *Agent) toolSchemas(ctx context.Context) []llm.ToolSchema {
	list, err := a.tools.List(ctx)
	if err != nil {
		return nil
	}
	out := make([]llm.ToolSchema, 0, len(list))
	for _, t := range list {
		s := llm.ToolSchema{Name: t.Name, Description: t.Summary}
		if t.Input != nil {
			s.Input = mustJSON(t.Input.Raw())
		}
		out = append(out, s)
	}
	return out
}

type assistantPayload struct {
	Message   llm.Message    `json:"message"`
	ToolCalls []llm.ToolCall `json:"toolCalls,omitempty"`
	Usage     llm.Usage      `json:"usage,omitempty"`
}

type toolPayload struct {
	Call   tools.CallInfo `json:"call"`
	Result tools.Result   `json:"result"`
}

// render 把模型可见的会话事件还原成消息;控制面事件被跳过。
func (a *Agent) render(sess session.Session) []llm.Message {
	evs, err := sess.Events(0, 0)
	if err != nil {
		return nil
	}
	out := make([]llm.Message, 0, len(evs))
	boundary := a.compactedTo(sess)
	for _, ev := range evs {
		if ev.Seq <= boundary && ev.Type != session.TypeCompactSummary {
			continue
		}
		switch ev.Type {
		case session.TypeUserMessage:
			var m llm.Message
			if json.Unmarshal(ev.Data, &m) == nil {
				out = append(out, m)
			}
		case session.TypeAssistantMessage:
			var p assistantPayload
			if json.Unmarshal(ev.Data, &p) == nil {
				out = append(out, p.Message)
			}
		case session.TypeToolResult:
			var p toolPayload
			if json.Unmarshal(ev.Data, &p) != nil {
				continue
			}
			blocks := p.Result.Content
			if len(blocks) == 0 {
				blocks = []llm.Block{{Kind: llm.BlockText, Text: "工具失败: " + p.Call.Name}}
			}
			for i := range blocks {
				if blocks[i].Meta == nil {
					blocks[i].Meta = map[string]string{}
				}
				blocks[i].Meta["toolCallId"] = p.Call.ID
				blocks[i].Meta["toolName"] = p.Call.Name
			}
			out = append(out, llm.Message{Role: llm.RoleTool, Content: blocks})
		case session.TypeCompactSummary:
			var text string
			var v map[string]any
			if json.Unmarshal(ev.Data, &v) == nil {
				text, _ = v["summary"].(string)
			}
			if text != "" {
				out = append(out, llm.Message{Role: llm.RoleSystem, Content: []llm.Block{{Kind: llm.BlockText, Text: text}}})
			}
		}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Plugin 返回 agent-loop 插件:inject 四个 seam,提供 agent 服务。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*session.Registry](ctx, "session")
		if err != nil {
			return nil, err
		}
		pr, err := kernel.Service[*prompts.Engine](ctx, "prompts")
		if err != nil {
			return nil, err
		}
		lm, err := kernel.Service[*llm.Registry](ctx, "llm")
		if err != nil {
			return nil, err
		}
		tl, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "agent-loop: 解析配置失败: %v", err)
		}
		agent := &Agent{bus: ctx, sessions: reg, prompts: pr, llms: lm, tools: tl, conf: conf}
		if _, err := ctx.Provide("agent", agent, kernel.WithVersion(1), kernel.WithKind("object")); err != nil {
			return nil, err
		}
		_, err = ctx.Provide("agent.runner", seamRunner{Agent: agent}, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithInject("session", "prompts", "llm", "tools").WithProvide("agent", "agent.runner")
}
