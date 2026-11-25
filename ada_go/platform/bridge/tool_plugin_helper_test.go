package bridge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// mountPluginHelper 挂载 seam-tools + fs-local(临时工作区)+ plugin-helper(TS)。
func mountPluginHelper(t *testing.T, workspace string) (*tools.Registry, string) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": workspace})

	manifest, err := plugin.Load("../../plugins/tool/plugin-helper")
	if err != nil {
		t.Fatalf("读取 plugin-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 plugin-helper 失败: %v", err)
	}
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 plugin-helper 失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("plugin-helper 未就绪: %v", err)
	}
	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*tools.Registry), workspace
}

func pluginCall(t *testing.T, reg *tools.Registry, name string, args map[string]any) tools.Result {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: name, Args: data})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	return res
}

func resultText(res tools.Result) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

// TestPluginHelperScaffoldAndLoad 是「生成 → 体检 → 列表 → 真的装上」的闭环:
// 生成的 TS 插件必须能被 loader 装载并调用它自己的工具。
func TestPluginHelperScaffoldAndLoad(t *testing.T) {
	ws := t.TempDir()
	reg, _ := mountPluginHelper(t, ws)

	res := pluginCall(t, reg, "plugin_list", map[string]any{})
	if res.Status != tools.StatusOK {
		t.Fatalf("空目录应正常返回: %+v", res)
	}
	if !strings.Contains(resultText(res), "没有找到") {
		t.Fatalf("空目录应提示没有插件: %q", resultText(res))
	}

	res = pluginCall(t, reg, "plugin_scaffold", map[string]any{
		"id":      "demo-helper",
		"name":    "Demo Helper",
		"summary": "演示插件:回显一段文本",
		"tool":    "demo_echo",
	})
	if res.Status != tools.StatusOK {
		t.Fatalf("生成失败: %+v", res)
	}
	dir := filepath.Join(ws, "plugins", "demo-helper")
	for _, name := range []string{"plugin.json", "plugin.ts", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("缺少生成文件 %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var man struct {
		ID     string              `json:"id"`
		Kind   string              `json:"kind"`
		Entry  string              `json:"entry"`
		Inject map[string][]string `json:"inject"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("生成的清单不是合法 JSON: %v", err)
	}
	if man.ID != "demo-helper" || man.Kind != "ts" || man.Entry != "plugin.ts" {
		t.Fatalf("清单字段不对: %+v", man)
	}
	if len(man.Inject["required"]) == 0 || man.Inject["required"][0] != "tools" {
		t.Fatalf("有示例工具时 inject 应包含 tools: %+v", man.Inject)
	}

	// 列表能找到它。
	res = pluginCall(t, reg, "plugin_list", map[string]any{"root": "/workspace/plugins"})
	if res.Status != tools.StatusOK || !strings.Contains(resultText(res), "demo-helper") {
		t.Fatalf("列表没有新插件: %+v %q", res, resultText(res))
	}

	// 体检通过。
	res = pluginCall(t, reg, "plugin_show", map[string]any{"dir": "/workspace/plugins/demo-helper"})
	if res.Status != tools.StatusOK {
		t.Fatalf("体检应通过: %+v", res)
	}
	if !strings.Contains(resultText(res), "ok: id 与目录同名") {
		t.Fatalf("体检报告缺少 ok 项: %q", resultText(res))
	}

	// 重复生成必须被拒(不覆盖)。
	res = pluginCall(t, reg, "plugin_scaffold", map[string]any{"id": "demo-helper"})
	if res.Status != tools.StatusError || !strings.Contains(resultText(res), "已存在") {
		t.Fatalf("重复生成应被拒: %+v", res)
	}

	// 只支持 TS:schema 应拒绝 kind=go。
	res = pluginCall(t, reg, "plugin_scaffold", map[string]any{"id": "demo-go", "kind": "go"})
	if res.Status != tools.StatusError || !strings.Contains(resultText(res), "ESCHEMA") {
		t.Fatalf("kind=go 应被 schema 拒绝: %+v %q", res, resultText(res))
	}
	if _, err := os.Stat(filepath.Join(ws, "plugins", "demo-go")); err == nil {
		t.Fatalf("kind=go 不应留下任何文件")
	}

	// 生成的插件真的能被装载,并调用它自己的工具。
	app2 := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app2.Stop(context.Background()) })
	mountBuiltin(t, app2, "seam-tools", nil)
	manifest, err := plugin.Load(dir)
	if err != nil {
		t.Fatalf("生成的插件清单不能被读取: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("生成的插件不能被构造: %v", err)
	}
	mount(t, app2, p)
	rawReg, err := app2.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	genReg := rawReg.(*tools.Registry)
	res2 := pluginCall(t, genReg, "demo_echo", map[string]any{"text": "hello"})
	if res2.Status != tools.StatusOK || !strings.Contains(resultText(res2), "demo-helper: hello") {
		t.Fatalf("生成的插件工具没有工作: %+v %q", res2, resultText(res2))
	}
}

// TestPluginHelperPreset 覆盖 preset 的加目录/启停:改完必须是合法 JSON 且状态正确。
func TestPluginHelperPreset(t *testing.T) {
	ws := t.TempDir()
	reg, _ := mountPluginHelper(t, ws)

	preset := filepath.Join(ws, "demo.preset.json")
	body := `{"name":"demo","plugins":["plugins"],"disable":["x-helper"]}`
	if err := os.WriteFile(preset, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res := pluginCall(t, reg, "plugin_preset", map[string]any{
		"preset": "/workspace/demo.preset.json",
		"action": "add",
		"dir":    "plugins/demo-helper",
	})
	if res.Status != tools.StatusOK {
		t.Fatalf("加目录失败: %+v", res)
	}

	res = pluginCall(t, reg, "plugin_preset", map[string]any{
		"preset": "/workspace/demo.preset.json",
		"action": "disable",
		"id":     "demo-helper",
	})
	if res.Status != tools.StatusOK {
		t.Fatalf("禁用失败: %+v", res)
	}
	res = pluginCall(t, reg, "plugin_preset", map[string]any{
		"preset": "/workspace/demo.preset.json",
		"action": "enable",
		"id":     "x-helper",
	})
	if res.Status != tools.StatusOK {
		t.Fatalf("启用失败: %+v", res)
	}

	raw, err := os.ReadFile(preset)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Plugins []string `json:"plugins"`
		Disable []string `json:"disable"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("改完的 preset 不是合法 JSON: %v\n%s", err, raw)
	}
	if !hasString(got.Plugins, "plugins/demo-helper") {
		t.Fatalf("plugins 里没有新目录: %+v", got.Plugins)
	}
	if !hasString(got.Disable, "demo-helper") {
		t.Fatalf("disable 里没有 demo-helper: %+v", got.Disable)
	}
	if hasString(got.Disable, "x-helper") {
		t.Fatalf("enable 应从 disable 里移除 x-helper: %+v", got.Disable)
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
