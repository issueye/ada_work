package kernel_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// 本文件是缺陷回归:每条用例对应一个曾经真实存在、且已经复现过的行为。
// 它们比「功能测试」更重要——功能测试验证新能力,回归测试防止旧伤口重新裂开。

func noopApply(*kernel.Context, kernel.Config) (kernel.Dispose, error) { return nil, nil }

func mustPlugin(t *testing.T, a *kernel.App, p kernel.Plugin) *kernel.Fiber {
	t.Helper()
	f, err := a.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
	}
	return f
}

// 1. waterfall 超时后,下游绝不允许执行(曾经的「幽灵执行」)。
func TestWaterfallTimeoutBlocksDownstream(t *testing.T) {
	a := newApp(t)
	root := a.Root()

	var mu sync.Mutex
	var downstream []string
	root.OnWaterfall("x/flow", func(ctx context.Context, c *kernel.Context, in any, next kernel.Next) (any, error) {
		<-ctx.Done() // 超时后监听器被唤醒,此时它已经「迟到」
		if _, err := next(in); err == nil {
			t.Errorf("超时后调用 next 应被拒绝")
		}
		return nil, ctx.Err()
	}, kernel.WithTimeout(10*time.Millisecond))
	root.OnWaterfall("x/flow", func(_ context.Context, c *kernel.Context, in any, next kernel.Next) (any, error) {
		mu.Lock()
		downstream = append(downstream, "ran")
		mu.Unlock()
		return next(in)
	})

	if _, err := root.Waterfall(context.Background(), "x/flow", 1); !kernel.IsCode(err, kernel.CodeTimeout) {
		t.Fatalf("应返回 ETIMEOUT,实际 %v", err)
	}
	// 给被放弃的监听器足够时间尝试进入下游。
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(downstream) != 0 {
		t.Fatalf("超时后下游不得执行,实际执行 %d 次", len(downstream))
	}
}

// 2. 监听器能拿到本次派发的 ctx,并在超时时收到取消信号。
func TestListenerSeesCanceledContext(t *testing.T) {
	a := newApp(t)
	seen := make(chan error, 1)
	a.Root().On("x/slow", func(ctx context.Context, c *kernel.Context, args []any) (any, error) {
		<-ctx.Done()
		seen <- ctx.Err()
		return nil, ctx.Err()
	}, kernel.WithTimeout(10*time.Millisecond))

	if err := a.Root().Serial(context.Background(), "x/slow"); !kernel.IsCode(err, kernel.CodeTimeout) {
		t.Fatalf("应返回 ETIMEOUT,实际 %v", err)
	}
	select {
	case err := <-seen:
		if err == nil {
			t.Fatalf("监听器应观察到 ctx 已取消")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("监听器未收到取消信号")
	}
}

// 3. 卸载一个消费者,不得牵连与它共享依赖的兄弟插件。
func TestDisposeDoesNotTouchSiblings(t *testing.T) {
	a := newApp(t)
	mustPlugin(t, a, kernel.Func("a", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("a", 1)
		return nil, err
	}).WithProvide("a"))

	var applies int32
	p1 := mustPlugin(t, a, kernel.Func("p1", noopApply).WithInject("a"))
	p2 := mustPlugin(t, a, kernel.Func("p2", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) {
		atomic.AddInt32(&applies, 1)
		return nil, nil
	}).WithInject("a"))

	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("p2 初始应只 Apply 一次,实际 %d", got)
	}
	if err := p1.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载 p1 失败: %v", err)
	}
	if got := atomic.LoadInt32(&applies); got != 1 {
		t.Fatalf("卸载 p1 不应让兄弟 p2 重跑 Apply,实际 %d 次", got)
	}
	if st := p2.State(); st != kernel.Active {
		t.Fatalf("p2 应保持 active,实际 %s", st)
	}
}

