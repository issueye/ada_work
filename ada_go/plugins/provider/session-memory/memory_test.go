package memory_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/session/sessiontest"
)

// 契约测试:provider 必须全绿才能进 preset(seam-api §1.6)。
func TestConformance(t *testing.T) {
	provider := memory.New()
	sessiontest.Run(t, sessiontest.Options{
		New: func(t *testing.T) session.Session {
			s, err := provider.Open(context.Background(), session.Meta{Title: t.Name()})
			if err != nil {
				t.Fatalf("打开会话失败: %v", err)
			}
			return s
		},
		SupportsFork:      true,
		SupportsCompact:   true,
		SupportsSubscribe: true,
	})
}

// 集成:seam 插件提供 session 服务,provider 注册进来,消费者从 Registry 打开会话。
func TestSeamAndProviderThroughKernel(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	if _, err := app.Plugin(session.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	providerFiber, err := app.Plugin(memory.Plugin(), kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 provider 失败: %v", err)
	}
	if providerFiber.State() != kernel.Active {
		// provider 依赖 seam 服务:seam 已挂载,所以这里必须 active。
		t.Fatalf("provider 应 active,实际 %s", providerFiber.State())
	}

	var opened session.Session
	consumer := kernel.Func("session-consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*session.Registry](ctx, "session")
		if err != nil {
			return nil, err
		}
		opened, err = registry.Open(context.Background(), "", session.Meta{Title: "demo"})
		return nil, err
	}).WithInject("session")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if opened == nil {
		t.Fatalf("消费者没有拿到会话")
	}
	if _, err := opened.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	events, err := opened.Events(0, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("读取失败: %v %d", err, len(events))
	}

	// 卸载 provider 后,注册表里不应再有实现。
	if err := providerFiber.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载 provider 失败: %v", err)
	}
	registry, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatalf("取 seam 服务失败: %v", err)
	}
	if _, err := registry.Provider(""); err == nil {
		t.Fatalf("卸载 provider 后不应还有可用实现")
	}
}

// 没有 provider 时,Registry 必须返回 EUNIMPLEMENTED 而不是让消费者 pending。
func TestRegistryWithoutProvider(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(session.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}

	registry, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatalf("取 seam 服务失败: %v", err)
	}
	if _, err := registry.Open(context.Background(), "", session.Meta{}); err == nil {
		t.Fatalf("无 provider 时应返回错误")
	}
}
