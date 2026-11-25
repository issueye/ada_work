package bridge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// TestJSMemoryAutoRecall 验证 autoRecall:TS 插件在 llm/request 上注册 waterfall,
// 按最后一条用户消息召回记忆,并把新增 system 消息还原成 *llm.Request 交给 agent-loop。
func TestJSMemoryAutoRecall(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})

	manifest, err := plugin.Load("../../plugins/tool/memory-helper")
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}
	// loader 的默认配置是 nil;这里显式打开 autoRecall 并给它一条预算。
	f, err := app.Plugin(p, kernel.NewConfig(map[string]any{
		"autoRecall": true, "recallLimit": 3, "recallMaxChars": 400,
	}))
	if err != nil {
		t.Fatalf("挂载插件失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("插件未就绪: %v", err)
	}

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)
	data, _ := json.Marshal(map[string]any{
		"text":       "用户偏好中文回答,代码注释也使用中文。",
		"tags":       []string{"preference"},
		"scope":      "user",
		"importance": 4,
	})
	if res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "memory_write", Args: data}); err != nil || res.Status != tools.StatusOK {
		t.Fatalf("写入记忆失败: %v %+v", err, res)
	}

	request := func(userText string) *llm.Request {
		return &llm.Request{
			Model: "replay",
			Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: []llm.Block{{Kind: llm.BlockText, Text: "你是助手"}}},
				{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: userText}}},
			},
		}
	}

	// 相关请求:应多出一条 system 记忆消息。
	got, err := app.Root().Waterfall(context.Background(), llm.EventRequest, request("请用中文回答这个问题"))
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	req, ok := got.(*llm.Request)
	if !ok {
		t.Fatalf("返回值应还原成 *llm.Request,实际 %T", got)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("应注入 1 条 system 消息,实际 %d 条: %+v", len(req.Messages), req.Messages)
	}
	injected := req.Messages[1]
	if injected.Role != llm.RoleSystem || injected.Text() == "" {
		t.Fatalf("注入消息形状不正确: %+v", injected)
	}
	if text := injected.Text(); !strings.Contains(text, "【长期记忆】") || !strings.Contains(text, "中文回答") {
		t.Fatalf("召回内容不正确: %q", text)
	}

	// 无关请求:不应注入,消息数保持 2。
	got, err = app.Root().Waterfall(context.Background(), llm.EventRequest, request("今天天气怎么样"))
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	req = got.(*llm.Request)
	if len(req.Messages) != 2 {
		t.Fatalf("无关请求不应注入记忆,实际 %d 条: %+v", len(req.Messages), req.Messages)
	}
}
