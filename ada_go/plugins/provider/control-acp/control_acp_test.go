package controlacp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	approvalgate "github.com/issueye/ada_go_v2/plugins/policy/approval-gate"
	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

type fakeControl struct {
	created  int
	prompt   string
	create   control.CreateRequest
	loadID   string
	loadCwd  string
	closed   []string
	onPrompt func()
}

func (f *fakeControl) Invoke(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case control.MethodSessionCreate:
		f.created++
		_ = json.Unmarshal(params, &f.create)
		return json.RawMessage(`{"id":"s1","title":"ACP"}`), nil
	case control.MethodSessionLoad:
		var req struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd,omitempty"`
		}
		_ = json.Unmarshal(params, &req)
		f.loadID, f.loadCwd = req.SessionID, req.Cwd
		return json.RawMessage(`{"id":"s1","title":"ACP"}`), nil
	case control.MethodSessionList:
		return json.RawMessage(`[{"id":"s1"}]`), nil
	case control.MethodSessionPrompt:
		var req control.PromptRequest
		_ = json.Unmarshal(params, &req)
		f.prompt = req.Text
		if f.onPrompt != nil {
			f.onPrompt()
		}
		return json.RawMessage(`{"id":"t1","stopReason":"end_turn"}`), nil
	case control.MethodSessionHistory:
		return json.RawMessage(`[
			{"sessionId":"s1","kind":"user/message","data":{"role":"user","content":[{"kind":"text","text":"问题"}]}},
			{"sessionId":"s1","kind":"turn/start","data":{}},
			{"sessionId":"s1","kind":"assistant/message","data":{"message":{"content":[{"kind":"text","text":"回答"}]}}}
		]`), nil
	case control.MethodSessionCancel:
		return json.RawMessage(`{}`), nil
	case control.MethodSessionClose:
		var req struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &req)
		f.closed = append(f.closed, req.SessionID)
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
	if fake.create.Cwd != "/workspace" {
		t.Fatalf("session/new 应把 cwd 传给控制层,实际 %+v", fake.create)
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
	fake := &fakeControl{}
	ep := controlacp.New(server, control.New(fake))
	t.Cleanup(func() { _ = ep.Close() })

	var res any
	if err := client.Call(ctx, "session/load", map[string]any{
		"sessionId": "s1", "cwd": "/workspace",
	}, &res); err != nil {
		t.Fatalf("session/load 失败: %v", err)
	}
	if fake.loadID != "s1" || fake.loadCwd != "/workspace" {
		t.Fatalf("session/load 应把 cwd 传给控制层: %+v", fake)
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

// TestACPApproverUsesContextSession 覆盖 approval.Request 没带 Details 的真实路径:
// fs/shell/gate 由 ctx 里的会话兜底,ACP 仍能把 sessionId 发给 client。
func TestACPApproverUsesContextSession(t *testing.T) {
	client, ep, ctx := acpClient(t, &fakeControl{})
	seen := make(chan string, 1)
	client.Handle("session/request_permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		seen <- in.SessionID
		return map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": control.OptionAllowOnce,
		}}, nil
	})

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("%s 未就绪: %v", p.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(approval.Plugin())

	sessions, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Open(ctx, "", session.Meta{Title: "权限"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.Service[*approval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatal(err)
	}
	dispose, err := ep.BindApproval(app.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispose(context.Background()) })

	dec, err := approval.Ask(session.WithSession(ctx, sess), reg, controlacp.ApproverName, approval.Request{
		Source: "fs", Op: "fs.write", Target: "a.txt", Reason: "测试",
	})
	if err != nil || !dec.Allow {
		t.Fatalf("应放行: %+v err=%v", dec, err)
	}
	select {
	case got := <-seen:
		if got != sess.ID() {
			t.Fatalf("sessionId 应为 %q,实际 %q", sess.ID(), got)
		}
	case <-ctx.Done():
		t.Fatal("client 没有收到权限请求")
	}
}

// recordingConn 记录服务端写出的字节,用于断言更新与响应的顺序。
type recordingConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingConn) Write(p []byte) (int, error) {
	r.mu.Lock()
	_, _ = r.buf.Write(p)
	r.mu.Unlock()
	return r.Conn.Write(p)
}

