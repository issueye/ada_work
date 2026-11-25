package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func toInt(v any) int64 {
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

func buildArtifact(t *testing.T, files map[string]string) *compile.Artifact {
	t.Helper()
	art, err := compile.Build("demo", "plugin.ts", files, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	return art
}

// 1. TS 插件端到端:编译 → 加载 → apply(ctx, cfg) → 提供服务 + 派发事件 → 卸载。
func TestJSPluginRunsInKernel(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { greet } from "./util"
export enum Level { High = 3 }
type Ctx = {
  log: (m: string) => void
  provide: (n: string, v: unknown) => void
  emit: (e: string, p: unknown) => void
  on: (e: string, h: (p: any) => void) => void
  effect: (l: string, f: () => void) => void
  waterfall: (e: string, h: (x: any, next: (v: any) => any) => any) => void
}
export function apply(ctx: Ctx, cfg: { name: string }) {
  ctx.log(greet(cfg.name))
  ctx.provide("ts.greeting", {
    text: greet(cfg.name),
    level: Level.High,
    render() { return this.text + "!" },
  })
  ctx.emit("ts/ready", { name: cfg.name })
  ctx.on("ts/ping", (p) => { ctx.log("ping:" + p.n); ctx.emit("ts/pong", { n: p.n + 1 }) })
  ctx.effect("first", () => ctx.log("cleanup:first"))
  ctx.effect("second", () => ctx.log("cleanup:second"))
  ctx.waterfall("ts/pipeline", (x, next) => next(x + 1))
  ctx.waterfall("ts/veto", (x, next) => "blocked")
}`,
		"util.ts": `export function greet(name: string): string { return "hello, " + name }`,
	})

	p, err := bridge.New(bridge.Options{Name: "js-demo", Artifact: art})
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}

	a := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	// 消费者先挂:依赖 JS 插件提供的服务,应当在 JS 提供后自动激活;
	// 并且真的调用一次 JS 方法(内核 → JS 的反向调用)。
	var (
		gotText  any
		rendered any
	)
	consumer := kernel.Func("ts-consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		svc, err := kernel.Service[*jsrt.Service](ctx, "ts.greeting")
		if err != nil {
			return nil, err
		}
		gotText = svc.Data()["text"]
		rendered, err = svc.Call(context.Background(), "render")
		return nil, err
	}).WithInject("ts.greeting")
	cf, err := a.Plugin(consumer, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if cf.State() != kernel.Pending {
		t.Fatalf("消费者应先 pending,实际 %s", cf.State())
	}

	// 运行期 emit:JS 处理器里 emit 的事件也要能到 Go 监听器。
	var pongPayload any
	a.Root().On("ts/pong", func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			pongPayload = args[0]
		}
		return nil, nil
	})

	// 事件在根作用域监听:JS 侧 emit 的提交点是 apply 之后的内核协程。
	var eventPayload any
	a.Root().On("ts/ready", func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			eventPayload = args[0]
		}
		return nil, nil
	})

	f, err := a.Plugin(p, kernel.NewConfig(map[string]any{"name": "ada"}))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if f.State() != kernel.Active {
		t.Fatalf("JS 插件应 active,实际 %s", f.State())
	}
	if cf.State() != kernel.Active {
		t.Fatalf("依赖 JS 服务的消费者应被激活,实际 %s", cf.State())
	}
	if gotText != "hello, ada" {
		t.Fatalf("消费者拿到的服务数据不正确: %v", gotText)
	}
	if rendered != "hello, ada!" {
		t.Fatalf("JS 方法 render() 的返回值不正确: %v", rendered)
	}
	if eventPayload == nil {
		t.Fatalf("JS 侧 emit 的事件没有派发")
	}
	// 反向回调:内核派发事件 → JS 处理器在 worker 上执行,并在处理器里再次 emit。
	a.Root().Emit("ts/ping", map[string]any{"n": 1})
	if m, ok := pongPayload.(map[string]any); !ok || toInt(m["n"]) != 2 {
		t.Fatalf("运行期 emit 的载荷不正确: %v", pongPayload)
	}
	logs := p.Logs()
	if len(logs) != 2 || logs[0] != "hello, ada" || logs[1] != "ping:1" {
		t.Fatalf("JS 日志不正确: %v", logs)
	}

	// waterfall:JS 中间件把输入 +1,Go 侧下游再翻倍 → 4。
	pipelineRan := false
	a.Root().OnWaterfall("ts/pipeline", func(_ context.Context, _ *kernel.Context, in any, next kernel.Next) (any, error) {
		pipelineRan = true
		return next(toInt(in) * 2)
	})
	got, err := a.Root().Waterfall(context.Background(), "ts/pipeline", 1)
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	if !pipelineRan || toInt(got) != 4 {
		t.Fatalf("waterfall 结果应为 4 且下游应执行,实际 %v(ran=%v)", got, pipelineRan)
	}

	// 否决:JS 不调 next,Go 侧下游不得执行。
	vetoRan := false
	a.Root().OnWaterfall("ts/veto", func(_ context.Context, _ *kernel.Context, in any, next kernel.Next) (any, error) {
		vetoRan = true
		return next(in)
	})
	out, err := a.Root().Waterfall(context.Background(), "ts/veto", 1)
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	if out != "blocked" || vetoRan {
		t.Fatalf("veto 应短路且下游不执行,实际 %v(ran=%v)", out, vetoRan)
	}

	if err := f.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	// effect 逆序执行:后登记的 "second" 先清理。
	logs = p.Logs()
	if len(logs) != 4 || logs[2] != "cleanup:second" || logs[3] != "cleanup:first" {
		t.Fatalf("清理顺序不正确: %v", logs)
	}
	if f.State() != kernel.Disposed {
		t.Fatalf("卸载后应为 disposed,实际 %s", f.State())
	}
}

// 2. 模块没有 apply 导出时,fiber 必须 failed,而不是静默「成功」。
// 1b. effect 清理抛错必须上报到 fiber 卸载结果,不能静默吞掉。
func TestJSEffectErrorSurfacesOnDispose(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { effect: (l: string, f: () => void) => void }
export function apply(ctx: Ctx): void {
  ctx.effect("boom", () => { throw new Error("cleanup failed") })
}`,
	})
	p, err := bridge.New(bridge.Options{Name: "bad-cleanup", Artifact: art})
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}
	a := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	f, err := a.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if f.State() != kernel.Active {
		t.Fatalf("JS 插件应 active,实际 %s", f.State())
	}
	if err := f.Dispose(context.Background()); err == nil || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("清理失败应上报,实际 %v", err)
	}
}

// 2. 模块没有 apply 导出时,fiber 必须 failed,而不是静默「成功」。
func TestJSPluginWithoutApplyFails(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `export const x = 1`,
	})
	p, err := bridge.New(bridge.Options{Name: "no-apply", Artifact: art})
	if err != nil {
		t.Fatalf("构造插件失败: %v", err)
	}
	a := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	f, err := a.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载本身不应失败: %v", err)
	}
	if f.State() != kernel.Failed {
		t.Fatalf("缺 apply 时应 failed,实际 %s", f.State())
	}
}
