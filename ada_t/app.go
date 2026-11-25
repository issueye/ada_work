// Package adat 是「前端式开发 golang native GUI」的运行时编译层公共入口。
//
// 开发者用 .aui 单文件组件(模板 + TS setup + scoped 样式)描述界面,
// 本层在 Go 进程内完成:模板→RenderPlan、esbuild TS→ESM、moejs 里跑
// 细粒度响应式与 Plan 解释器,再由 Backend 重放为原生控件(mygo GPU 绘制
// 或 headless 快照)。架构与决策见 README.md。
//
// 用法:
//
//	pkg, err := compile.CompileDir("examples/counter", compile.Options{})
//	b := mygoui.New()
//	app, _ := adat.NewApp(pkg, adat.AppOptions{Backend: b})
//	app.Window(adat.WindowOptions{Title: "Counter", Width: 320, Height: 240})
//	app.Run()
package adat

import (
	"fmt"
	"sync"
	"time"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/inspector"
	"github.com/issueye/ada_t/internal/rt"
	"github.com/issueye/ada_t/widget"
)

// AppOptions 是应用参数。
type AppOptions struct {
	// Backend 必填:渲染后端(mygoui.New() 或 headless.New())。
	Backend backend.Backend
	// Logf 接收 JS/运行时日志;nil 丢弃。
	Logf func(format string, args ...any)
	// Locale 当前语言,空为 zh-CN。
	Locale string
	// AppI18n 应用级文案:locale → key → 文案(组件 i18n 优先)。
	AppI18n map[string]map[string]string
	// Inspector 开启内置调试面板:自动多开一个原生窗口,实时展示
	// 注册表统计与各窗口组件树(每 1.5 秒推送)。
	Inspector bool
	// Theme 是初始主题令牌(插件 ctx.theme() 读取;宿主可传 Lobe MyGo tokens)。
	Theme map[string]any
	// SlotMode 让 LoadPlugin 自动把槽位贡献挂载成 slot 视图(配合
	// backend/embed 或 headless;mygoui 会为每个视图新建原生窗口)。
	SlotMode bool
	// PluginTrust 是插件信任策略(摘要与签名)。
	PluginTrust PluginTrust
}

// WindowOptions 是窗口描述(宽高为 DIP)。
type WindowOptions struct {
	Title  string
	Width  int
	Height int
	// BackgroundColor 是窗口底色(CSS 语法 "#10151d");空为系统默认。
	// 原生 UI 在控件不绘制的地方露出它,深色主题需要与主题同色避免露白。
	BackgroundColor string
	// Component 是根组件名(Package 内);空则用入口组件。
	Component string
	// Props 传给根组件的初始 props。
	Props map[string]any
}

// App 是一个组件应用:编译产物 + JS 运行时 + 渲染后端。
type App struct {
	mu      sync.Mutex
	pkg     *compile.Package
	rt      *rt.Runtime
	backend backend.Backend
	opts    AppOptions

	windows []backend.WindowSpec
	winSeq  int

	slots []string   // 宿主注册的插槽名
	views []SlotView // 插件槽位视图(SlotMode)
	theme map[string]any

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewApp 创建应用并装载组件包(编译产物来自 compile.CompileDir/CompileFiles)。
func NewApp(pkg *compile.Package, opts AppOptions) (*App, error) {
	if opts.Backend == nil {
		return nil, fmt.Errorf("adat: 需要 Backend(mygoui.New() 或 headless.New())")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	rtOpts := rt.Options{
		Logf:    opts.Logf,
		Locale:  opts.Locale,
		AppI18n: opts.AppI18n,
		Theme:   opts.Theme,
	}
	a := &App{pkg: pkg, backend: opts.Backend, opts: opts, stopCh: make(chan struct{}), theme: opts.Theme}
	// 后端事件 → rt.Fire(原生事件被翻译成 handler 调用)
	if d, ok := opts.Backend.(backend.Dispatcher); ok {
		d.SetDispatch(func(winID, hid string, payload any) {
			if a.rt != nil {
				a.rt.Fire(hid, payload)
			}
		})
	}
	// 渲染树 → 后端
	rtOpts.OnTree = func(winID string, tree *widget.Node) {
		opts.Backend.Apply(winID, tree)
	}
	r, err := rt.New(pkg, rtOpts)
	if err != nil {
		return nil, err
	}
	// 组件 ctx.toast 入队的提示 → 后端重放帧里显示
	if ts, ok := opts.Backend.(backend.ToastSource); ok {
		ts.SetToastSource(r.DrainToasts)
	}
	a.rt = r

	// 内置 Inspector 调试面板:装载附加包并多开一个原生窗口,
	// 每 1.5 秒推送 stats 与各窗口组件树(排除自身避免递归)。
	if opts.Inspector {
		ipkg, err := inspector.Package()
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("adat: 装载 Inspector 组件失败: %w", err)
		}
		if err := r.LoadAdditional(ipkg); err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("adat: 注册 Inspector 失败: %w", err)
		}
		if _, err := r.MountWindow("inspector", "Inspector", nil); err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("adat: 挂载 Inspector 窗口失败: %w", err)
		}
		a.windows = append(a.windows, backend.WindowSpec{
			ID:     "inspector",
			Title:  "Ada Inspector",
			Width:  420,
			Height: 620,
		})
		go func() {
			t := time.NewTicker(1500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-a.stopCh:
					return
				case <-t.C:
				}
				wins := []map[string]any{}
				for _, id := range a.WindowIDs() {
					if id == "inspector" {
						continue
					}
					rows := r.FlatRows(id)
					if rows == nil {
						continue
					}
					wins = append(wins, map[string]any{"id": id, "rows": rows})
				}
				m, _ := r.Stats()
				a.Emit("__inspector", map[string]any{"stats": m, "windows": wins})
			}
		}()
	}
	return a, nil
}

