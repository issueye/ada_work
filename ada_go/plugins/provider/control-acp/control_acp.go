// Package controlacp 是 control 的最小 ACP 适配器。
//
// 它只做方法翻译,不实现 agent:
//   - initialize
//   - session/new
//   - session/load
//   - session/prompt
//   - session/cancel
//
// 流式 session/update 可通过 SetUpdates 注入;权限请求通过 Approver/BindApproval
// 接到 approval(见 permission.go);文件/终端回调仍待接。
package controlacp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProtocolVersion 是当前实现的 ACP 协议版本。
const ProtocolVersion = 1

// Endpoint 是一个 ACP agent 端点。
type Endpoint struct {
	conn *jsonrpc2.Conn
	ctl  *control.Client

	mu      sync.Mutex
	updates <-chan control.Update
	stop    chan struct{}

	capMu sync.RWMutex
	caps  acpseam.Capabilities
}

// New 在 conn 上注册 ACP 方法。
func New(conn *jsonrpc2.Conn, ctl *control.Client) *Endpoint {
	e := &Endpoint{conn: conn, ctl: ctl, stop: make(chan struct{})}
	conn.Handle("initialize", e.initialize)
	conn.Handle("session/new", e.newSession)
	conn.Handle("session/load", e.loadSession)
	conn.Handle("session/prompt", e.prompt)
	conn.Handle("session/cancel", e.cancel)
	return e
}

// Close 关闭底层连接。
func (e *Endpoint) Close() error {
	if e == nil || e.conn == nil {
		return nil
	}
	select {
	case <-e.stop:
	default:
		close(e.stop)
	}
	return e.conn.Close()
}

type initializeParams struct {
	ProtocolVersion    int                 `json:"protocolVersion,omitempty"`
	ClientCapabilities *clientCapabilities `json:"clientCapabilities,omitempty"`
}

// clientCapabilities 是 initialize 里 client 声明的反向能力。
type clientCapabilities struct {
	FS struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal"`
}

func (e *Endpoint) initialize(_ context.Context, raw json.RawMessage) (any, error) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "initialize 参数非法"}
		}
	}
	e.setCapabilities(p.ClientCapabilities)
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image": false, "audio": false, "embeddedContext": false,
			},
		},
	}, nil
}

type newSessionParams struct {
	Cwd        string `json:"cwd,omitempty"`
	MCPServers []any  `json:"mcpServers,omitempty"`
}

func (e *Endpoint) newSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var p newSessionParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/new 参数非法"}
		}
	}
	info, err := e.ctl.Create(ctx, control.CreateRequest{Title: "ACP", Meta: map[string]any{"cwd": p.Cwd}})
	if err != nil {
		return nil, rpcError(err)
	}
	return map[string]any{"sessionId": info.ID}, nil
}

func (e *Endpoint) loadSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/load 需要 sessionId"}
	}
	if _, err := e.ctl.Load(ctx, p.SessionID); err != nil {
		return nil, rpcError(err)
	}
	// 先回放历史,再返回 null(ACP session/load 的结果就是 null)。
	history, err := e.ctl.History(ctx, p.SessionID)
	if err != nil && !seamerr.Is(err, seamerr.ErrUnimplemented) {
		return nil, rpcError(err)
	}
	for _, u := range history {
		params := replayUpdateParams(u)
		if params == nil {
			continue
		}
		if err := e.conn.Notify(ctx, "session/update", params); err != nil {
			return nil, rpcError(err)
		}
	}
	return nil, nil
}

