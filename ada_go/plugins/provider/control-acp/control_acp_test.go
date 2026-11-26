package controlacp_test

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
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
	case control.MethodSessionHistory:
		return json.RawMessage(`[
			{"sessionId":"s1","kind":"user/message","data":{"role":"user","content":[{"kind":"text","text":"问题"}]}},
			{"sessionId":"s1","kind":"turn/start","data":{}},
			{"sessionId":"s1","kind":"assistant/message","data":{"message":{"content":[{"kind":"text","text":"回答"}]}}}
		]`), nil
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

	// agent-loop 的真实载荷是 {"message":{"content":[...]}};不能把整段 JSON 当成文本。
	updates <- control.Update{
		SessionID: "s1", Kind: "assistant/message",
		Data: json.RawMessage(`{"message":{"role":"assistant","content":[{"kind":"text","text":"你好"}]}}`),
	}
	select {
	case msg := <-notifications:
		var params struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("通知参数非法: %v", err)
		}
		if params.Update.Content.Text != "你好" {
			t.Fatalf("assistant 载荷文本不符: %q", params.Update.Content.Text)
		}
	case <-ctx.Done():
		t.Fatal("没有收到第二条 session/update")
	}
}

// acpClient 创建一个测试用的 ACP 客户端/服务端对。
func acpClient(t *testing.T, fake *fakeControl) (*jsonrpc2.Conn, *controlacp.Endpoint, context.Context) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	client := jsonrpc2.New(ctx, a)
	server := jsonrpc2.New(ctx, b)
	ep := controlacp.New(server, control.New(fake))
	t.Cleanup(func() { _ = ep.Close() })
	return client, ep, ctx
}

func TestACPRequestPermission(t *testing.T) {
	client, ep, ctx := acpClient(t, &fakeControl{})
	seen := make(chan map[string]any, 1)
	client.Handle("session/request_permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var in map[string]any
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		seen <- in
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}}, nil
	})

	dec, err := ep.RequestPermission(ctx, control.PermissionRequest{
		SessionID: "s1", Tool: "fs.write", ToolCallID: "call-1", Title: "写入文件",
	})
	if err != nil {
		t.Fatalf("request_permission 失败: %v", err)
	}
	if dec.Cancelled || dec.OptionID != control.OptionAllowOnce {
		t.Fatalf("权限决定不符: %+v", dec)
	}
	select {
	case in := <-seen:
		if in["sessionId"] != "s1" {
			t.Fatalf("sessionId 不符: %+v", in)
		}
		toolCall, _ := in["toolCall"].(map[string]any)
		if toolCall["toolCallId"] != "call-1" || toolCall["title"] != "写入文件" {
			t.Fatalf("toolCall 不符: %+v", toolCall)
		}
		options, _ := in["options"].([]any)
		if len(options) != 2 {
			t.Fatalf("默认选项数量不符: %+v", options)
		}
		first, _ := options[0].(map[string]any)
		if first["optionId"] != control.OptionAllowOnce || first["kind"] != control.OptionAllowOnce {
			t.Fatalf("选项映射不符: %+v", first)
		}
	case <-ctx.Done():
		t.Fatal("client 没有收到权限请求")
	}
}

func TestACPRequestPermissionCancelled(t *testing.T) {
	client, ep, ctx := acpClient(t, &fakeControl{})
	client.Handle("session/request_permission", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	})
	dec, err := ep.RequestPermission(ctx, control.PermissionRequest{SessionID: "s1", Tool: "shell.run"})
	if err != nil {
		t.Fatalf("request_permission 失败: %v", err)
	}
	if !dec.Cancelled {
		t.Fatalf("取消应映射成 Cancelled: %+v", dec)
	}
}

func TestACPBindApproval(t *testing.T) {
	client, ep, ctx := acpClient(t, &fakeControl{})
	mode := control.OptionAllowOnce
	var mu sync.Mutex
	client.Handle("session/request_permission", func(_ context.Context, _ json.RawMessage) (any, error) {
		mu.Lock()
		m := mode
		mu.Unlock()
		return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": m}}, nil
	})
	setMode := func(m string) {
		mu.Lock()
		mode = m
		mu.Unlock()
	}

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(approval.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 approval 失败: %v", err)
	}
	reg, err := kernel.Service[*approval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatal(err)
	}
	dispose, err := ep.BindApproval(app.Root())
	if err != nil {
		t.Fatalf("BindApproval 失败: %v", err)
	}
	t.Cleanup(func() { _ = dispose(context.Background()) })
	if names := reg.Names(); len(names) != 1 || names[0] != controlacp.ApproverName {
		t.Fatalf("审批人注册不符: %+v", names)
	}

	dec, err := approval.Ask(ctx, reg, controlacp.ApproverName, approval.Request{
		Source: "control", Op: "fs.write", Target: "写入文件",
		Details: map[string]any{"sessionId": "s1", "toolCallId": "call-1"},
	})
	if err != nil || !dec.Allow || dec.Approver != controlacp.ApproverName {
		t.Fatalf("允许路径不符: %+v err=%v", dec, err)
	}

	setMode(control.OptionRejectOnce)
	dec, err = approval.Ask(ctx, reg, controlacp.ApproverName, approval.Request{
		Source: "control", Op: "fs.write", Target: "写入文件",
		Details: map[string]any{"sessionId": "s1"},
	})
	if err != nil || dec.Allow {
		t.Fatalf("拒绝路径不符: %+v err=%v", dec, err)
	}

	setMode("unknown-option")
	dec, err = approval.Ask(ctx, reg, controlacp.ApproverName, approval.Request{
		Source: "control", Op: "fs.write", Target: "写入文件",
		Details: map[string]any{"sessionId": "s1"},
	})
	if err != nil || dec.Allow {
		t.Fatalf("未知选项应 fail-closed: %+v err=%v", dec, err)
	}
}