// Window 注册并挂载一个窗口;在 Run 之前调用。
func (a *App) Window(opts WindowOptions) error {
	a.mu.Lock()
	component := opts.Component
	if component == "" {
		component = a.pkg.Entry
	}
	a.winSeq++
	winID := fmt.Sprintf("win%d", a.winSeq)
	a.mu.Unlock()
	if _, err := a.rt.MountWindow(winID, component, opts.Props); err != nil {
		return err
	}
	a.mu.Lock()
	a.windows = append(a.windows, backend.WindowSpec{
		ID:              winID,
		Title:           opts.Title,
		Width:           opts.Width,
		Height:          opts.Height,
		BackgroundColor: opts.BackgroundColor,
	})
	a.mu.Unlock()
	return nil
}

// Run 启动后端并阻塞;Close/Stop 或窗口全关时返回。
func (a *App) Run() error {
	a.mu.Lock()
	wins := append([]backend.WindowSpec(nil), a.windows...)
	a.mu.Unlock()
	err := a.backend.Run(wins)
	a.stopInspector()
	_ = a.rt.Close()
	return err
}

// Stop 停止应用(幂等)。
func (a *App) Stop() error {
	a.stopInspector()
	if err := a.backend.Stop(); err != nil {
		return err
	}
	return a.rt.Close()
}

// stopInspector 停止内置 Inspector 的推送 goroutine(只执行一次)。
func (a *App) stopInspector() {
	a.stopOnce.Do(func() { close(a.stopCh) })
}

// Pkg 返回编译产物(诊断用)。
func (a *App) Pkg() *compile.Package {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pkg
}

// WatchDir 以轮询监听组件目录,变化即回调重新编译的包(配合 HotReload)。
// 回调在后台 goroutine 上执行;编译失败不回调。返回停止函数。
func WatchDir(dir, entry string, interval time.Duration, onPkg func(*compile.Package)) (stop func()) {
	stopCh := make(chan struct{})
	go func() {
		last := compile.DirSignature(dir)
		for {
			select {
			case <-stopCh:
				return
			case <-time.After(interval):
			}
			now := compile.DirSignature(dir)
			if now == last {
				continue
			}
			last = now
			pkg, err := compile.CompileDir(dir, compile.Options{Entry: entry})
			if err != nil {
				continue // 编译失败:保持旧世界
			}
			onPkg(pkg)
		}
	}()
	return func() { close(stopCh) }
}

// —— Go 服务桥 ——

// Expose 把一个 Go 函数暴露为组件可调用的服务方法:
// 组件里 `ctx.api.call(service, method, ...args)`。
//
// 参数与返回值走 JSON:参数按函数签名逐个反序列化,返回值序列化回 JS;
// Go error 变成 JS 侧异常。签名支持 func() T、func(...) (T, error)、
// func(...) error;首参若是 context.Context 自动注入。
//
// 服务方法在 JS loop goroutine 上同步执行——慢调用会阻塞渲染,
// 长任务请立即返回并用 Emit 把结果推回组件。
func (a *App) Expose(service, method string, fn any) error {
	return a.rt.Expose(service, method, fn)
}

// Emit 向所有订阅组件广播一个事件(组件用 `ctx.on(event, fn)` 接收),
// 随后安排一次渲染。任意 goroutine 可调。
func (a *App) Emit(event string, payload any) {
	a.rt.Notify(event, payload)
}

// HotReload 组件级热重载:重新编译后的组件包就地热替换——
//   - 模板/样式/i18n 变更:实例与状态完整保留,下一帧即生效
//   - 脚本变更:重跑受影响组件的 setup(props 保留,组件内部状态重置),窗口不关
//
// 编译失败的包不要传入(旧世界不受影响)。新增组件会被装载并可立即挂载。
func (a *App) HotReload(pkg *compile.Package) error {
	if err := a.rt.HotReload(pkg); err != nil {
		return err
	}
	a.mu.Lock()
	a.pkg = pkg
	a.mu.Unlock()
	return nil
}

// —— 调试/检查 ——

// WindowIDs 返回已注册窗口 id 列表(Inspector 用)。
func (a *App) WindowIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.windows))
	for _, w := range a.windows {
		ids = append(ids, w.ID)
	}
	return ids
}

// DumpWindow 返回窗口当前 widget 树的调试文本。
func (a *App) DumpWindow(winID string) string { return a.rt.TreeDump(winID) }

// Stats 返回 surface 注册表计数(实例/handler/事件订阅/窗口/渲染 pass 数)。
func (a *App) Stats() (map[string]float64, error) { return a.rt.Stats() }