// 4. 提供者卸载时,依赖它的消费者必须先停机(方向正确的依赖通知)。
func TestProviderDisposeStopsConsumer(t *testing.T) {
	a := newApp(t)
	prov := mustPlugin(t, a, kernel.Func("prov", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("s", "v")
		return nil, err
	}).WithProvide("s"))
	cons := mustPlugin(t, a, kernel.Func("cons", noopApply).WithInject("s"))

	if cons.State() != kernel.Active {
		t.Fatalf("消费者应 active,实际 %s", cons.State())
	}
	if err := prov.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载提供者失败: %v", err)
	}
	if st := cons.State(); st != kernel.Pending {
		t.Fatalf("提供者下线后消费者应 pending,实际 %s", st)
	}
}

// 5. 失败的插件必须能通过 Reload 复活(曾经被关闭的 scope 永久卡死)。
func TestFailedFiberCanRecover(t *testing.T) {
	a := newApp(t)
	attempt := 0
	f := mustPlugin(t, a, kernel.Func("flaky", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		attempt++
		if attempt == 1 {
			return nil, kernel.Fail(kernel.CodeInvalid, "第一次故意失败")
		}
		_, err := ctx.Provide("s", "ok")
		return nil, err
	}).WithProvide("s"))

	if f.State() != kernel.Failed {
		t.Fatalf("首次装配应 failed,实际 %s", f.State())
	}
	if err := f.Reload(context.Background()); err != nil {
		t.Fatalf("Reload 应能让失败插件复活,实际 %v", err)
	}
	if f.State() != kernel.Active {
		t.Fatalf("重试后应 active,实际 %s", f.State())
	}
	if v, ok := a.Root().TryService("s"); !ok || v != "ok" {
		t.Fatalf("重试后服务应可用,实际 %v(%v)", v, ok)
	}
}

// 6. 卸载时副作用回收失败必须上报,且不能中断其余副作用的回收。
func TestDisposeReportsDisposerFailure(t *testing.T) {
	a := newApp(t)
	var order []string
	f := mustPlugin(t, a, kernel.Func("p", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		if _, err := ctx.Effect("好", func() (kernel.Dispose, error) {
			return func(context.Context) error { order = append(order, "好"); return nil }, nil
		}); err != nil {
			return nil, err
		}
		_, err := ctx.Effect("坏", func() (kernel.Dispose, error) {
			return func(context.Context) error { panic("boom") }, nil
		})
		return nil, err
	}))

	err := f.Dispose(context.Background())
	if !kernel.IsCode(err, kernel.CodePanic) {
		t.Fatalf("disposer panic 应上报 EPANIC,实际 %v", err)
	}
	if len(order) != 1 || order[0] != "好" {
		t.Fatalf("其余副作用仍应回收,实际 %v", order)
	}
}

// 7. 卸载后的 fiber 不得残留在注册表里(曾经无限增长)。
func TestDisposedFiberLeavesRegistry(t *testing.T) {
	a := newApp(t)
	p := kernel.Func("p", noopApply)
	for i := 0; i < 20; i++ {
		f := mustPlugin(t, a, p)
		if err := f.Dispose(context.Background()); err != nil {
			t.Fatalf("第 %d 次卸载失败: %v", i, err)
		}
	}
	if got := len(a.Inspect().Fibers); got != 0 {
		t.Fatalf("卸载后不应残留 fiber,实际 %d 个", got)
	}
}

// 8. Intercept 合并必须「越近覆盖越远」。
func TestInterceptNearestWins(t *testing.T) {
	a := newApp(t)
	outer := a.Root().Intercept("svc", kernel.ServiceConfig{"k": "外层", "o": 1})
	inner := outer.Intercept("svc", kernel.ServiceConfig{"k": "内层"})

	if got := inner.ServiceConfig("svc"); got["k"] != "内层" {
		t.Fatalf("最近的作用域应覆盖外层,实际 %v", got)
	}
	if got := inner.ServiceConfig("svc"); got["o"] != 1 {
		t.Fatalf("外层独有的键应保留,实际 %v", got)
	}
}

