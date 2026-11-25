// 命令 ada-demo 演示 M1 内核的装配、依赖门控、事件与重载。
// 它只依赖标准库与 kernel 包，用来验证「Go 插件也能按 Cordis 语义组合」。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	app := kernel.New(kernel.Options{Logger: logger})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = app.Stop(ctx)
	}()

	// 三个插件：greeting 提供服务，upper 依赖它，presenter 依赖 upper 并订阅事件。
	// 挂载顺序故意反过来，验证依赖决定激活顺序而不是声明顺序。
	greeting := kernel.Func("greeting", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		prefix := cfg.String("prefix")
		if prefix == "" {
			prefix = "你好"
		}
		fmt.Printf("[greeting] 提供 greeting，前缀=%q\n", prefix)
		_, err := ctx.Provide("greeting", prefix)
		return func(context.Context) error {
			fmt.Println("[greeting] 已卸载")
			return nil
		}, err
	}).WithProvide("greeting").
		WithSchema(schemaWithPrefix())

	upper := kernel.Func("upper", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		base, err := kernel.Service[string](ctx, "greeting")
		if err != nil {
			return nil, err
		}
		fmt.Println("[upper] 拿到 greeting，提供 upper")
		_, err = ctx.Provide("upper", base+"（已转大写）")
		return nil, err
	}).WithInject("greeting").WithProvide("upper")

	presenter := kernel.Func("presenter", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		text, err := kernel.Service[string](ctx, "upper")
		if err != nil {
			return nil, err
		}
		fmt.Println("[presenter] 渲染:", text)
		ctx.On("demo/visit", func(_ context.Context, c *kernel.Context, args []any) (any, error) {
			fmt.Println("[presenter] 收到访问事件，参数:", args)
			return nil, nil
		})
		_, err = ctx.Effect("订阅", func() (kernel.Dispose, error) {
			return func(context.Context) error {
				fmt.Println("[presenter] 退订")
				return nil
			}, nil
		})
		return nil, err
	}).WithInject("upper")

	for _, p := range []kernel.Plugin{presenter, upper, greeting} {
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			fmt.Println("挂载失败:", err)
			return
		}
		fmt.Printf("挂载 %s → %s\n", p.Name(), f.State())
	}

	app.Root().Emit("demo/visit", "home")

	fmt.Println("\n== 装配快照 ==")
	show(app.Inspect())

	// 重载提供者：应触发 upper 与 presenter 级联重载。
	provider, ok := find(app.Inspect().Fibers, "greeting")
	if !ok {
		fmt.Println("找不到 greeting")
		return
	}
	gf, _ := app.Fiber(provider)
	if err := gf.Reload(context.Background()); err != nil {
		fmt.Println("重载失败:", err)
	}
	fmt.Println("\n== 重载 greeting 后 ==")
	show(app.Inspect())
}

// schemaWithPrefix 用内联 JSON 定义一个只有 prefix 的配置。
func schemaWithPrefix() *schema.Schema {
	s, err := schema.Parse([]byte(`{
	  "type": "object",
	  "properties": { "prefix": { "type": "string", "default": "你好" } },
	  "additionalProperties": false
	}`))
	if err != nil {
		panic(err)
	}
	return s
}

func find(fibers []kernel.FiberInfo, plugin string) (string, bool) {
	for _, f := range fibers {
		if f.Plugin == plugin {
			return f.ID, true
		}
	}
	return "", false
}

func show(s kernel.Snapshot) {
	for _, f := range s.Fibers {
		fmt.Printf("  fiber %-12s %-10s epoch=%d\n", f.Plugin, f.State, f.Epoch)
	}
	for _, svc := range s.Services {
		fmt.Printf("  service %-10s v%d provider=%-12s dependents=%d\n",
			svc.Name, svc.Version, svc.Provider, len(svc.Dependents))
	}
}
