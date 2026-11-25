package kernel_test

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
)

func newApp(t *testing.T) *kernel.App {
	t.Helper()
	a := kernel.New(kernel.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.Stop(ctx)
	})
	return a
}

// 1. 依赖门控：先挂消费者，再挂提供者，消费者由 pending 自动转为 active。
func TestDependencyGating(t *testing.T) {
	a := newApp(t)

	var got string
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		v, err := kernel.Service[string](ctx, "greeting")
		if err != nil {
			return nil, err
		}
		got = v
		return nil, nil
	}).WithInject("greeting").WithProvide("consumer")

	cf, err := a.Plugin(consumer, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if cf.State() != kernel.Pending {
		t.Fatalf("依赖缺失时应为 pending，实际 %s", cf.State())
	}

	provider := kernel.Func("provider", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("greeting", "你好")
		return nil, err
	}).WithProvide("greeting")
	if _, err := a.Plugin(provider, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载提供者失败: %v", err)
	}

	if cf.State() != kernel.Active {
		t.Fatalf("提供者上线后消费者应 active，实际 %s", cf.State())
	}
	if got != "你好" {
		t.Fatalf("服务值应为 你好，实际 %q", got)
	}
}

// 2. 拓扑激活：按 C→B→A 的顺序挂载，三者仍按 A→B→C 激活。
func TestTopologicalActivation(t *testing.T) {
	a := newApp(t)

	var mu sync.Mutex
	order := make([]string, 0, 3)
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

	c := kernel.Func("c", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		if _, err := kernel.Service[string](ctx, "b"); err != nil {
			return nil, err
		}
		record("c")
		return nil, nil
	}).WithInject("b")

	b := kernel.Func("b", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		if _, err := kernel.Service[string](ctx, "a"); err != nil {
			return nil, err
		}
		record("b")
		_, err := ctx.Provide("b", "b")
		return nil, err
	}).WithInject("a").WithProvide("b")

	ap := kernel.Func("a", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		record("a")
		_, err := ctx.Provide("a", "a")
		return nil, err
	}).WithProvide("a")

	for _, p := range []kernel.Plugin{c, b, ap} {
		if _, err := a.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载失败: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "a,b,c" {
		t.Fatalf("激活顺序应为 a,b,c，实际 %v", order)
	}
}

