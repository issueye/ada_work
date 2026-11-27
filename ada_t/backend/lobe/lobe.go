// Package lobeui 提供 lobe-mygo 视觉的独立渲染后端(ada_t backend.Backend)。
//
// 与 backend/mygoui 相同地把 widget 树重放为 mygo 原生控件,
// 但默认每帧应用 lobe-mygo Lobe UI 原生主题(明/暗跟随系统外观),
// 让 ada_t 应用开箱即用 lobe-mygo 的视觉令牌。
//
//	b := lobeui.New()
//	app, _ := adat.NewApp(pkg, adat.AppOptions{Backend: b})
//
// 需要扁平令牌传给插件时,用 ada_t/lobe.Theme.ApplyFlat(app, dark)。
package lobeui

import (
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/backend/mygoui"
	adalobe "github.com/issueye/ada_t/lobe"
	"github.com/issueye/ada_t/widget"
)

// Backend 是应用 lobe-mygo 视觉的 mygo 原生渲染后端。实现 backend.Backend。
type Backend struct {
	mu             sync.Mutex
	trees          map[string]*widget.Node
	windows        map[string]*mygo.Window
	renderers      map[string]*mygoui.Renderer
	dispatch       backend.DispatchFunc
	toastSource    func(winID string) []string
	theme          *adalobe.Theme
	themeOn        bool
	lobeComponents bool // widget 映射到 lobe-mygo 组件(默认开启);关闭则纯 mygoui 重放 + lobe 主题
}

// Option 配置 Backend。
type Option func(*Backend)

// WithTheme 指定 lobe-mygo 主题(默认 ada_t/lobe.Default())。
func WithTheme(t *adalobe.Theme) Option {
	return func(b *Backend) { b.theme = t }
}

// WithoutTheme 关闭 lobe-mygo 原生主题(保留 mygo 默认主题)。
func WithoutTheme() Option {
	return func(b *Backend) { b.themeOn = false }
}

// WithoutLobeComponents 关闭 widget→lobe-mygo 组件映射,退化为纯 mygoui 重放 + lobe 主题。
func WithoutLobeComponents() Option {
	return func(b *Backend) { b.lobeComponents = false }
}

// New 创建 lobe-mygo 视觉后端。
func New(opts ...Option) *Backend {
	b := &Backend{
		trees:          map[string]*widget.Node{},
		windows:        map[string]*mygo.Window{},
		renderers:      map[string]*mygoui.Renderer{},
		themeOn:        true,
		lobeComponents: true,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

// SetDispatch 注入事件回传(backend.Dispatcher)。
func (b *Backend) SetDispatch(fn backend.DispatchFunc) {
	b.mu.Lock()
	b.dispatch = fn
	b.mu.Unlock()
}

// SetToastSource 注入 toast 来源(backend.ToastSource)。
func (b *Backend) SetToastSource(fn func(winID string) []string) {
	b.mu.Lock()
	b.toastSource = fn
	b.mu.Unlock()
}

// Apply 提交窗口最新树;tree 为 nil 表示窗口已移除(契约 backend.Backend)。
func (b *Backend) Apply(winID string, tree *widget.Node) {
	b.mu.Lock()
	if tree == nil {
		delete(b.trees, winID)
		delete(b.renderers, winID)
	} else {
		b.trees[winID] = tree
	}
	w := b.windows[winID]
	if tree == nil {
		delete(b.windows, winID)
	}
	b.mu.Unlock()
	if tree == nil && w != nil {
		w.Close()
		return
	}
	if w != nil {
		w.Invalidate()
	}
}

// Run 启动 mygo 应用并阻塞(占主线程);windows 是启动窗口列表(契约 backend.Backend)。
func (b *Backend) Run(wins []backend.WindowSpec) error {
	fire := func(winID, hid string, payload any) {
		b.mu.Lock()
		fn := b.dispatch
		b.mu.Unlock()
		if fn != nil {
			fn(winID, hid, payload)
		}
	}

	mygo.App.WhenReady(func() {
		for _, w := range wins {
			id := w.ID
			ren := mygoui.NewRenderer()
			ren.SetDispatch(func(winID, hid string, payload any) { fire(id, hid, payload) })
			win := mygo.NewWindow(mygo.WindowOptions{
				Title:           w.Title,
				Width:           w.Width,
				Height:          w.Height,
				BackgroundColor: w.BackgroundColor,
				Content: ui.View(func(c *ui.Context) {
					b.mu.Lock()
					tree := b.trees[id]
					src := b.toastSource
					themeOn := b.themeOn
					lobeComponents := b.lobeComponents
					theme := b.theme
					b.mu.Unlock()
					if themeOn {
						// lobe-mygo 原生主题:帧起始应用,明/暗跟随系统外观。
						if theme == nil {
							theme = adalobe.Default()
						}
						theme.ApplyNative(c)
					}
					if tree == nil {
						ui.Text(c, "…").Center()
						return
					}
					if lobeComponents {
						renderLobeNode(c, tree, id, func(winID, hid string, payload any) { fire(id, hid, payload) }, ren)
					} else {
						ren.Render(c, id, tree)
					}
					if src != nil {
						for _, msg := range src(id) {
							c.Toast(msg)
						}
					}
				}),
			})
			b.mu.Lock()
			b.windows[id] = win
			b.renderers[id] = ren
			b.mu.Unlock()
		}
	})
	return mygo.App.Run()
}

// Stop 请求退出 mygo 主循环(契约 backend.Backend)。
func (b *Backend) Stop() error {
	mygo.App.Quit()
	return nil
}

// 编译期确认契约。
var (
	_ backend.Backend     = (*Backend)(nil)
	_ backend.Dispatcher  = (*Backend)(nil)
	_ backend.ToastSource = (*Backend)(nil)
)
