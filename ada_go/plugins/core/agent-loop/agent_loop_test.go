package agentloop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	replay "github.com/issueye/ada_go_v2/plugins/provider/llm-replay"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// 投影存在且超过 contextBudget 时,agent 必须返回 ECONTEXT,而不是把超长请求发给模型。
func TestContextBudgetReturnsECONTEXT(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(ctx) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}
	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("session-stats", nil)
	mount("seam-prompts", nil)
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{
		"steps": []replay.Step{{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}},
			Finish:  llm.FinishStop,
		}}},
		"models":       []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}},
		"defaultModel": "replay",
	})
	mount("seam-tools", nil)
	mount("agent-loop", map[string]any{"model": "replay", "contextBudget": 1})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ag.Run(ctx, sess, "hello")
	if !seamerr.Is(err, seamerr.ErrContext) {
		t.Fatalf("应返回 ECONTEXT,实际 %v", err)
	}
}

// 超预算时 agent 应调用 compaction 服务,压缩后继续请求,而不是直接拒绝。
func TestCompactionFitsBudget(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(ctx) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}
	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("session-stats", nil)
	mount("compaction-basic", map[string]any{"keepRecent": 4})
	mount("seam-prompts", nil)
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{
		"steps": []replay.Step{{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "done"}}},
			Finish:  llm.FinishStop,
		}}},
		"models":       []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}},
		"defaultModel": "replay",
	})
	mount("seam-tools", nil)
	mount("agent-loop", map[string]any{"model": "replay", "contextBudget": 300})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 200)
	for i := 0; i < 12; i++ {
		data, _ := json.Marshal(llm.Message{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: long}}})
		if _, err := sess.Append(session.Event{
			Type: session.TypeUserMessage,
			Data: data,
		}); err != nil {
			t.Fatalf("预置历史失败: %v", err)
		}
	}
	res, err := ag.Run(ctx, sess, "short")
	if err != nil {
		t.Fatalf("压缩后应继续,实际 %v", err)
	}
	if res.Text != "done" {
		t.Fatalf("回复不正确: %+v", res)
	}
}

// 同一步的多个工具调用必须并行;串行实现会在 barrier 处超时。
func TestParallelTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(ctx) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	barrier := kernel.Func("barrier-tools", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		return reg.Register(tools.ToolDef{
			Name: "barrier",
			Handler: func(ctx context.Context, _ tools.CallInfo) (tools.Result, error) {
				started <- struct{}{}
				select {
				case <-release:
					return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}, nil
				case <-ctx.Done():
					return tools.Result{}, ctx.Err()
				}
			},
		})
	}).WithInject("tools")

	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", nil)
	mount("seam-tools", nil)
	fiber, err := app.Plugin(barrier, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 barrier 失败: %v", err)
	}
	if err := fiber.Ready(ctx); err != nil {
		t.Fatalf("barrier 未就绪: %v", err)
	}
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{
		"steps": []replay.Step{
			{Response: llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "run"}}},
				ToolCalls: []llm.ToolCall{
					{ID: "c1", Name: "barrier"},
					{ID: "c2", Name: "barrier"},
				},
				Finish: llm.FinishToolCalls,
			}},
			{Response: llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "done"}}},
				Finish:  llm.FinishStop,
			}},
		},
		"models":       []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}},
		"defaultModel": "replay",
	})
	mount("agent-loop", map[string]any{"model": "replay", "maxParallelTools": 2})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ag.Run(ctx, sess, "start"); done <- err }()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("工具调用没有并行: 只收到 %d 个开始信号", i)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("并行执行失败: %v", err)
	}
}

// 没有步数上限:模型连续调用工具超过旧的 8 步仍能继续,直到模型自己停止。
func TestNoStepLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(ctx) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}
	noop := kernel.Func("noop-tools", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		return reg.Register(tools.ToolDef{
			Name: "noop",
			Handler: func(context.Context, tools.CallInfo) (tools.Result, error) {
				return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}, nil
			},
		})
	}).WithInject("tools")

	steps := make([]replay.Step, 0, 11)
	for i := 0; i < 10; i++ {
		steps = append(steps, replay.Step{Response: llm.Response{
			Message:   llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "step"}}},
			ToolCalls: []llm.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "noop"}},
			Finish:    llm.FinishToolCalls,
		}})
	}
	steps = append(steps, replay.Step{Response: llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "done"}}},
		Finish:  llm.FinishStop,
	}})

	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", nil)
	mount("seam-tools", nil)
	fiber, err := app.Plugin(noop, kernel.NewConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := fiber.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{
		"steps":        steps,
		"models":       []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}},
		"defaultModel": "replay",
	})
	mount("agent-loop", map[string]any{"model": "replay"})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ag.Run(ctx, sess, "keep going")
	if err != nil {
		t.Fatalf("无限步运行失败: %v", err)
	}
	if res.Steps != 11 || res.ToolCalls != 10 || res.Text != "done" {
		t.Fatalf("步数/工具/结束不正确: %+v", res)
	}
}

func TestTurnWithReplayAndTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk, ok := builtins.Plugins()[id]
		if !ok {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}

	steps := []replay.Step{
		{Response: llm.Response{
			Message:   llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "我先读文件"}}},
			ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read_file", Args: json.RawMessage(`{"path":"a.txt"}`)}},
			Finish:    llm.FinishToolCalls,
			Usage:     llm.Usage{InputTokens: 12, OutputTokens: 4},
		}},
		{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "文件内容是 hello"}}},
			Finish:  llm.FinishStop,
			Usage:   llm.Usage{InputTokens: 20, OutputTokens: 6},
		}},
	}
	model := []llm.ModelInfo{{ID: "replay", ContextWindow: 8192, MaxOutput: 1024}}

	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", map[string]any{"budget": 4096})
	mount("system-prompt", map[string]any{"identity": "你是测试助手", "rules": []string{"只读工作区"}})
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{"steps": steps, "models": model, "defaultModel": "replay"})
	mount("seam-tools", nil)
	mount("seam-fs", nil)
	mount("fs-local", map[string]any{"workspace": workspace})
	mountToolText(t, app)
	mount("agent-loop", map[string]any{"model": "replay"})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatalf("取 agent 服务失败: %v", err)
	}
	agent, ok := raw.(*agentloop.Agent)
	if !ok {
		t.Fatalf("agent 服务类型不对: %T", raw)
	}

	sess, err := agent.Open(ctx, session.Meta{Title: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(ctx, sess, "读取 a.txt 并总结")
	if err != nil {
		t.Fatalf("跑一轮失败: %v", err)
	}
	if res.Text != "文件内容是 hello" || res.Steps != 2 || res.ToolCalls != 1 {
		t.Fatalf("结果不对: %+v", res)
	}

	evs, err := sess.Events(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		session.TypeTurnStart, session.TypeUserMessage, session.TypeStepStart,
		session.TypeAssistantMessage, session.TypeToolCall, session.TypeToolResult,
		session.TypeStepEnd, session.TypeStepStart, session.TypeAssistantMessage,
		session.TypeStepEnd, session.TypeTurnEnd,
	}
	if len(evs) != len(want) {
		t.Fatalf("事件数 %d,期望 %d", len(evs), len(want))
	}
	for i, typ := range want {
		if evs[i].Type != typ {
			t.Fatalf("第 %d 条事件 %s,期望 %s", i+1, evs[i].Type, typ)
		}
	}

	var toolPayload struct {
		Call   tools.CallInfo `json:"call"`
		Result tools.Result   `json:"result"`
	}
	if err := json.Unmarshal(evs[5].Data, &toolPayload); err != nil {
		t.Fatal(err)
	}
	if toolPayload.Result.Status != tools.StatusOK || len(toolPayload.Result.Content) == 0 || toolPayload.Result.Content[0].Text != "hello" {
		t.Fatalf("工具结果不对: %+v", toolPayload.Result)
	}
}

// 工作区绑定必须从 session.Meta 进入调用 ctx,工具能读到同一绑定。
func TestWorkspaceBindingPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(ctx) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}

	got := make(chan workspace.Binding, 1)
	capture := kernel.Func("capture-workspace", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		return reg.Register(tools.ToolDef{
			Name: "capture",
			Handler: func(ctx context.Context, _ tools.CallInfo) (tools.Result, error) {
				b, _ := workspace.BindingFrom(ctx)
				got <- b
				return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}, nil
			},
		})
	}).WithInject("tools")

	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", nil)
	mount("seam-tools", nil)
	fiber, err := app.Plugin(capture, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 capture 失败: %v", err)
	}
	if err := fiber.Ready(ctx); err != nil {
		t.Fatalf("capture 未就绪: %v", err)
	}
	mount("seam-llm", nil)
	mount("llm-replay", map[string]any{
		"steps": []replay.Step{
			{Response: llm.Response{
				Message:   llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "run"}}},
				ToolCalls: []llm.ToolCall{{ID: "c1", Name: "capture"}},
				Finish:    llm.FinishToolCalls,
			}},
			{Response: llm.Response{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "done"}}},
				Finish:  llm.FinishStop,
			}},
		},
		"models":       []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}},
		"defaultModel": "replay",
	})
	mount("agent-loop", map[string]any{"model": "replay"})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{Workspace: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(ctx, sess, "start"); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	select {
	case b := <-got:
		if b.Workspace != "beta" || b.SessionID != sess.ID() {
			t.Fatalf("绑定未正确传播: %+v", b)
		}
	default:
		t.Fatalf("工具没有收到绑定")
	}
}