type promptParams struct {
	SessionID string `json:"sessionId"`
	Prompt    []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"prompt"`
}

func (e *Endpoint) prompt(ctx context.Context, raw json.RawMessage) (any, error) {
	var p promptParams
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/prompt 参数非法"}
	}
	text := ""
	for _, part := range p.Prompt {
		if part.Type == "text" {
			text += part.Text
		}
	}
	if text == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/prompt 需要 text"}
	}
	res, err := e.ctl.Prompt(ctx, control.PromptRequest{SessionID: p.SessionID, Text: text})
	if err != nil {
		return nil, rpcError(err)
	}
	reason := res.StopReason
	if reason == "" {
		reason = "end_turn"
	}
	return map[string]any{"stopReason": reason}, nil
}

func (e *Endpoint) cancel(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, nil
	}
	_ = e.ctl.Cancel(ctx, p.SessionID)
	return nil, nil
}

// SetUpdates 注入 control/update 源;只能设置一次,重复调用忽略。
//
// 宿主应把 core/agent-control 发出的 control/update 事件转发到这个 channel。
func (e *Endpoint) SetUpdates(ch <-chan control.Update) {
	e.mu.Lock()
	if e.updates != nil {
		e.mu.Unlock()
		return
	}
	e.updates = ch
	e.mu.Unlock()
	go e.forwardUpdates(ch)
}

// BindUpdates 订阅 kernel 的 control/update 事件并转发成 ACP session/update。
// Go 与 TS 控制实现都通过同一个事件名,因此 ACP 不关心控制层是哪种语言实现。
func (e *Endpoint) BindUpdates(ctx *kernel.Context) kernel.Dispose {
	ch := make(chan control.Update, 128)
	dispose := ctx.On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				select {
				case ch <- u:
				default:
				}
			}
		}
		return nil, nil
	})
	e.SetUpdates(ch)
	return func(c context.Context) error {
		_ = dispose(c)
		close(ch)
		return nil
	}
}

func (e *Endpoint) forwardUpdates(ch <-chan control.Update) {
	for {
		select {
		case <-e.stop:
			return
		case u, ok := <-ch:
			if !ok {
				return
			}
			params := sessionUpdateParams(u)
			if params == nil {
				continue
			}
			if err := e.conn.Notify(context.Background(), "session/update", params); err != nil {
				return
			}
		}
	}
}

// controlPlaneKinds 没有 ACP 等价物;实时转发直接跳过,避免非法 sessionUpdate。
var controlPlaneKinds = map[string]bool{
	"turn/start": true, "turn/end": true,
	"step/start": true, "step/end": true,
	"user/message": true, "user_message": true,
	"compact/summary": true, "error": true,
}

func sessionUpdateParams(u control.Update) map[string]any {
	if controlPlaneKinds[u.Kind] {
		return nil
	}
	update := map[string]any{"sessionUpdate": u.Kind}
	switch u.Kind {
	case "assistant/message", "assistant_message", "assistant/delta", "assistant_delta":
		text, recognized := messageText(u.Data)
		if !recognized {
			// 未知形状:原样透出,至少不丢内容。
			text = string(u.Data)
		}
		if text == "" {
			// 纯工具调用的 assistant 消息没有文本,不产生空 chunk。
			return nil
		}
		update["sessionUpdate"] = "agent_message_chunk"
		update["content"] = map[string]any{"type": "text", "text": text}
	case "tool/call", "tool_call":
		if mapped := toolCallUpdate(u.Data); mapped != nil {
			update = mapped
		} else if len(u.Data) > 0 {
			update["data"] = u.Data
		}
	case "tool/result", "tool_result":
		if mapped := toolResultUpdate(u.Data); mapped != nil {
			update = mapped
		} else if len(u.Data) > 0 {
			update["data"] = u.Data
		}
	default:
		if len(u.Data) > 0 {
			update["data"] = u.Data
		}
	}
	return map[string]any{"sessionId": u.SessionID, "update": update}
}

// replayUpdateParams 用于 session/load 回放:把 user/message 还原成
// user_message_chunk;实时转发不回显用户消息。
func replayUpdateParams(u control.Update) map[string]any {
	switch u.Kind {
	case "user/message", "user_message":
		text, recognized := messageText(u.Data)
		if !recognized || text == "" {
			return nil
		}
		return map[string]any{"sessionId": u.SessionID, "update": map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		}}
	}
	return sessionUpdateParams(u)
}

// toolCallUpdate 把 tool/call 映射成 ACP tool_call;CallInfo 的 id 是关联键,
// 缺失时不做映射(避免产生无法收尾的 UI 条目)。
func toolCallUpdate(raw json.RawMessage) map[string]any {
	var info struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.ID == "" {
		return nil
	}
	title := info.Name
	if title == "" {
		title = info.ID
	}
	out := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    info.ID,
		"title":         title,
		"kind":          toolKind(info.Name),
		"status":        "in_progress",
	}
	if len(info.Args) > 0 {
		out["rawInput"] = info.Args
	}
	return out
}

// toolResultUpdate 把 tool/result 映射成 ACP tool_call_update:只把文本内容
// 交给 ACP,原始 details 放进 rawOutput,避免私有字段混进 content。
func toolResultUpdate(raw json.RawMessage) map[string]any {
	var payload struct {
		Call struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"call"`
		Result struct {
			Content []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"content"`
			Status  string          `json:"status"`
			Details json.RawMessage `json:"details"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Call.ID == "" {
		return nil
	}
	out := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    payload.Call.ID,
		"status":        toolStatus(payload.Result.Status),
	}
	var b strings.Builder
	for _, block := range payload.Result.Content {
		if block.Kind == "" || block.Kind == "text" {
			b.WriteString(block.Text)
		}
	}
	if b.Len() > 0 {
		out["content"] = []map[string]any{{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": b.String()},
		}}
	}
	if len(payload.Result.Details) > 0 {
		out["rawOutput"] = payload.Result.Details
	}
	return out
}

