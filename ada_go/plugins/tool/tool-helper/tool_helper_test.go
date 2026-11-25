package toolhelper_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
	toolhelper "github.com/issueye/ada_go_v2/plugins/tool/tool-helper"
)

// mountHelper 挂载 seam-tools 与 tool-helper,返回注册表。
func mountHelper(t *testing.T) *seamtools.Registry {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	f, err := app.Plugin(seamtools.Plugin(), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 seam-tools 失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("seam-tools 未就绪: %v", err)
	}
	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*seamtools.Registry)

	// 目标工具:echo 与 fail 都是普通 Go 工具,用来验证 tool_exec 的转发与状态。
	echo := seamtools.ToolDef{
		Name: "echo", Summary: "回显 text", ReadOnly: true, Source: "test",
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"text"},
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		}),
		Handler: func(ctx context.Context, call seamtools.CallInfo) (seamtools.Result, error) {
			var args struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(call.Args, &args)
			return seamtools.Result{Status: seamtools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: "echo: " + args.Text}}}, nil
		},
	}
	fail := seamtools.ToolDef{
		Name: "fail", Summary: "总是失败", Source: "test",
		Handler: func(context.Context, seamtools.CallInfo) (seamtools.Result, error) {
			return seamtools.Result{Status: seamtools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: "目标工具失败"}}}, nil
		},
	}
	for _, def := range []seamtools.ToolDef{echo, fail} {
		if _, err := reg.Register(def); err != nil {
			t.Fatalf("注册目标工具 %s 失败: %v", def.Name, err)
		}
	}

	hf, err := app.Plugin(toolhelper.Plugin(), kernel.NewConfig(map[string]any{
		"maxDepth": 1, "defaultLimit": 5,
	}))
	if err != nil {
		t.Fatalf("挂载 tool-helper 失败: %v", err)
	}
	if err := hf.Ready(context.Background()); err != nil {
		t.Fatalf("tool-helper 未就绪: %v", err)
	}
	return reg
}

func invoke(t *testing.T, reg *seamtools.Registry, name string, args map[string]any) seamtools.Result {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(context.Background(), seamtools.CallInfo{Name: name, Args: data})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	return res
}

func text(res seamtools.Result) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

// tool_find:按名字过滤、limit 生效、verbose 带 schema。
func TestToolFind(t *testing.T) {
	reg := mountHelper(t)

	res := invoke(t, reg, "tool_find", map[string]any{"query": "echo", "verbose": true})
	if res.Status != seamtools.StatusOK {
		t.Fatalf("tool_find 失败: %+v", res)
	}
	out := text(res)
	if !strings.Contains(out, "echo") || !strings.Contains(out, "input") {
		t.Fatalf("tool_find(verbose) 输出不正确: %s", out)
	}

	res = invoke(t, reg, "tool_find", map[string]any{"query": "不存在", "limit": 5})
	if res.Status != seamtools.StatusOK || !strings.Contains(text(res), "没有匹配") {
		t.Fatalf("无匹配时输出不正确: %+v", res)
	}

	// limit 应截断结果,但统计行仍给出命中数。
	res = invoke(t, reg, "tool_find", map[string]any{"limit": 1})
	if res.Status != seamtools.StatusOK || !strings.Contains(text(res), "显示前 1 个") {
		t.Fatalf("limit 截断不正确: %s", text(res))
	}
}

// tool_exec:转发参数与结果、目标错误透传、未知工具与自身递归明确拒绝。
func TestToolExec(t *testing.T) {
	reg := mountHelper(t)

	res := invoke(t, reg, "tool_exec", map[string]any{"name": "echo", "args": map[string]any{"text": "hi"}})
	if res.Status != seamtools.StatusOK || !strings.Contains(text(res), "echo: hi") {
		t.Fatalf("tool_exec 转发失败: %+v", res)
	}

	res = invoke(t, reg, "tool_exec", map[string]any{"name": "fail"})
	if res.Status != seamtools.StatusError || !strings.Contains(text(res), "目标工具失败") {
		t.Fatalf("目标工具的错误状态应透传: %+v", res)
	}

	res = invoke(t, reg, "tool_exec", map[string]any{"name": "missing"})
	if res.Status != seamtools.StatusError || !strings.Contains(text(res), "未知工具") {
		t.Fatalf("未知工具应报错: %+v", res)
	}

	res = invoke(t, reg, "tool_exec", map[string]any{"name": "tool_exec"})
	if res.Status != seamtools.StatusError || !strings.Contains(text(res), "不允许") {
		t.Fatalf("tool_exec 不应允许调用自身: %+v", res)
	}
}
