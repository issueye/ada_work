package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/jsrt"
)

// TestNodeChildProcess 验证 node:child_process 只有 execFile/execFileSync,
// 且命令经 shell seam 执行(不经过 shell 解析)。
func TestNodeChildProcess(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-shell", nil)
	mountBuiltin(t, app, "shell-local", map[string]any{"workspace": t.TempDir()})

	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { execFileSync, spawn } from "node:child_process"
type Ctx = { provide: (name: string, value: unknown) => void }
export function apply(ctx: Ctx) {
  const out = execFileSync("go", ["version"], { cwd: "/workspace", timeout: 10000 })
  let spawnError = ""
  try { spawn("go", []) } catch (e) { spawnError = e.code }
  ctx.provide("child.probe", { out, spawnError })
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "node-child", Artifact: art,
		Inject: kernel.Inject{Names: []string{"shell"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("child.probe")
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(*jsrt.Service).Data()
	out, _ := data["out"].(string)
	if !strings.Contains(out, "go version") {
		t.Fatalf("execFileSync 输出不正确: %q", out)
	}
	if data["spawnError"] != "ERR_NOT_SUPPORTED" {
		t.Fatalf("spawn 应显式失败: %+v", data)
	}
}
