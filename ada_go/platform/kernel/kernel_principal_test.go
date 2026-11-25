package kernel_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// 1. 插件上下文能给出调用者身份,并携带在 Context() 的 context 里。
func TestContextPrincipal(t *testing.T) {
	app := newApp(t)
	var (
		principal string
		fiberCtx  context.Context
	)
	if _, err := app.Plugin(kernel.Func("plug", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		principal = ctx.Principal()
		fiberCtx = ctx.Context()
		return nil, nil
	}), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	if principal != "plug" {
		t.Fatalf("Principal 不正确: %q", principal)
	}
	if id, ok := kernel.Principal(fiberCtx); !ok || id != "plug" {
		t.Fatalf("Context() 未携带身份: id=%q ok=%v", id, ok)
	}
	if app.Root().Principal() != "" {
		t.Fatalf("根上下文不应有身份")
	}
}

// 2. WithPrincipal/Principal 的往返与空值语义。
func TestWithPrincipal(t *testing.T) {
	ctx := context.Background()
	if _, ok := kernel.Principal(ctx); ok {
		t.Fatalf("空 context 不应有身份")
	}
	with := kernel.WithPrincipal(ctx, "p1")
	if id, ok := kernel.Principal(with); !ok || id != "p1" {
		t.Fatalf("身份未携带: id=%q ok=%v", id, ok)
	}
	if _, ok := kernel.Principal(kernel.WithPrincipal(ctx, "")); ok {
		t.Fatalf("空 id 不应设置身份")
	}
}