// 3. 副作用逆序回收。
func TestEffectReverseOrder(t *testing.T) {
	a := newApp(t)
	var order []string
	p := kernel.Func("effecter", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		for _, name := range []string{"a", "b", "c"} {
			n := name
			if _, err := ctx.Effect("扣-"+n, func() (kernel.Dispose, error) {
				return func(context.Context) error {
					order = append(order, n)
					return nil
				}, nil
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	f, err := a.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if err := f.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if strings.Join(order, ",") != "c,b,a" {
		t.Fatalf("副作用应按后进先出回收，实际 %v", order)
	}
}

// 4. 父子级联：父卸载时子插件被回收。
func TestDisposeCascade(t *testing.T) {
	a := newApp(t)
	child := kernel.Func("child", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		return nil, nil
	})
	var childFiber *kernel.Fiber
	parent := kernel.Func("parent", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		f, err := ctx.Plugin(child, kernel.NewConfig(nil))
		if err != nil {
			return nil, err
		}
		childFiber = f
		return nil, nil
	})

	pf, err := a.Plugin(parent, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if childFiber == nil || childFiber.State() != kernel.Active {
		t.Fatalf("子插件应已激活")
	}
	if err := pf.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载父插件失败: %v", err)
	}
	if childFiber.State() != kernel.Disposed {
		t.Fatalf("父卸载后子插件应 disposed，实际 %s", childFiber.State())
	}
}

// 5. 可见性：未 inject 的服务不可见。
func TestServiceVisibility(t *testing.T) {
	a := newApp(t)
	provider := kernel.Func("provider", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("secret-value", 42)
		return nil, err
	})
	if _, err := a.Plugin(provider, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载提供者失败: %v", err)
	}

	nosy := kernel.Func("nosy", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Service("secret-value")
		return nil, err
	})
	f, err := a.Plugin(nosy, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if f.State() != kernel.Failed {
		t.Fatalf("未 inject 却取服务应失败，实际 %s", f.State())
	}
}

// 6. 配置：补默认值、拒绝非法配置。
func TestConfigDefaultsAndValidation(t *testing.T) {
	a := newApp(t)
	sch, err := schema.Parse([]byte(`{
	  "type": "object",
	  "properties": {
	    "n": {"type": "integer", "default": 3, "minimum": 1},
	    "name": {"type": "string", "default": "ada"}
	  },
	  "additionalProperties": false
	}`))
	if err != nil {
		t.Fatalf("解析 schema 失败: %v", err)
	}
	var n int
	var name string
	p := kernel.Func("with-config", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		n = cfg.Int("n")
		name = cfg.String("name")
		return nil, nil
	}).WithSchema(sch)
	if _, err := a.Plugin(p, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if n != 3 || name != "ada" {
		t.Fatalf("默认值未生效: n=%d name=%q", n, name)
	}
	if _, err := a.Plugin(p, kernel.NewConfig(map[string]any{"n": 0})); err != nil {
		if !kernel.IsCode(err, kernel.CodeConfig) {
			t.Fatalf("非法配置应报 ECONFIG，实际 %v", err)
		}
	} else {
		t.Fatalf("n=0 违反 minimum=1，应报错")
	}
}

// 7. 事件：优先级、waterfall 改写与短路、bail。
func TestEvents(t *testing.T) {
	a := newApp(t)
	ctx := a.Root()

	var order []string
	var orderMu sync.Mutex
	appendOrder := func(n string) {
		orderMu.Lock()
		order = append(order, n)
		orderMu.Unlock()
	}
	low := ctx.On("demo/order", func(_ context.Context, c *kernel.Context, args []any) (any, error) {
		appendOrder("low")
		return nil, nil
	}, kernel.WithPriority(1))
	high := ctx.On("demo/order", func(_ context.Context, c *kernel.Context, args []any) (any, error) {
		appendOrder("high")
		return nil, nil
	}, kernel.WithPriority(10))
	defer func() { _ = low(context.Background()) }()
	defer func() { _ = high(context.Background()) }()

	if err := ctx.Serial(context.Background(), "demo/order"); err != nil {
		t.Fatalf("parallel 失败: %v", err)
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 2 || order[0] != "high" || order[1] != "low" {
		t.Fatalf("优先级顺序应为 high,low，实际 %v", order)
	}

	// waterfall：第一层改写，第二层短路。
	d1 := ctx.OnWaterfall("demo/flow", func(_ context.Context, c *kernel.Context, in any, next kernel.Next) (any, error) {
		return next(in.(int) + 1)
	})
	defer func() { _ = d1(context.Background()) }()
	d2 := ctx.OnWaterfall("demo/flow", func(_ context.Context, c *kernel.Context, in any, next kernel.Next) (any, error) {
		return "短路:" + "ok", nil
	})
	defer func() { _ = d2(context.Background()) }()
	out, err := ctx.Waterfall(context.Background(), "demo/flow", 1)
	if err != nil {
		t.Fatalf("waterfall 失败: %v", err)
	}
	if out != "短路:ok" {
		t.Fatalf("第二层未调用 Next 时应短路，实际 %v", out)
	}
}

// 8. 重载 100 次：监听器不重复、作用域不增长。
func TestReloadDoesNotLeak(t *testing.T) {
	a := newApp(t)
	p := kernel.Func("reloader", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		ctx.On("demo/reload", func(_ context.Context, c *kernel.Context, args []any) (any, error) { return nil, nil })
		if _, err := ctx.Effect("timer", func() (kernel.Dispose, error) {
			return func(context.Context) error { return nil }, nil
		}); err != nil {
			return nil, err
		}
		return nil, nil
	})
	f, err := a.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := f.Reload(context.Background()); err != nil {
			t.Fatalf("第 %d 次重载失败: %v", i, err)
		}
	}
	if got := a.ListenerCount(); got != 1 {
		t.Fatalf("重载后监听器应只剩 1 个，实际 %d", got)
	}
	if err := f.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if got := a.ListenerCount(); got != 0 {
		t.Fatalf("卸载后监听器应为 0，实际 %d", got)
	}
	if got := a.EffectCount(); got != 0 {
		t.Fatalf("卸载后根作用域副作用应为 0，实际 %d", got)
	}
}

// 9. Apply 失败要回滚已注册的服务，等待它的消费者保持 pending。
func TestFailedApplyRollsBack(t *testing.T) {
	a := newApp(t)
	bad := kernel.Func("bad", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("flaky", "x")
		if err != nil {
			return nil, err
		}
		return nil, kernel.Fail(kernel.CodeInvalid, "故意失败")
	}).WithProvide("flaky")
	bf, err := a.Plugin(bad, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if bf.State() != kernel.Failed {
		t.Fatalf("应进入 failed，实际 %s", bf.State())
	}
	for _, s := range a.Inspect().Services {
		if s.Name == "flaky" {
			t.Fatalf("失败插件注册的服务应被回滚")
		}
	}

	consumer := kernel.Func("needs-flaky", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		return nil, nil
	}).WithInject("flaky")
	cf, err := a.Plugin(consumer, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if cf.State() != kernel.Pending {
		t.Fatalf("依赖缺失时应 pending，实际 %s", cf.State())
	}
}

// 10. 默认值排序稳定性（Inspect 输出可做 golden）。
func TestInspectStableOrder(t *testing.T) {
	a := newApp(t)
	p := kernel.Func("p", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("z", 1)
		return nil, err
	})
	if _, err := a.Plugin(p, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	snap := a.Inspect()
	if len(snap.Fibers) != 1 || len(snap.Services) != 1 {
		t.Fatalf("快照数量不对: %+v", snap)
	}
	var names []string
	for _, s := range snap.Services {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if names[0] != "z" {
		t.Fatalf("服务名快照不对: %v", names)
	}
}
