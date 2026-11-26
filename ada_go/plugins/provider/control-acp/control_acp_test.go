package controlacp_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
)

type fakeControl struct {
	created int
	prompt  string
}

func (f *fakeControl) Invoke(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case control.MethodSessionCreate:
		f.created++
		return json.RawMessage(`{"id":"s1","title":"ACP"}`), nil
	case control.MethodSessionLoad:
		return json.RawMessage(`{"id":"s1","title":"ACP"}`), nil
	case control.MethodSessionList:
		return json.RawMessage(`[{"id":"s1"}]`), nil
	case control.MethodSessionPrompt:
		var req control.PromptRequest
		_ = json.Unmarshal(params, &req)
		f.prompt = req.Text
		return json.RawMessage(`{"id":"t1","stopReason":"end_turn"}`), nil
	case control.MethodSessionCancel:
		return json.RawMessage(`{}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func TestACPInitializeAndPrompt(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	notifications := make(chan jsonrpc2.Message, 1)
	client := jsonrpc2.New(ctx, a, jsonrpc2.WithNotificationHandler(func(m jsonrpc2.Message) {
		select {
		case notifications <- m:
		default:
		}
	}))
	server := jsonrpc2.New(ctx, b)
	fake := &fakeControl{}
	ep := controlacp.New(server, control.New(fake))
	t.Cleanup(func() { _ = ep.Close() })

	var init map[string]any
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, &init); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	if init["protocolVersion"] != float64(1) {
		t.Fatalf("initialize 结果不符: %+v", init)
	}
	var session map[string]string
	if err := client.Call(ctx, "session/new", map[string]any{"cwd": "/workspace"}, &session); err != nil {
		t.Fatalf("session/new 失败: %v", err)
	}
	if session["sessionId"] != "s1" {
		t.Fatalf("sessionId 不符: %+v", session)
	}
	var prompt map[string]string
	if err := client.Call(ctx, "session/prompt", map[string]any{
		"sessionId": "s1",
		"prompt":    []map[string]string{{"type": "text", "text": "hello"}},
	}, &prompt); err != nil {
		t.Fatalf("session/prompt 失败: %v", err)
	}
	if prompt["stopReason"] != "end_turn" || fake.prompt != "hello" {
		t.Fatalf("prompt 结果不符: %+v text=%q", prompt, fake.prompt)
	}

	updates := make(chan control.Update, 1)
	ep.SetUpdates(updates)
	updates <- control.Update{SessionID: "s1", Kind: "assistant/message", Data: json.RawMessage(`{"text":"hi"}`)}
	select {
	case msg := <-notifications:
		if msg.Method != "session/update" {
			t.Fatalf("通知方法不符: %s", msg.Method)
		}
		var params struct {
			SessionID string         `json:"sessionId"`
			Update    map[string]any `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("通知参数非法: %v", err)
		}
		if params.SessionID != "s1" || params.Update["sessionUpdate"] != "agent_message_chunk" {
			t.Fatalf("session/update 不符: %+v", params)
		}
		content, _ := params.Update["content"].(map[string]any)
		if content["text"] != "hi" {
			t.Fatalf("update 文本不符: %+v", content)
		}
	case <-ctx.Done():
		t.Fatal("没有收到 session/update")
	}
}
