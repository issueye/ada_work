package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

type wfInvoke func(ctx context.Context, input any, next func(any) (any, error)) (any, error)

func waterfallRuntime(t *testing.T, src string) (*jsrt.Runtime, map[string]wfInvoke) {
	t.Helper()
	art := newArtifact(t, map[string]string{"plugin.ts": src})
	invokes := map[string]wfInvoke{}
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			Waterfall: func(event string, invoke func(context.Context, any, func(any) (any, error)) (any, error)) error {
				invokes[event] = invoke
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	if _, err := rt.CallWithHost(context.Background(), "run"); err != nil {
		t.Fatalf("调用 run 失败: %v", err)
	}
	return rt, invokes
}

// 12. ctx.waterfall:JS 中间件能调用 next 拿到下游结果,也能不调 next 直接短路。
func TestInvokeWaterfall(t *testing.T) {
	_, invokes := waterfallRuntime(t, `type Ctx = { waterfall: (e: string, h: (x: any, next: (v: any) => any) => any) => void }
export function run(ctx: Ctx) {
  ctx.waterfall("flow", (x, next) => next(x + 1))
  ctx.waterfall("veto", (x, next) => "blocked")
}`)

	flow, ok := invokes["flow"]
	if !ok {
		t.Fatalf("flow 未注册")
	}
	got, err := flow(context.Background(), 1, func(v any) (any, error) { return v, nil })
	if err != nil {
		t.Fatalf("调用 flow 失败: %v", err)
	}
	if asInt(t, got) != 2 {
		t.Fatalf("flow 应返回 2,实际 %v", got)
	}

	veto, ok := invokes["veto"]
	if !ok {
		t.Fatalf("veto 未注册")
	}
	called := false
	out, err := veto(context.Background(), 1, func(v any) (any, error) {
		called = true
		return v, nil
	})
	if err != nil {
		t.Fatalf("调用 veto 失败: %v", err)
	}
	if out != "blocked" || called {
		t.Fatalf("veto 应短路且不调用 next,实际 out=%v called=%v", out, called)
	}
}

// 13. 重复调用 next 必须报错(否则会重复执行下游副作用)。
func TestWaterfallDoubleNextFails(t *testing.T) {
	_, invokes := waterfallRuntime(t, `type Ctx = { waterfall: (e: string, h: (x: any, next: (v: any) => any) => any) => void }
export function run(ctx: Ctx) {
  ctx.waterfall("bad", (x, next) => { next(x); next(x); return 0 })
}`)
	bad := invokes["bad"]
	if bad == nil {
		t.Fatalf("bad 未注册")
	}
	if _, err := bad(context.Background(), 1, func(v any) (any, error) { return v, nil }); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复调用 next 应报错,实际 %v", err)
	}
}

// 13.1 JS 中间件改写「指向结构体的指针」输入时,返回值必须还原成同一类型,
// 否则宿主的 `v.(*T)` 断言会失败,改写被静默丢弃。
type probeInput struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func TestWaterfallCoercesStructPointer(t *testing.T) {
	_, invokes := waterfallRuntime(t, `type Ctx = { waterfall: (e: string, h: (x: any, next: (v: any) => any) => any) => void }
export function run(ctx: Ctx) {
  ctx.waterfall("struct", (x, next) => { const out = next(x); out.value = out.value + "-js"; out.count = out.count + 1; return out })
  ctx.waterfall("short", (x, _next) => { x.value = x.value + "-short"; return x })
}`)

	next := func(v any) (any, error) { return v, nil }
	in := &probeInput{Value: "go", Count: 1}
	got, err := invokes["struct"](context.Background(), in, next)
	if err != nil {
		t.Fatalf("调用 struct 失败: %v", err)
	}
	p, ok := got.(*probeInput)
	if !ok {
		t.Fatalf("返回值应还原成 *probeInput,实际 %T", got)
	}
	if p.Value != "go-js" || p.Count != 2 {
		t.Fatalf("JS 改写未生效: %+v", p)
	}

	got, err = invokes["short"](context.Background(), &probeInput{Value: "go", Count: 1}, next)
	if err != nil {
		t.Fatalf("调用 short 失败: %v", err)
	}
	p, ok = got.(*probeInput)
	if !ok || p.Value != "go-short" {
		t.Fatalf("短路返回也应还原类型: %T %+v", got, got)
	}
}

// 14. 同一运行时在等待 next 时被再次调用必然死锁,必须快速失败。
func TestWaterfallReentrantFails(t *testing.T) {
	_, invokes := waterfallRuntime(t, `type Ctx = { waterfall: (e: string, h: (x: any, next: (v: any) => any) => any) => void }
export function run(ctx: Ctx) {
  ctx.waterfall("inner", (x, next) => next(x))
  ctx.waterfall("outer", (x, next) => next(x))
}`)
	outer, inner := invokes["outer"], invokes["inner"]
	if outer == nil || inner == nil {
		t.Fatalf("处理器未注册")
	}
	// next 里再次进入同一个运行时,模拟「链上游回调又调回自己」。
	_, err := outer(context.Background(), 1, func(v any) (any, error) {
		return inner(context.Background(), v, func(x any) (any, error) { return x, nil })
	})
	if err == nil || !strings.Contains(err.Error(), "嵌套") {
		t.Fatalf("同运行时嵌套应返回 ErrReentrant,实际 %v", err)
	}
}