// toolKind 把工具名映射成 ACP 的 ToolKind;只做粗分类,未知一律 other。
func toolKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "search"), strings.Contains(n, "grep"), strings.Contains(n, "find"):
		return "search"
	case strings.Contains(n, "read"), strings.Contains(n, "list"), strings.Contains(n, "view"):
		return "read"
	case strings.Contains(n, "delete"), strings.Contains(n, "remove"):
		return "delete"
	case strings.Contains(n, "move"), strings.Contains(n, "rename"):
		return "move"
	case strings.Contains(n, "write"), strings.Contains(n, "edit"),
		strings.Contains(n, "patch"), strings.Contains(n, "replace"):
		return "edit"
	case strings.Contains(n, "shell"), strings.Contains(n, "exec"),
		strings.Contains(n, "command"), strings.Contains(n, "run"):
		return "execute"
	case strings.Contains(n, "http"), strings.Contains(n, "fetch"), strings.Contains(n, "web"):
		return "fetch"
	}
	return "other"
}

// toolStatus 把 tools.Status 映射成 ACP 的 ToolCallStatus。
// 未知状态按失败处理:宁可让 UI 显示失败,也不假装成功。
func toolStatus(s string) string {
	switch s {
	case "ok", "":
		return "completed"
	}
	return "failed"
}

// textBlock 是 assistant/user 消息里的一小块内容。
type textBlock struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// messageText 从 assistant/user 事件里取文本,兼容 {"message":{...}}、
// 直接 llm.Message 与 {"text":...} 三种形状。第二个返回值表示「认得形状」:
// 空消息返回 ("", true),未知 JSON 返回 false(调用方回退原文)。
func messageText(raw json.RawMessage) (string, bool) {
	var envelope struct {
		Message json.RawMessage `json:"message"`
		Content []textBlock     `json:"content"`
		Text    *string         `json:"text"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false
	}
	blocks := envelope.Content
	recognized := envelope.Message != nil || envelope.Content != nil || envelope.Text != nil
	if envelope.Message != nil {
		var msg struct {
			Content []textBlock `json:"content"`
		}
		if err := json.Unmarshal(envelope.Message, &msg); err == nil {
			blocks = msg.Content
		}
	}
	var b strings.Builder
	for _, c := range blocks {
		if c.Kind == "" || c.Kind == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() > 0 {
		return b.String(), true
	}
	if envelope.Text != nil {
		return *envelope.Text, true
	}
	return "", recognized
}

func rpcError(err error) *jsonrpc2.Error {
	if err == nil {
		return nil
	}
	return &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: err.Error()}
}
