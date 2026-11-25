package toolworkspace_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
	toolworkspace "github.com/issueye/ada_go_v2/plugins/tool/workspace-helper"
)

func setup(t *testing.T, root string) (*kernel.App, *tools.Registry) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	if _, err := app.Plugin(workspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 workspace seam 失败: %v", err)
	}
	if _, err := app.Plugin(tools.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 tools seam 失败: %v", err)
	}
	if _, err := app.Plugin(workspacelocal.Plugin(), kernel.NewConfig(map[string]any{
		"workspaces": []any{
			map[string]any{"id": "alpha", "root": root, "readOnly": true, "ignore": []any{".git"}},
			map[string]any{"id": "beta", "root": root},
		},
	})); err != nil {
		t.Fatalf("挂载 workspace provider 失败: %v", err)
	}
	if _, err := app.Plugin(toolworkspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 tool 失败: %v", err)
	}
	reg, err := kernel.Service[*tools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatalf("取 tools 服务失败: %v", err)
	}
	return app, reg
}

func invoke(t *testing.T, reg *tools.Registry, ctx context.Context, name string) tools.Result {
	t.Helper()
	res, err := reg.Invoke(ctx, tools.CallInfo{Name: name, Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("调用 %s 失败: %v", name, err)
	}
	return res
}

// 1. 两个工具都注册且只读;info 输出包含 current 与绑定,但不含宿主根。
func TestInfoAndList(t *testing.T) {
	root := t.TempDir()
	app, reg := setup(t, root)
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	list, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	found := map[string]tools.ToolInfo{}
	for _, info := range list {
		found[info.Name] = info
	}
	for _, name := range []string{"workspace_info", "workspace_list"} {
		if _, ok := found[name]; !ok {
			t.Fatalf("缺少工具 %s", name)
		}
		if !found[name].ReadOnly || found[name].Source != toolworkspace.PluginName {
			t.Fatalf("%s 元信息不正确: %+v", name, found[name])
		}
	}

	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "beta"})
	res := invoke(t, reg, ctx, "workspace_info")
	if res.Status != tools.StatusOK {
		t.Fatalf("workspace_info 失败: %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, `"binding": "beta"`) || !strings.Contains(text, `"id": "beta"`) {
		t.Fatalf("绑定信息不正确: %s", text)
	}
	vol := filepath.VolumeName(root)
	if strings.Contains(text, root) || (vol != "" && strings.Contains(strings.ToLower(text), strings.ToLower(vol))) {
		t.Fatalf("工具结果不应包含宿主根: %s", text)
	}

	res = invoke(t, reg, context.Background(), "workspace_list")
	if res.Status != tools.StatusOK {
		t.Fatalf("workspace_list 失败: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, `"alpha"`) || !strings.Contains(res.Content[0].Text, `"beta"`) {
		t.Fatalf("列表不完整: %s", res.Content[0].Text)
	}
	if strings.Contains(res.Content[0].Text, root) {
		t.Fatalf("列表不应包含宿主根: %s", res.Content[0].Text)
	}
}

// 2. 未知绑定必须报错,不能悄悄回退到 current。
func TestUnknownBindingFails(t *testing.T) {
	app, reg := setup(t, t.TempDir())
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "missing"})
	res := invoke(t, reg, ctx, "workspace_info")
	if res.Status != tools.StatusError || !strings.Contains(res.Content[0].Text, "ENOTFOUND") {
		t.Fatalf("未知绑定应 ENOTFOUND,实际 %+v", res)
	}
}

// 3. 没有 provider 时工具返回 EUNIMPLEMENTED,而不是 panic/空结果。
func TestNoProvider(t *testing.T) {
	app := kernel.New(kernel.Options{})
	if _, err := app.Plugin(workspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	if _, err := app.Plugin(tools.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 tools 失败: %v", err)
	}
	if _, err := app.Plugin(toolworkspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 tool 失败: %v", err)
	}
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	reg, err := kernel.Service[*tools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatalf("取 tools 失败: %v", err)
	}
	res := invoke(t, reg, context.Background(), "workspace_info")
	if res.Status != tools.StatusError || !strings.Contains(res.Content[0].Text, "EUNIMPLEMENTED") {
		t.Fatalf("无 provider 应 EUNIMPLEMENTED,实际 %+v", res)
	}
}
