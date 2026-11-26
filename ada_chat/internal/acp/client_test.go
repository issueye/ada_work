package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type fakeAgent struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
}

func newFake(t *testing.T) (*Client, *fakeAgent) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	_ = b.SetDeadline(time.Now().Add(5 * time.Second))
	client := New(b, b)
	t.Cleanup(func() { _ = client.Close() })
	return client, &fakeAgent{t: t, conn: a, br: bufio.NewReader(a)}
}

func (f *fakeAgent) read() wire {
	f.t.Helper()
	line, err := f.br.ReadBytes('\n')
	if err != nil {
		f.t.Fatalf("fake agent 读失败: %v", err)
	}
	var msg wire
	if err := json.Unmarshal(line, &msg); err != nil {
		f.t.Fatalf("fake agent 解析失败: %v (%s)", err, line)
	}
	return msg
}

func (f *fakeAgent) write(msg wire) {
	f.t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.conn.Write(append(b, '\n')); err != nil {
		f.t.Fatalf("fake agent 写失败: %v", err)
	}
}

func (f *fakeAgent) respond(id json.RawMessage, result any) {
	b, _ := json.Marshal(result)
	f.write(wire{JSONRPC: "2.0", ID: id, Result: b})
}

func (f *fakeAgent) notify(method string, params any) {
	b, _ := json.Marshal(params)
	f.write(wire{JSONRPC: "2.0", Method: method, Params: b})
}

func (f *fakeAgent) request(id, method string, params any) {
	b, _ := json.Marshal(params)
	f.write(wire{JSONRPC: "2.0", ID: json.RawMessage(`"` + id + `"`), Method: method, Params: b})
}

func TestInitializeNewSessionPrompt(t *testing.T) {
	client, agent := newFake(t)
	updates := make(chan string, 4)
	client.OnNotification(func(n Notification) {
		var p struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		_ = json.Unmarshal(n.Params, &p)
		updates <- p.Update.SessionUpdate + ":" + p.Update.Content.Text
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		req := agent.read()
		if req.Method != "initialize" {
			t.Errorf("第一个方法应为 initialize,实际 %s", req.Method)
		}
		agent.respond(req.ID, map[string]any{"protocolVersion": 1})
	}()
	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}

	go func() {
		req := agent.read()
		if req.Method != "session/new" {
			t.Errorf("应为 session/new,实际 %s", req.Method)
		}
		agent.respond(req.ID, map[string]any{"sessionId": "s1"})
	}()
	if got, err := client.NewSession(ctx, "/workspace"); err != nil || got != "s1" {
		t.Fatalf("session/new 失败: %q %v", got, err)
	}

	go func() {
		req := agent.read()
		if req.Method != "session/prompt" {
			t.Errorf("应为 session/prompt,实际 %s", req.Method)
		}
		if !strings.Contains(string(req.Params), `"text":"hi"`) {
			t.Errorf("prompt 参数不符: %s", req.Params)
		}
		agent.notify("session/update", map[string]any{
			"sessionId": "s1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": "hello"},
			},
		})
		agent.respond(req.ID, map[string]any{"stopReason": "end_turn"})
	}()
	stop, err := client.Prompt(ctx, "s1", "hi")
	if err != nil || stop != "end_turn" {
		t.Fatalf("prompt 失败: %q %v", stop, err)
	}
	select {
	case got := <-updates:
		if got != "agent_message_chunk:hello" {
			t.Fatalf("update 不符: %q", got)
		}
	case <-ctx.Done():
		t.Fatal("没有收到 session/update")
	}
}

func TestReversePermissionRequest(t *testing.T) {
	client, agent := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client.Handle("session/request_permission", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.SessionID != "s1" {
			t.Errorf("权限参数不符: %s", params)
		}
		return map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": "allow_once",
		}}, nil
	})

	go func() {
		req := agent.read()
		agent.request("perm-1", "session/request_permission", map[string]any{
			"sessionId": "s1",
			"toolCall":  map[string]any{"toolCallId": "call-1", "title": "写文件"},
			"options": []map[string]string{
				{"optionId": "allow_once", "kind": "allow_once", "name": "允许"},
				{"optionId": "reject_once", "kind": "reject_once", "name": "拒绝"},
			},
		})
		resp := agent.read()
		if string(resp.ID) != `"perm-1"` {
			t.Errorf("权限响应 id 不符: %s", resp.ID)
		}
		if !strings.Contains(string(resp.Result), `"optionId":"allow_once"`) {
			t.Errorf("权限响应结果不符: %s", resp.Result)
		}
		agent.respond(req.ID, map[string]any{"stopReason": "end_turn"})
	}()

	stop, err := client.Prompt(ctx, "s1", "hi")
	if err != nil || stop != "end_turn" {
		t.Fatalf("prompt 失败: %q %v", stop, err)
	}
}

func TestCancelNotification(t *testing.T) {
	client, agent := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		req := agent.read()
		if req.Method != "session/cancel" || req.ID != nil {
			t.Errorf("cancel 应是无 id 通知: %+v", req)
		}
	}()
	if err := client.Cancel(ctx, "s1"); err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}
}

func TestInitializeRejectsMismatchedVersion(t *testing.T) {
	client, agent := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		req := agent.read()
		agent.respond(req.ID, map[string]any{"protocolVersion": 2})
	}()
	if err := client.Initialize(ctx); err == nil {
		t.Fatal("协议版本不匹配应失败")
	}
}
