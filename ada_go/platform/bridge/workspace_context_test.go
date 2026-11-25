package bridge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// JS 原生 fs 回调必须继承工具调用 ctx;否则脚本工具永远落在 Current,看不到会话绑定。
func TestJSNativeFSInheritsCallBinding(t *testing.T) {
	alpha, beta := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(alpha, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "a.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { read } from "ada:fs"
type Ctx = { tool: (spec: any, handler: (args: any) => any) => void }
export function apply(ctx: Ctx) {
  ctx.tool(
    { name: "read_ws", summary: "读取当前绑定的工作区", readOnly: true, input: { type: "object", properties: {} } },
    () => read("/workspace/a.txt"),
  )
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "js-ws", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools", "fs"}},
	})
	if err != nil {
		t.Fatalf("构造 bridge 失败: %v", err)
	}

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(plugin kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(plugin, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", plugin.Name(), err)
		}
	}
	mount(workspace.Plugin(), nil)
	mount(workspacelocal.Plugin(), map[string]any{
		"active": "alpha",
		"workspaces": []any{
			map[string]any{"id": "alpha", "root": alpha},
			map[string]any{"id": "beta", "root": beta},
		},
	})
	mount(seamfs.Plugin(), nil)
	mount(fslocal.Plugin(), map[string]any{"workspace": "", "tmp": t.TempDir()})
	mount(seamtools.Plugin(), nil)
	bf, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 bridge 失败: %v", err)
	}
	if err := bf.Ready(context.Background()); err != nil {
		t.Fatalf("bridge 未就绪: %v", err)
	}

	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatalf("取 tools 失败: %v", err)
	}
	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "beta"})
	call := seamtools.CallInfo{Name: "read_ws", Args: json.RawMessage(`{}`)}
	res, err := reg.Invoke(ctx, call)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	text := ""
	if len(res.Content) > 0 {
		text = strings.TrimSpace(res.Content[0].Text)
	}
	if res.Status != seamtools.StatusOK || text != "beta" {
		t.Fatalf("JS fs 未继承绑定: status=%s text=%q", res.Status, text)
	}
}