func waitUpdate(t *testing.T, ch <-chan jsonrpc2.Message, ctx context.Context) map[string]any {
	t.Helper()
	select {
	case msg := <-ch:
		if msg.Method != "session/update" {
			t.Fatalf("通知方法不符: %s", msg.Method)
		}
		var params struct {
			Update map[string]any `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			t.Fatalf("通知参数非法: %v", err)
		}
		return params.Update
	case <-ctx.Done():
		t.Fatal("没有收到 session/update")
		return nil
	}
}

func TestACPToolUpdates(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan jsonrpc2.Message, 1)
	client := jsonrpc2.New(ctx, a, jsonrpc2.WithNotificationHandler(func(m jsonrpc2.Message) {
		select {
		case notifications <- m:
		default:
		}
	}))
	t.Cleanup(func() { _ = client.Close() })
	server := jsonrpc2.New(ctx, b)
	ep := controlacp.New(server, control.New(&fakeControl{}))
	t.Cleanup(func() { _ = ep.Close() })
	updates := make(chan control.Update, 1)
	ep.SetUpdates(updates)

	updates <- control.Update{
		SessionID: "s1", Kind: "tool/call",
		Data: json.RawMessage(`{"id":"call-1","name":"read_file","args":{"path":"a.txt"}}`),
	}
	call := waitUpdate(t, notifications, ctx)
	if call["sessionUpdate"] != "tool_call" || call["toolCallId"] != "call-1" ||
		call["kind"] != "read" || call["status"] != "in_progress" {
		t.Fatalf("tool_call 映射不符: %+v", call)
	}
	if raw, ok := call["rawInput"].(map[string]any); !ok || raw["path"] != "a.txt" {
		t.Fatalf("tool_call rawInput 不符: %+v", call["rawInput"])
	}

	updates <- control.Update{
		SessionID: "s1", Kind: "tool/result",
		Data: json.RawMessage(`{"call":{"id":"call-1","name":"read_file"},"result":{"content":[{"kind":"text","text":"hello"}],"status":"ok"}}`),
	}
	done := waitUpdate(t, notifications, ctx)
	if done["sessionUpdate"] != "tool_call_update" || done["toolCallId"] != "call-1" || done["status"] != "completed" {
		t.Fatalf("tool_call_update 映射不符: %+v", done)
	}
	content, _ := done["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("工具结果内容数量不符: %+v", content)
	}
	first, _ := content[0].(map[string]any)
	inner, _ := first["content"].(map[string]any)
	if first["type"] != "content" || inner["text"] != "hello" {
		t.Fatalf("工具结果内容不符: %+v", first)
	}

	updates <- control.Update{
		SessionID: "s1", Kind: "tool/result",
		Data: json.RawMessage(`{"call":{"id":"call-1"},"result":{"status":"timeout","content":[{"kind":"text","text":"超时"}]}}`),
	}
	failed := waitUpdate(t, notifications, ctx)
	if failed["status"] != "failed" {
		t.Fatalf("失败状态应映射为 failed: %+v", failed)
	}
}

func TestACPLoadReplaysHistory(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan jsonrpc2.Message, 8)
	client := jsonrpc2.New(ctx, a, jsonrpc2.WithNotificationHandler(func(m jsonrpc2.Message) {
		notifications <- m
	}))
	t.Cleanup(func() { _ = client.Close() })
	server := jsonrpc2.New(ctx, b)
	ep := controlacp.New(server, control.New(&fakeControl{}))
	t.Cleanup(func() { _ = ep.Close() })

	var res any
	if err := client.Call(ctx, "session/load", map[string]any{"sessionId": "s1"}, &res); err != nil {
		t.Fatalf("session/load 失败: %v", err)
	}
	if res != nil {
		t.Fatalf("session/load 应返回 null: %+v", res)
	}
	var kinds, texts []string
	for i := 0; i < 2; i++ {
		msg := <-notifications
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			t.Fatalf("通知参数非法: %v", err)
		}
		kinds = append(kinds, p.Update.SessionUpdate)
		texts = append(texts, p.Update.Content.Text)
	}
	if kinds[0] != "user_message_chunk" || texts[0] != "问题" {
		t.Fatalf("user 回放不符: %v %v", kinds, texts)
	}
	if kinds[1] != "agent_message_chunk" || texts[1] != "回答" {
		t.Fatalf("assistant 回放不符: %v %v", kinds, texts)
	}
	if len(notifications) != 0 {
		t.Fatalf("控制面事件不应回放,多出 %d 条", len(notifications))
	}
}
