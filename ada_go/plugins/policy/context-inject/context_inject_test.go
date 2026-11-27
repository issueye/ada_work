package contextinject_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	contextinject "github.com/issueye/ada_go_v2/plugins/policy/context-inject"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamllm "github.com/issueye/ada_go_v2/plugins/seam/llm"
)

func mount(t *testing.T, app *kernel.App, p kernel.Plugin, cfg map[string]any) {
	t.Helper()
	f, err := app.Plugin(p, kernel.NewConfig(cfg))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("未就绪: %v", err)
	}
}

// workspaceApp 造一个带真实工作区文件的 app:seam-fs + fs-local + context-inject。
func workspaceApp(t *testing.T, files map[string]string, conf map[string]any) *kernel.App {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount(t, app, seamfs.Plugin(), nil)
	mount(t, app, fslocal.Plugin(), map[string]any{"workspace": dir})
	mount(t, app, contextinject.Plugin(), conf)
	return app
}

func requestWithUser(text string) *seamllm.Request {
	return &seamllm.Request{
		Model: "m",
		Messages: []seamllm.Message{
			{Role: seamllm.RoleSystem, Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: "你是助手"}}},
			{Role: seamllm.RoleUser, Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: text}}},
		},
	}
}

// 1. 文件与静态文本都注入进第一条 system 消息,原有内容保留。
func TestInjectsFilesAndText(t *testing.T) {
	app := workspaceApp(t, map[string]string{
		"AGENTS.md": "规则:先读文档再动手",
	}, map[string]any{
		"files": []string{"AGENTS.md"},
		"text":  "额外说明:这是一个 Go 项目",
	})
	req := requestWithUser("你好")
	out, err := app.Root().Waterfall(context.Background(), seamllm.EventRequest, req)
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	got, ok := out.(*seamllm.Request)
	if !ok || got == nil {
		t.Fatalf("应返回改写后的请求: %T", out)
	}
	system := joinBlocks(got.Messages[0])
	for _, want := range []string{"你是助手", "## 工作区上下文", "### AGENTS.md", "先读文档再动手", "额外说明"} {
		if !strings.Contains(system, want) {
			t.Fatalf("system 缺少 %q:\n%s", want, system)
		}
	}
	if len(got.Messages) != 2 {
		t.Fatalf("不应新增 system 消息: %d 条", len(got.Messages))
	}
	if got.Messages[1].Role != seamllm.RoleUser || got.Messages[1].Content[0].Text != "你好" {
		t.Fatalf("用户消息被改动: %+v", got.Messages[1])
	}
}

// 2. 文件不存在时跳过,不注入、不报错。
func TestMissingFileIsSkipped(t *testing.T) {
	app := workspaceApp(t, nil, map[string]any{"files": []string{"nope.md"}})
	req := requestWithUser("hi")
	out, err := app.Root().Waterfall(context.Background(), seamllm.EventRequest, req)
	if err != nil {
		t.Fatalf("waterfall 不应失败: %v", err)
	}
	got := out.(*seamllm.Request)
	if text := joinBlocks(got.Messages[0]); text != "你是助手" {
		t.Fatalf("没有可注入内容时应保持原样: %q", text)
	}
}

// 3. 超长内容按 maxBytes 截断并留下说明。
func TestMaxBytesTruncates(t *testing.T) {
	big := strings.Repeat("很长的说明。", 200)
	app := workspaceApp(t, map[string]string{"AGENTS.md": big}, map[string]any{
		"files": []string{"AGENTS.md"}, "maxBytes": 256,
	})
	req := requestWithUser("hi")
	out, _ := app.Root().Waterfall(context.Background(), seamllm.EventRequest, req)
	system := joinBlocks(out.(*seamllm.Request).Messages[0])
	// 注入段(标题+内容)不超过 maxBytes;原 system 消息与分隔符另算。
	if injected := system[strings.Index(system, "## 工作区上下文"):]; len(injected) > 256 {
		t.Fatalf("注入内容应截断到 256 字节,实际 %d", len(injected))
	}
	if !strings.Contains(system, "上下文已截断") {
		t.Fatalf("截断应带说明: %q", system)
	}
}

// 4. 没有 system 消息时新建一条放在最前面。
func TestPrependsSystemMessage(t *testing.T) {
	app := workspaceApp(t, nil, map[string]any{"text": "项目约定:用中文回答"})
	req := &seamllm.Request{
		Model: "m",
		Messages: []seamllm.Message{
			{Role: seamllm.RoleUser, Content: []seamllm.Block{{Kind: seamllm.BlockText, Text: "hi"}}},
		},
	}
	out, err := app.Root().Waterfall(context.Background(), seamllm.EventRequest, req)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*seamllm.Request)
	if len(got.Messages) != 2 || got.Messages[0].Role != seamllm.RoleSystem {
		t.Fatalf("应前置一条 system 消息: %+v", got.Messages)
	}
	if !strings.Contains(got.Messages[0].Content[0].Text, "用中文回答") {
		t.Fatalf("system 内容不正确: %q", got.Messages[0].Content[0].Text)
	}
}

// joinBlocks 把一条消息的所有文本块拼起来:注入是追加块,不是改写原块。
func joinBlocks(m seamllm.Message) string {
	var b strings.Builder
	for _, block := range m.Content {
		b.WriteString(block.Text)
	}
	return b.String()
}
