package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

func echoDef() tools.ToolDef {
	return tools.ToolDef{
		Name: "echo", Summary: "echo",
		Handler: func(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
			var a struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(call.Args, &a); err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: a.Text}}}, nil
		},
	}
}

// 注册、schema、restrict、guard 的最小契约。
func TestRegisterInvokeAndDeny(t *testing.T) {
	r := tools.NewRegistry(nil)
	def := echoDef()
	def.Input = schema.FromMap(map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
		"required":   []any{"text"},
	})
	if _, err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	call := tools.CallInfo{Name: "echo", Args: json.RawMessage(`{"text":"hi"}`)}
	res, err := r.Invoke(context.Background(), call)
	if err != nil || res.Status != tools.StatusOK || res.Content[0].Text != "hi" {
		t.Fatalf("调用失败: %+v %v", res, err)
	}
	res, _ = r.Invoke(context.Background(), tools.CallInfo{Name: "echo", Args: json.RawMessage(`{"text":1}`)})
	if res.Status != tools.StatusError {
		t.Fatalf("schema 应拒绝,实际 %+v", res)
	}

	dispose, _ := r.Restrict("echo")
	if list, _ := r.List(context.Background()); len(list) != 0 {
		t.Fatalf("restrict 后不应可见")
	}
	if res, _ := r.Invoke(context.Background(), tools.CallInfo{Name: "echo"}); res.Status != tools.StatusDenied {
		t.Fatalf("restrict 后应拒绝")
	}
	if err := dispose(context.Background()); err != nil {
		t.Fatal(err)
	}

	g, _ := r.Guard(func(context.Context, tools.CallInfo) error { return context.DeadlineExceeded })
	res, _ = r.Invoke(context.Background(), tools.CallInfo{Name: "echo"})
	if res.Status != tools.StatusDenied {
		t.Fatalf("guard 应拒绝,实际 %+v", res)
	}
	_ = g(context.Background())
}

// timeout 必须返回 StatusTimeout,而不是一直等 handler。
func TestTimeout(t *testing.T) {
	r := tools.NewRegistry(nil)
	def := echoDef()
	def.Timeout = 20 * time.Millisecond
	def.Handler = func(context.Context, tools.CallInfo) (tools.Result, error) {
		time.Sleep(80 * time.Millisecond)
		return tools.Result{Status: tools.StatusOK}, nil
	}
	if _, err := r.Register(def); err != nil {
		t.Fatal(err)
	}
	res, err := r.Invoke(context.Background(), tools.CallInfo{Name: "echo", Args: json.RawMessage(`{}`)})
	if err != nil || res.Status != tools.StatusTimeout {
		t.Fatalf("应超时: %+v %v", res, err)
	}
}
