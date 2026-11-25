// Package desktop 是 host/desktop 适配器的映射骨架:把内核快照映射成宿主视图。
//
// 它不引入任何窗口/UI 依赖,只定义 Renderer 接口;真正的 mygo 窗口由宿主实现。
package desktop

import (
	"context"
	"errors"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// Renderer 由桌面宿主实现:把 View 画到窗口。
type Renderer interface {
	Render(View) error
}

// FiberRow 是视图里的一个 fiber。
type FiberRow struct {
	ID     string
	Plugin string
	State  string
	Parent string
}

// ServiceRow 是视图里的一个服务。
type ServiceRow struct {
	Name       string
	Provider   string
	Version    int
	Dependents []string
}

// EffectRow 是一条副作用注册。
type EffectRow struct {
	Plugin string
	Label  string
}

// View 是渲染所需的只读视图模型。
type View struct {
	Fibers   []FiberRow
	Services []ServiceRow
	Effects  []EffectRow
	Events   []string
}

// EventPageOpen 是宿主页面切换事件:由 OpenPage 发出,供延迟激活消费。
const EventPageOpen = "page/open"

// Host 持有内核 App 与渲染器。
type Host struct {
	app      *kernel.App
	renderer Renderer
}

// New 构造桌面宿主映射;app 与 renderer 都不能为 nil。
func New(app *kernel.App, renderer Renderer) (*Host, error) {
	if app == nil {
		return nil, errors.New("desktop: app 不能为 nil")
	}
	if renderer == nil {
		return nil, errors.New("desktop: renderer 不能为 nil")
	}
	return &Host{app: app, renderer: renderer}, nil
}

// OpenPage 发出 page/open 事件(宿主触发页面切换);空页面名报错。
func (h *Host) OpenPage(name string) error {
	if name == "" {
		return errors.New("desktop: 页面名不能为空")
	}
	h.app.Root().Emit(EventPageOpen, name)
	return nil
}

// Subscribe 在根上下文上监听事件,事件到达时 Refresh 并重绘。
//
// 注意:重绘发生在事件派发回调里,渲染器应快速返回;返回的 Dispose 取消订阅。
func (h *Host) Subscribe(events []string) (kernel.Dispose, error) {
	if len(events) == 0 {
		return nil, errors.New("desktop: events 不能为空")
	}
	var mu sync.Mutex
	var disposers []kernel.Dispose
	for _, name := range events {
		if name == "" {
			for _, d := range disposers {
				_ = d(context.Background())
			}
			return nil, errors.New("desktop: 事件名不能为空")
		}
		eventName := name
		d := h.app.Root().On(eventName, func(_ context.Context, _ *kernel.Context, _ []any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			_, _ = h.Refresh()
			return nil, nil
		})
		disposers = append(disposers, d)
	}
	return func(ctx context.Context) error {
		var errs []error
		for i := len(disposers) - 1; i >= 0; i-- {
			if err := disposers[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}, nil
}

// Refresh 把 app.Inspect() 映射成 View 并交给渲染器;返回映射出的视图。
func (h *Host) Refresh() (*View, error) {
	snap := h.app.Inspect()
	view := &View{Events: snap.Events}
	for _, f := range snap.Fibers {
		view.Fibers = append(view.Fibers, FiberRow{ID: f.ID, Plugin: f.Plugin, State: f.State, Parent: f.Parent})
	}
	for _, s := range snap.Services {
		view.Services = append(view.Services, ServiceRow{
			Name: s.Name, Provider: s.Provider, Version: s.Version, Dependents: s.Dependents,
		})
	}
	for _, e := range snap.Effects {
		view.Effects = append(view.Effects, EffectRow{Plugin: e.Plugin, Label: e.Label})
	}
	if err := h.renderer.Render(*view); err != nil {
		return view, err
	}
	return view, nil
}