func (r *recordingConn) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw := strings.Split(r.buf.String(), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestACPPromptResponseWaitsForUpdates(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := jsonrpc2.New(ctx, a)
	t.Cleanup(func() { _ = client.Close() })
	rec := &recordingConn{Conn: b}
	server := jsonrpc2.New(ctx, rec)
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	fake := &fakeControl{}
	fake.onPrompt = func() {
		app.Root().EmitContext(ctx, control.EventUpdate, control.Update{
			SessionID: "s1", Kind: "assistant/delta", Data: json.RawMessage(`{"text":"最后一段"}`),
		})
	}
	ep := controlacp.New(server, control.New(fake))
	t.Cleanup(func() { _ = ep.Close() })
	dispose := ep.BindUpdates(app.Root())
	t.Cleanup(func() { _ = dispose(context.Background()) })

	var res map[string]string
	if err := client.Call(ctx, "session/prompt", map[string]any{
		"sessionId": "s1",
		"prompt":    []map[string]string{{"type": "text", "text": "hi"}},
	}, &res); err != nil {
		t.Fatalf("session/prompt 失败: %v", err)
	}

	updateIdx, responseIdx := -1, -1
	for i, line := range rec.lines() {
		if strings.Contains(line, `"method":"session/update"`) {
			updateIdx = i
		}
		if strings.Contains(line, `"id":"c-1"`) && strings.Contains(line, `"result"`) {
			responseIdx = i
		}
	}
	if updateIdx < 0 {
		t.Fatalf("没有写出 session/update: %+v", rec.lines())
	}
	if responseIdx < 0 {
		t.Fatalf("没有写出 prompt 响应: %+v", rec.lines())
	}
	if updateIdx > responseIdx {
		t.Fatalf("session/update 应早于 prompt 响应: update=%d response=%d", updateIdx, responseIdx)
	}
}

// TestACPUpdateFailureKeepsStream 覆盖单条更新超限:后续更新仍应到达。
func TestACPUpdateFailureKeepsStream(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	notifications := make(chan jsonrpc2.Message, 2)
	client := jsonrpc2.New(ctx, a, jsonrpc2.WithNotificationHandler(func(m jsonrpc2.Message) {
		notifications <- m
	}))
	t.Cleanup(func() { _ = client.Close() })
	server := jsonrpc2.New(ctx, b, jsonrpc2.WithMaxLine(256))
	ep := controlacp.New(server, control.New(&fakeControl{}))
	t.Cleanup(func() { _ = ep.Close() })
	updates := make(chan control.Update, 2)
	ep.SetUpdates(updates)

	updates <- control.Update{
		SessionID: "s1", Kind: "assistant/delta",
		Data: json.RawMessage(`{"text":"` + strings.Repeat("x", 1024) + `"}`),
	}
	updates <- control.Update{
		SessionID: "s1", Kind: "assistant/delta", Data: json.RawMessage(`{"text":"ok"}`),
	}
	select {
	case msg := <-notifications:
		var p struct {
			Update struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			t.Fatalf("通知参数非法: %v", err)
		}
		if p.Update.Content.Text != "ok" {
			t.Fatalf("应跳过超限更新并继续发后续更新,实际 %q", p.Update.Content.Text)
		}
	case <-ctx.Done():
		t.Fatal("超限更新之后的事件流被终止")
	}
	if _, failed := ep.Stats(); failed == 0 {
		t.Fatal("超限更新应计入失败计数")
	}
}

func TestACPRejectsMCPServers(t *testing.T) {
	client, _, ctx := acpClient(t, &fakeControl{})
	var out map[string]any
	err := client.Call(ctx, "session/new", map[string]any{
		"cwd":        "/workspace",
		"mcpServers": []map[string]any{{"name": "demo"}},
	}, &out)
	if err == nil {
		t.Fatal("未实现 MCP 时应显式拒绝")
	}
	var rerr *jsonrpc2.Error
	if !errors.As(err, &rerr) || !strings.Contains(rerr.Message, "mcpServers") {
		t.Fatalf("错误应说明 mcpServers 不支持: %v", err)
	}
}

func TestACPInitializeRejectsUnsupportedVersion(t *testing.T) {
	client, _, ctx := acpClient(t, &fakeControl{})
	var out map[string]any
	err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 2}, &out)
	if err == nil {
		t.Fatal("不支持的协议版本应显式拒绝")
	}
	var rerr *jsonrpc2.Error
	if !errors.As(err, &rerr) || rerr.Code != jsonrpc2.CodeInvalidParams {
		t.Fatalf("应返回 InvalidParams: %v", err)
	}
}

func TestACPCloseRecyclesSessions(t *testing.T) {
	fake := &fakeControl{}
	client, ep, ctx := acpClient(t, fake)
	var session map[string]string
	if err := client.Call(ctx, "session/new", map[string]any{"cwd": "/workspace"}, &session); err != nil {
		t.Fatalf("session/new 失败: %v", err)
	}
	if err := ep.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if len(fake.closed) != 1 || fake.closed[0] != session["sessionId"] {
		t.Fatalf("端点关闭应回收会话,实际 %+v", fake.closed)
	}
}

// TestACPPermissionThroughApprovalGate 覆盖真实链路:
// tools/pre-execute(gate) → approval.Ask → ACP approver → session/request_permission。
func TestACPPermissionThroughApprovalGate(t *testing.T) {
	client, ep, ctx := acpClient(t, &fakeControl{})
	seen := make(chan string, 1)
	client.Handle("session/request_permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		seen <- in.SessionID
		return map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": control.OptionAllowOnce,
		}}, nil
	})

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin, cfg kernel.Config) {
		t.Helper()
		f, err := app.Plugin(p, cfg)
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("%s 未就绪: %v", p.Name(), err)
		}
	}
	mount(tools.Plugin(), kernel.NewConfig(nil))
	mount(approval.Plugin(), kernel.NewConfig(nil))
	mount(session.Plugin(), kernel.NewConfig(nil))
	mount(memory.Plugin(), kernel.NewConfig(nil))
	mount(approvalgate.Plugin(), kernel.NewConfig(map[string]any{"tools": []string{"danger"}}))

	toolsReg, err := kernel.Service[*tools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	var called int32
	if _, err := toolsReg.Register(tools.ToolDef{
		Name: "danger",
		Handler: func(context.Context, tools.CallInfo) (tools.Result, error) {
			atomic.AddInt32(&called, 1)
			return tools.Result{Status: tools.StatusOK}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	dispose, err := ep.BindApproval(app.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispose(context.Background()) })

	sessions, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Open(ctx, "", session.Meta{Title: "审批"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := toolsReg.Invoke(session.WithSession(ctx, sess), tools.CallInfo{
		ID: "call-1", Name: "danger", SessionID: sess.ID(),
	})
	if err != nil || res.Status != tools.StatusOK || atomic.LoadInt32(&called) != 1 {
		t.Fatalf("审批放行后应执行: %+v err=%v called=%d", res, err, called)
	}
	select {
	case got := <-seen:
		if got != sess.ID() {
			t.Fatalf("sessionId 应为 %q,实际 %q", sess.ID(), got)
		}
	case <-ctx.Done():
		t.Fatal("ACP client 没有收到权限请求")
	}
}
