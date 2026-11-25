package desktop_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/desktop"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

type fakeRenderer struct {
	views []desktop.View
}

func (f *fakeRenderer) Render(v desktop.View) error {
	f.views = append(f.views, v)
	return nil
}

func mountDemo(t *testing.T, app *kernel.App) {
	t.Helper()
	if _, err := app.Plugin(kernel.Func("demo", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		if _, err := ctx.Provide("svc", "demo"); err != nil {
			return nil, err
		}
		_, err := ctx.Effect("demo-effect", func() (kernel.Dispose, error) { return nil, nil })
		return nil, err
	}), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
}

// 1. 映射 fiber/service/effect 并交给渲染器。
func TestRefresh(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountDemo(t, app)
	r := &fakeRenderer{}
	h, err := desktop.New(app, r)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	view, err := h.Refresh()
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if len(view.Fibers) != 1 || view.Fibers[0].Plugin != "demo" {
		t.Fatalf("fibers 映射不正确: %+v", view.Fibers)
	}
	if len(view.Services) != 1 || view.Services[0].Name != "svc" {
		t.Fatalf("services 映射不正确: %+v", view.Services)
	}
	if len(view.Effects) < 1 || view.Effects[0].Plugin != "demo" {
		t.Fatalf("effects 映射不正确: %+v", view.Effects)
	}
	if len(r.views) != 1 {
		t.Fatalf("渲染器应被调用一次,实际 %d", len(r.views))
	}

	// 重复刷新幂等(视图内容一致)。
	if _, err := h.Refresh(); err != nil {
		t.Fatalf("第二次刷新失败: %v", err)
	}
	if len(r.views) != 2 {
		t.Fatalf("渲染器应被调用两次,实际 %d", len(r.views))
	}
}

// 2. 构造参数校验。
func TestNewValidation(t *testing.T) {
	if _, err := desktop.New(nil, &fakeRenderer{}); err == nil {
		t.Fatalf("nil app 应报错")
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := desktop.New(app, nil); err == nil {
		t.Fatalf("nil renderer 应报错")
	}
}

// 3. 订阅事件触发重绘;Dispose 后取消。
func TestSubscribe(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountDemo(t, app)
	r := &fakeRenderer{}
	h, err := desktop.New(app, r)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := h.Subscribe(nil); err == nil {
		t.Fatalf("空事件集应报错")
	}
	dispose, err := h.Subscribe([]string{seamfs.EventObserve})
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	app.Root().Emit(seamfs.EventObserve, map[string]any{"op": "read"})
	if len(r.views) != 1 {
		t.Fatalf("事件应触发一次重绘,实际 %d", len(r.views))
	}
	if err := dispose(context.Background()); err != nil {
		t.Fatalf("取消订阅失败: %v", err)
	}
	app.Root().Emit(seamfs.EventObserve, map[string]any{"op": "read"})
	if len(r.views) != 1 {
		t.Fatalf("取消后不应再重绘,实际 %d", len(r.views))
	}
}

// 4. OpenPage 发出页面切换事件。
func TestOpenPage(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	var got any
	app.Root().On(desktop.EventPageOpen, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			got = args[0]
		}
		return nil, nil
	})
	h, err := desktop.New(app, &fakeRenderer{})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := h.OpenPage("settings"); err != nil {
		t.Fatalf("OpenPage 失败: %v", err)
	}
	if got != "settings" {
		t.Fatalf("事件载荷不正确: %v", got)
	}
	if err := h.OpenPage(""); err == nil {
		t.Fatalf("空页面名应报错")
	}
}
