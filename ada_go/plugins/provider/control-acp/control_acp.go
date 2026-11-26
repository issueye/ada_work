// Package controlacp 是 control 的最小 ACP 适配器。
//
// 它只做方法翻译,不实现 agent:
//   - initialize
//   - session/new
//   - session/load
//   - session/prompt
//   - session/cancel
//
// 流式 session/update 可通过 SetUpdates 注入;文件/终端回调与
// request_permission 仍待接。
package controlacp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
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
	ProtocolVersion int `json:"protocolVersion,omitempty"`
}

func (e *Endpoint) initialize(_ context.Context, _ json.RawMessage) (any, error) {
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
	info, err := e.ctl.Load(ctx, p.SessionID)
	if err != nil {
		return nil, rpcError(err)
	}
	return map[string]any{"sessionId": info.ID}, nil
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
			if err := e.conn.Notify(context.Background(), "session/update", sessionUpdateParams(u)); err != nil {
				return
			}
		}
	}
}

func sessionUpdateParams(u control.Update) map[string]any {
	update := map[string]any{"sessionUpdate": u.Kind}
	switch u.Kind {
	case "assistant/message", "assistant_message":
		update["sessionUpdate"] = "agent_message_chunk"
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(u.Data, &p)
		if p.Text == "" {
			p.Text = string(u.Data)
		}
		update["content"] = map[string]any{"type": "text", "text": p.Text}
	default:
		if len(u.Data) > 0 {
			update["data"] = u.Data
		}
	}
	return map[string]any{"sessionId": u.SessionID, "update": update}
}

func rpcError(err error) *jsonrpc2.Error {
	if err == nil {
		return nil
	}
	return &jsonrpc2.Error{Code: jsonrpc2.CodeInternalError, Message: err.Error()}
}