// 9. 20 个插件按依赖拓扑激活:挂载顺序任意,激活顺序由依赖决定(kernel-api §5 DoD)。
func TestTwentyPluginTopology(t *testing.T) {
	a := newApp(t)
	const n = 20
	var order []int
	plugins := make([]kernel.Plugin, 0, n)
	for i := 0; i < n; i++ {
		i := i
		svc := fmt.Sprintf("svc%02d", i)
		p := kernel.Func(fmt.Sprintf("p%02d", i), func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
			order = append(order, i)
			_, err := ctx.Provide(svc, i)
			return nil, err
		}).WithProvide(svc)
		if i > 0 {
			p = p.WithInject(fmt.Sprintf("svc%02d", i-1))
		}
		plugins = append(plugins, p)
	}
	// 逆序挂载:激活顺序必须由依赖决定,而不是声明顺序。
	for i := n - 1; i >= 0; i-- {
		mustPlugin(t, a, plugins[i])
	}
	if len(order) != n {
		t.Fatalf("应激活 %d 个插件,实际 %d", n, len(order))
	}
	for i, got := range order {
		if got != i {
			t.Fatalf("第 %d 个激活的应是 p%02d,实际 p%02d", i, i, got)
		}
	}
}

// 10. pending 的错误必须点名「在等哪个服务」,否则只能靠猜。
func TestPendingErrorNamesMissingService(t *testing.T) {
	a := newApp(t)
	f := mustPlugin(t, a, kernel.Func("needs-missing", noopApply).WithInject("missing-svc"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := f.Ready(ctx)
	if !kernel.IsCode(err, kernel.CodePending) {
		t.Fatalf("应返回 EPENDING,实际 %v", err)
	}
	if !strings.Contains(err.Error(), "missing-svc") {
		t.Fatalf("pending 错误应点名缺失的服务,实际 %v", err)
	}
}

// 11. Stop 之后事件总线封口,不再派发;注册表也清空。
func TestStopSealsEventsAndRegistry(t *testing.T) {
	a := newApp(t)
	hits := 0
	a.Root().On("x/y", func(context.Context, *kernel.Context, []any) (any, error) {
		hits++
		return nil, nil
	})
	mustPlugin(t, a, kernel.Func("p", noopApply))

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	a.Root().Emit("x/y")
	if hits != 0 {
		t.Fatalf("Stop 后不应再派发事件,实际 %d 次", hits)
	}
	if got := len(a.Inspect().Fibers); got != 0 {
		t.Fatalf("Stop 后注册表应清空,实际 %d 个 fiber", got)
	}
}

// 12. Inspect 必须能看到 effect 登记:没有 inspect 的能力等于没有交付(P9)。
func TestInspectShowsEffects(t *testing.T) {
	a := newApp(t)
	f := mustPlugin(t, a, kernel.Func("p", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		ctx.On("x/y", func(context.Context, *kernel.Context, []any) (any, error) { return nil, nil })
		if _, err := ctx.Effect("res", func() (kernel.Dispose, error) {
			return func(context.Context) error { return nil }, nil
		}); err != nil {
			return nil, err
		}
		_, err := ctx.Provide("svc", 1)
		return nil, err
	}))

	labels := map[string]bool{}
	for _, e := range a.Inspect().Effects {
		if e.Fiber != f.ID() {
			t.Fatalf("effect 归属错误: %+v", e)
		}
		labels[e.Label] = true
	}
	for _, want := range []string{"listener", "res", "provide:svc"} {
		if !labels[want] {
			t.Fatalf("快照缺少 effect %q: %v", want, labels)
		}
	}

	if err := f.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if got := len(a.Inspect().Effects); got != 0 {
		t.Fatalf("卸载后不应残留 effect 快照,实际 %d", got)
	}
}
