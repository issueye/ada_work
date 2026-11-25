package bridge_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// asyncPluginSource 是一个「装配与工具都异步」的插件:
// apply 里有 await,工具处理器也 await,用来守住事件循环的端到端契约
// (docs/event-loop.md §3.1)。
const asyncPluginSource = `
type Ctx = {
  log: (msg: string) => void
  tool: (spec: any, handler: (args: any) => any) => void
}

function pause(ms: number): Promise<void> {
  return new Promise<void>((resolve) => { setTimeout(() => resolve(), ms) })
}

export async function apply(ctx: Ctx): Promise<void> {
  await pause(1)
  ctx.log("async apply done")
  ctx.tool(
    {
      name: "async_echo",
      summary: "异步回显",
      input: {
        type: "object",
        properties: { text: { type: "string" } },
        required: ["text"],
      },
    },
    async (args: any) => {
      await pause(1)
      const text = args && typeof args.text === "string" ? args.text : ""
      return { text: "echo:" + text }
    },
  )
}
`

// TestAsyncApplyAndTool 验证 async apply 与 async 工具处理器都能在 bridge 下正常结算。
func TestAsyncApplyAndTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plugin.json"), `{
  "id": "async-helper",
  "name": "async helper (script)",
  "version": "0.1.0",
  "kind": "ts",
  "entry": "plugin.ts",
  "inject": {"required": ["tools"]}
}`)
	writeFile(t, filepath.Join(dir, "plugin.ts"), asyncPluginSource)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)

	manifest, err := plugin.Load(dir)
	if err != nil {
		t.Fatalf("读取清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载插件失败: %v", err)
	}
	// Ready 会跑 apply:async apply 里的 await 必须在这里完成,工具才注册得上。
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("插件未就绪: %v", err)
	}

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)

	res := pluginCall(t, reg, "async_echo", map[string]any{"text": "hi"})
	if got := resultText(res); !strings.Contains(got, "echo:hi") {
		t.Fatalf("异步工具返回值不对: %q", got)
	}
}
