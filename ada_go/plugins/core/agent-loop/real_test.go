package agentloop_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// TestRealAgentTurn 是显式的真实模型端到端测试:只在设置
// ADA_LLM_ENDPOINT/ADA_LLM_API_KEY/ADA_LLM_MODEL 时运行。
func TestRealAgentTurn(t *testing.T) {
	endpoint := os.Getenv("ADA_LLM_ENDPOINT")
	key := os.Getenv("ADA_LLM_API_KEY")
	model := os.Getenv("ADA_LLM_MODEL")
	if endpoint == "" || key == "" || model == "" {
		t.Skip("未设置真实 LLM 环境变量,跳过")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		f, err := app.Plugin(builtins.Plugins()[id](), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", map[string]any{"budget": 2048})
	mount("system-prompt", map[string]any{"identity": "你是测试助手,回答尽量短;需要读文件时调用工具。"})
	mount("seam-llm", nil)
	mount("llm-openai", map[string]any{"endpoint": endpoint, "model": model, "apiKeyEnv": "ADA_LLM_API_KEY", "maxTokens": 128})
	mount("seam-tools", nil)
	mount("seam-fs", nil)
	mount("fs-local", map[string]any{"workspace": workspace})
	mountToolText(t, app)
	mount("agent-loop", map[string]any{"model": model})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := raw.(*agentloop.Agent)
	if !ok {
		t.Fatalf("agent 类型不对: %T", raw)
	}
	sess, err := agent.Open(ctx, session.Meta{Title: "real"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(ctx, sess, "请调用 read_file 读取 a.txt,然后只回复文件内容。")
	if err != nil {
		t.Fatalf("真实一轮失败: %v", err)
	}
	if res.Text == "" {
		t.Fatalf("真实模型返回空文本: %+v", res)
	}
	if res.ToolCalls < 1 {
		t.Fatalf("真实模型没有调用工具: %+v", res)
	}
	if !strings.Contains(strings.ToLower(res.Text), "hello") {
		t.Fatalf("最终文本未包含文件内容: %+v", res)
	}
	t.Logf("真实一轮: text=%q steps=%d toolCalls=%d usage=%+v", res.Text, res.Steps, res.ToolCalls, res.Usage)
}
