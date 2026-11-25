// 命令 ada-ts-demo 演示 M2 的闭环:
// 一份 TS 插件(带 enum、参数属性、跨文件导入)被 esbuild 降级、被 moejs 在进程内加载,
// 作为普通 kernel 插件装配;它在 apply 期间通过 ctx 提供服务、派发事件,
// 由 Go 侧的消费者与监听器接收——全程没有 Node,也没有第二个进程。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

const pluginTS = `import { format } from "./src/util"
export enum Level { High = 3 }
type Ctx = {
  log: (m: string) => void
  provide: (name: string, value: unknown) => void
  emit: (event: string, payload: unknown) => void
  on: (event: string, handler: (payload: any) => void) => void
  effect: (label: string, cleanup: () => void) => void
  waterfall: (event: string, handler: (x: any, next: (v: any) => any) => any) => void
}
export function apply(ctx: Ctx, cfg: { name: string }) {
  ctx.log(format(cfg.name))
  ctx.provide("ts.greeting", {
    text: format(cfg.name),
    level: Level.High,
    render(): string { return this.text + "!" },
  })
  ctx.emit("ts/ready", { name: cfg.name })
  ctx.on("ts/ping", (p) => {
    ctx.log("收到 ping:" + p.n)
    ctx.emit("ts/pong", { n: p.n + 1 })
  })
  ctx.effect("ts-demo", () => ctx.log("清理: ts-demo"))
  ctx.waterfall("ts/chain", (x: number, next: (v: number) => number) => next(x + 1))
}`

const utilTS = `export function format(name: string): string { return "你好," + name }`

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

func main() {
	artifact, err := compile.Build("ts-demo", "plugin.ts", map[string]string{
		"plugin.ts":   pluginTS,
		"src/util.ts": utilTS,
	}, compile.Options{})
	if err != nil {
		fmt.Println("编译失败:", err)
		os.Exit(1)
	}
	fmt.Printf("Artifact: entry=%s files=%d digest=%s\n", artifact.Entry, len(artifact.Files), artifact.Digest[:19]+"…")

	plugin, err := bridge.New(bridge.Options{
		Name:     "ts-demo",
		Artifact: artifact,
		Logf:     func(format string, args ...any) { fmt.Printf("[js] "+format+"\n", args...) },
	})
	if err != nil {
		fmt.Println("构造插件失败:", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app := kernel.New(kernel.Options{Logger: logger})
	defer func() { _ = app.Stop(context.Background()) }()

	// Go 侧消费者:先挂载,依赖 JS 提供的服务;激活后调用一次 JS 方法。
	var (
		seenText any
		rendered any
	)
	consumer := kernel.Func("ts-consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		svc, err := kernel.Service[*jsrt.Service](ctx, "ts.greeting")
		if err != nil {
			return nil, err
		}
		seenText = svc.Data()["text"]
		rendered, err = svc.Call(context.Background(), "render")
		return nil, err
	}).WithInject("ts.greeting")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		fmt.Println("挂载消费者失败:", err)
		os.Exit(1)
	}

	// 事件监听:JS 侧 emit 在 apply 返回后由内核协程提交。
	app.Root().On("ts/ready", func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		fmt.Println("[go] 收到 ts/ready:", args)
		return nil, nil
	})
	// 运行期 emit:JS 处理器里再次 emit 的事件也应到达 Go。
	app.Root().On("ts/pong", func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		fmt.Println("[go] 收到 ts/pong:", args)
		return nil, nil
	})

	fiber, err := app.Plugin(plugin, kernel.NewConfig(map[string]any{"name": "ada"}))
	if err != nil {
		fmt.Println("挂载失败:", err)
		os.Exit(1)
	}
	fmt.Println("fiber 状态:", fiber.State())

	// 反向回调:内核派发事件,JS 处理器在自己的 goroutine 上执行。
	app.Root().Emit("ts/ping", map[string]any{"n": 1})

	// waterfall:JS 中间件把输入 +1,Go 侧下游再 ×2。
	app.Root().OnWaterfall("ts/chain", func(_ context.Context, _ *kernel.Context, in any, next kernel.Next) (any, error) {
		return next(asInt64(in) * 2)
	})
	out, err := app.Root().Waterfall(context.Background(), "ts/chain", 1)
	if err != nil {
		fmt.Println("waterfall 失败:", err)
	} else {
		fmt.Println("waterfall 结果(JS +1 → Go ×2):", out)
	}

	fmt.Println("Go 消费者拿到的服务数据:", seenText)
	fmt.Println("Go 调用 JS 方法 render():", rendered)
	if fiber.State() != kernel.Active {
		os.Exit(1)
	}

	if err := fiber.Dispose(context.Background()); err != nil {
		fmt.Println("卸载失败:", err)
	}
	fmt.Println("卸载后:", fiber.State())
}
