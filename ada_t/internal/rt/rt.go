// Package rt 是运行时编译层的 JS 宿主与渲染桥。
//
// 线程模型(moejs 硬约束:Runtime 只能在专属 goroutine 上使用):
//
//	eventloop.Loop(专属 goroutine)
//	  — 所有 moejs 访问:加载组件模块、mount、渲染 pass、事件求值
//	  — surface.js 的响应式 flush 在 loop 上,notify 经原生函数回到 Go
//	其他 goroutine(backend 主线程/测试)
//	  — Fire / 渲染调度:一律 RunOnLoop 投递任务,不直接碰 Runtime
//
// 渲染 pass:surface.renderWindow(winID) 产出 widget 树 JSON → Go 反序列化
// → 保存窗口最新树 → 通知 backend(backend 自己决定如何呈现)。
package rt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"

	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/scriptc"
	"github.com/issueye/ada_t/widget"
)

// Options 是运行时参数。
type Options struct {
	// Logf 接收 JS 侧日志与运行时诊断;nil 丢弃。
	Logf func(format string, args ...any)
	// Locale 是当前语言(zh-CN 等);空则 zh-CN。
	Locale string
	// AppI18n 是应用级文案:locale → key → 文案(组件 i18n 优先)。
	AppI18n map[string]map[string]string
	// OnTree 在每次渲染 pass 后按窗口回调(任意顺序;tree 只读)。
	// 回调发生在 loop goroutine 上,实现方不得阻塞。
	OnTree func(winID string, tree *widget.Node)
	// Theme 是初始主题令牌(插件通过 ctx.theme() 读取,宿主可用 Lobe MyGo tokens)。
	Theme map[string]any
}

// Window 是一个已挂载的窗口。
type Window struct {
	ID        string
	Component string
	Props     map[string]any
	InstID    string // surface 侧实例 id
	// Owner 是所属插件 id(空表示宿主窗口);UnloadPlugin 据此回收。
	Owner string

	mu       sync.RWMutex
	tree     *widget.Node
	lastJSON string // 上一帧的渲染 JSON(no-op 跳过)
	mounted  bool   // surface 侧 mountWindow 是否已完成(未完成跳过渲染)
}

// Tree 返回窗口最新渲染树(快照引用;调用方不得修改)。
func (w *Window) Tree() *widget.Node {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.tree
}

// Runtime 是一个组件应用的 JS 世界。
type Runtime struct {
	pkg     *compile.Package
	opts    Options
	loop    *eventloop.Loop
	raw     *moejs.Runtime // 只在 loop goroutine 上使用
	surface *moejs.Module
	logf    func(format string, args ...any)

	mu      sync.Mutex
	windows map[string]*Window
	stopped bool

	notifyPending atomic.Bool
	closed        atomic.Bool
	// closedCh 在 Close 时关闭:阻塞等待 loop 结果的调用方据此立即返回。
	// moejs 的 Loop.Stop 会把「已投递但未执行」的任务留给下一次 Start,
	// 而本层从不重启 loop,没有这个信号等待方会永久阻塞。
	closedCh  chan struct{}
	closeOnce sync.Once

	toastMu sync.Mutex
	toastQ  map[string][]string // winID → 待显示的 toast(重放帧时消费)

	// defined 记录已经进入 surface CJS 注册表的模块(HMR 增量装载用)。
	// 不能用单个「已初始化」布尔值:附加包(Inspector)与主包各自装载,
	// 否则附加包先装载会让主包未变更的依赖模块永远进不了注册表。
	defined map[string]bool

	knownMu    sync.Mutex
	knownComps map[string]bool // 已装载的组件名(含附加包;MountWindow 校验用)

	pluginMu sync.Mutex
	plugins  map[string]*loadedPlugin // 已装载插件

	themeMu sync.Mutex
	theme   map[string]any

	hostMu    sync.Mutex
	hostHooks []func(name string, payload any)
}

// New 创建运行时:启动 loop、注入宿主能力、加载 surface 与全部组件。
func New(pkg *compile.Package, opts Options) (*Runtime, error) {
	if pkg == nil {
		return nil, fmt.Errorf("rt: Package 不能为 nil")
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Locale == "" {
		opts.Locale = "zh-CN"
	}
	r := &Runtime{
		pkg:        pkg,
		opts:       opts,
		logf:       opts.Logf,
		windows:    map[string]*Window{},
		toastQ:     map[string][]string{},
		knownComps: map[string]bool{},
		defined:    map[string]bool{},
		plugins:    map[string]*loadedPlugin{},
		theme:      map[string]any{},
		closedCh:   make(chan struct{}),
	}

	raw := moejs.NewRuntime(moejs.Options{
		DisableDynamicCode: true, // 无 eval/Function 构造器(§4.1 硬规则)
	})
	r.raw = raw
	loop, err := eventloop.New(raw, eventloop.Options{
		OnError: func(err error) {
			opts.Logf("rt: JS 异常(继续运行): %v", err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("rt: 创建 eventloop 失败: %w", err)
	}
	r.loop = loop
	if err := loop.Start(); err != nil {
		return nil, fmt.Errorf("rt: 启动 eventloop 失败: %w", err)
	}
	if err := r.initOnLoop(); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// initOnLoop 在 loop 上完成装配:宿主注入 → surface → 组件模块。
func (r *Runtime) initOnLoop() error {
	ready := make(chan error, 1)
	if !r.post(func() { ready <- r.init() }) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-ready:
		if err != nil {
			return err
		}
		return nil
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}
}

func (r *Runtime) post(fn func()) bool {
	if r.closed.Load() {
		return false
	}
	return r.loop.RunOnLoop(func(_ *moejs.Runtime) { fn() })
}

// init 只在 loop goroutine 上执行。
func (r *Runtime) init() error {
	realm := r.raw.Realm()
	// __adatHost:notify(渲染调度)/ log(JS 日志)
	hostObj := realm.NewObject()
	setFn := func(name string, fn moejs.NativeFunc) error {
		return hostObj.SetProp(realm, realm.KeyFromGoString(name), r.raw.Function(name, 0, fn))
	}
	if err := setFn("notify", r.nativeNotify()); err != nil {
		return fmt.Errorf("rt: 注入 __adatHost.notify 失败: %w", err)
	}
	if err := setFn("toast", r.nativeToast()); err != nil {
		return fmt.Errorf("rt: 注入 __adatHost.notify 失败: %w", err)
	}
	if err := setFn("log", r.nativeLog()); err != nil {
		return fmt.Errorf("rt: 注入 __adatHost.log 失败: %w", err)
	}
	// emit:插件 ctx.emitHost(ev, payload) → 宿主 Go 回调(事件名已被
	// surface 加上 "plugin.<id>." 前缀,避免与宿主事件串流)。
	if err := setFn("emit", r.nativeEmit()); err != nil {
		return fmt.Errorf("rt: 注入 __adatHost.emit 失败: %w", err)
	}
	if err := realm.Global.SetProp(realm, realm.KeyFromGoString("__adatHost"), engine.ObjectValue(hostObj)); err != nil {
		return fmt.Errorf("rt: 注入 __adatHost 失败: %w", err)
	}

	// surface 模块(嵌入 JS,即 ada:surface)
	// 一个 moejs runtime 只能 Load 一个模块图:合成单一 root,
	// 把 surface(副作用导入)+ 全部组件 adopt loader 串进同一张图。
	surfaceMod, err := moejs.Compile("ada:surface", surfaceJS)
	if err != nil {
		return fmt.Errorf("rt: 编译 surface 模块失败: %w", err)
	}
	linkedSurface, err := moejs.Link(surfaceMod, func(_ moejs.Referrer, spec string) (*moejs.Module, error) {
		return nil, fmt.Errorf("rt: surface 模块不允许导入 %q", spec)
	})
	if err != nil {
		return fmt.Errorf("rt: 链接 surface 失败: %w", err)
	}
	r.surface = linkedSurface

	// 组件脚本模块
	compMods := map[string]*moejs.Module{}
	for name, code := range r.pkg.Modules {
		m, cerr := moejs.Compile(name, string(code))
		if cerr != nil {
			return fmt.Errorf("rt: 编译模块 %s 失败: %w", name, cerr)
		}
		compMods[name] = m
	}

	// 合成 root:副作用导入 surface + 每个 loader
	var rootSrc strings.Builder
	rootSrc.WriteString("import \"ada:surface\"\n")
	loaderNames := make([]string, 0, len(r.pkg.Components))
	for _, comp := range r.pkg.Components {
		r.knownMu.Lock()
		r.knownComps[comp.Name] = true
		r.knownMu.Unlock()
		loaderName := "adopt:" + comp.Name
		loaderNames = append(loaderNames, loaderName)
		loader, lerr := moejs.Compile(loaderName, r.loaderSource(comp))
		if lerr != nil {
			return fmt.Errorf("rt: 生成 %s 失败: %w", loaderName, lerr)
		}
		compMods[loaderName] = loader
		rootSrc.WriteString("import " + jsStr("./"+loaderName) + "\n")
	}
	root, err := moejs.Compile("entry", rootSrc.String())
	if err != nil {
		return fmt.Errorf("rt: 生成装配根模块失败: %w", err)
	}
	linked, err := moejs.Link(root, func(referrer moejs.Referrer, spec string) (*moejs.Module, error) {
		if spec == "ada:surface" {
			return linkedSurface, nil
		}
		// 合成模块(adopt loader)与裸模块名直查
		if m, ok := compMods[strings.TrimPrefix(spec, "./")]; ok {
			return m, nil
		}
		resolved, ok := r.pkg.Resolve(referrer.Name(), spec)
		if !ok {
			return nil, fmt.Errorf("rt: 无法解析 %q(来自 %s):只允许相对导入与 ada:surface;裸包名/Node 内置被拒绝 [EUIIMPORT]", spec, referrer.Name())
		}
		m, ok := compMods[resolved]
		if !ok {
			return nil, fmt.Errorf("rt: 解析到不存在的模块 %q", resolved)
		}
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("rt: 链接装配图失败: %w", err)
	}
	if err := r.raw.Load(linked); err != nil {
		return fmt.Errorf("rt: 加载装配图失败: %w", err)
	}

	// 语言与应用级文案
	if _, err := r.callString("setLocale", r.opts.Locale); err != nil {
		return fmt.Errorf("rt: setLocale 失败: %w", err)
	}
	if len(r.opts.AppI18n) > 0 {
		b, _ := json.Marshal(r.opts.AppI18n)
		if _, err := r.callString("setAppI18n", string(b)); err != nil {
			return fmt.Errorf("rt: setAppI18n 失败: %w", err)
		}
	}
	if len(r.opts.Theme) > 0 {
		b, _ := json.Marshal(r.opts.Theme)
		if _, err := r.callString("setTheme", string(b)); err != nil {
			return fmt.Errorf("rt: setTheme 失败: %w", err)
		}
	}
	return nil
}

// loaderSource 生成一个组件的 adopt loader:import 组件脚本并注册。
func (r *Runtime) loaderSource(comp *compile.Component) string {
	planStr, _ := json.Marshal(string(comp.Plan))
	styleStr, _ := json.Marshal("")
	if comp.Style != nil {
		styleStr, _ = json.Marshal(mustJSON(comp.Style))
	}
	i18nStr, _ := json.Marshal("")
	if comp.I18n != nil {
		i18nStr, _ = json.Marshal(mustJSON(comp.I18n))
	}
	if comp.Module != "" {
		// 有脚本(含无脚本组件的占位模块):import DEF 注册
		return fmt.Sprintf(`import DEF from %s
import { registerComponent } from "ada:surface"
registerComponent(%s, %s, %s, %s, DEF)
`, jsStr("./"+comp.Module), jsStr(comp.Name), planStr, styleStr, i18nStr)
	}
	return fmt.Sprintf(`import { registerComponent } from "ada:surface"
registerComponent(%s, %s, %s, %s, undefined)
`, jsStr(comp.Name), planStr, styleStr, i18nStr)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func jsStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// —— 原生函数(在 loop goroutine 上被 JS 调用) ——

// nativeNotify:surface 的响应式 flush 之后通知 Go 安排渲染 pass。
func (r *Runtime) nativeNotify() moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		r.scheduleRender()
		return engine.Undefined(), nil
	}
}

// nativeToast:组件 ctx.toast(message) 入队;重放帧时由后端消费显示。
func (r *Runtime) nativeToast() moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("toast(winId, message) 需要两个参数")
		}
		winID, err1 := r.raw.ToGo(args[0])
		msg, err2 := r.raw.ToGo(args[1])
		if err1 != nil || err2 != nil {
			return engine.Undefined(), fmt.Errorf("rt: toast 参数无法转换: %v/%v", err1, err2)
		}
		winIDStr, _ := winID.(string)
		msgStr, _ := msg.(string)
		if msgStr == "" {
			return engine.Undefined(), nil
		}
		r.toastMu.Lock()
		r.toastQ[winIDStr] = append(r.toastQ[winIDStr], msgStr)
		r.toastMu.Unlock()
		// 入队后安排渲染:重放帧才会消费 toast(c.Toast),否则没有帧就永远不显示
		r.scheduleRender()
		return engine.Undefined(), nil
	}
}

// DrainToasts 取走指定窗口待显示的 toast(由 mygo 后端在重放帧里消费)。
// 任意 goroutine 可调。
func (r *Runtime) DrainToasts(winID string) []string {
	r.toastMu.Lock()
	defer r.toastMu.Unlock()
	q := r.toastQ[winID]
	if len(q) == 0 {
		return nil
	}
	delete(r.toastQ, winID)
	return q
}

// nativeEmit:插件 ctx.emitHost(ev, payload) → 宿主 Go 监听(异步派发,
// 避免监听者在 loop 上再调 rt 方法时自锁)。
func (r *Runtime) nativeEmit() moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) == 0 {
			return engine.Undefined(), fmt.Errorf("emit(name, payload) 至少需要事件名")
		}
		nameV, err := r.raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("rt: emit 事件名无法转换: %w", err)
		}
		name, _ := nameV.(string)
		var payload any
		if len(args) > 1 {
			s, _ := r.raw.ToGo(args[1])
			if pj, ok := s.(string); ok && pj != "" {
				if err := json.Unmarshal([]byte(pj), &payload); err != nil {
					return engine.Undefined(), fmt.Errorf("rt: emit 载荷不是合法 JSON: %w", err)
				}
			}
		}
		r.hostMu.Lock()
		hooks := append([]func(string, any){}, r.hostHooks...)
		r.hostMu.Unlock()
		for _, fn := range hooks {
			fn := fn
			go fn(name, payload)
		}
		return engine.Undefined(), nil
	}
}

func (r *Runtime) nativeLog() moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		parts := make([]any, 0, len(args))
		for _, a := range args {
			v, err := r.raw.ToGo(a)
			if err != nil {
				v = "<无法转换>"
			}
			parts = append(parts, v)
		}
		r.logf("%v", parts)
		return engine.Undefined(), nil
	}
}

// scheduleRender 合并渲染请求(一个 loop 轮次最多一个渲染 pass)。
func (r *Runtime) scheduleRender() {
	if !r.notifyPending.CompareAndSwap(false, true) {
		return
	}
	if !r.post(func() {
		r.notifyPending.Store(false)
		r.renderPass()
	}) {
		r.notifyPending.Store(false)
	}
}

// renderPass 在 loop goroutine 上渲染全部窗口。
func (r *Runtime) renderPass() {
	r.mu.Lock()
	wins := make([]*Window, 0, len(r.windows))
	for _, w := range r.windows {
		wins = append(wins, w)
	}
	r.mu.Unlock()
	for _, w := range wins {
		w.mu.RLock()
		mounted := w.mounted
		w.mu.RUnlock()
		if !mounted {
			continue // 挂载任务还没执行(避免 "窗口未挂载" 噪声)
		}
		out, err := r.callString("renderWindow", w.ID)
		if err != nil {
			r.logf("rt: 渲染窗口 %s 失败: %v", w.ID, err)
			continue
		}
		// no-op 跳过:与上一帧 JSON 相同则不惊动后端(避免空帧重放)
		w.mu.RLock()
		unchanged := w.lastJSON == out
		w.mu.RUnlock()
		if unchanged {
			continue
		}
		var tree widget.Node
		if err := json.Unmarshal([]byte(out), &tree); err != nil {
			r.logf("rt: 窗口 %s 的渲染结果不是合法 widget JSON: %v", w.ID, err)
			continue
		}
		w.mu.Lock()
		w.tree = &tree
		w.lastJSON = out
		w.mu.Unlock()
		if r.opts.OnTree != nil {
			r.opts.OnTree(w.ID, w.tree)
		}
	}
}

// —— 供 loop 外调用的公共 API ——

// MountWindow 挂载一个窗口(组件必须已装载:主包或附加包)。
func (r *Runtime) MountWindow(winID, component string, props map[string]any) (*Window, error) {
	return r.MountWindowOwned(winID, component, props, "")
}

// MountWindowOwned 挂载一个属于某插件的窗口/槽位视图(owner 为空即宿主)。
// 卸载插件时这些视图会被自动回收。
func (r *Runtime) MountWindowOwned(winID, component string, props map[string]any, owner string) (*Window, error) {
	r.knownMu.Lock()
	found := r.knownComps[component]
	r.knownMu.Unlock()
	if !found {
		return nil, fmt.Errorf("rt: 组件 %q 未编译注册", component)
	}
	w := &Window{ID: winID, Component: component, Props: props, Owner: owner}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil, fmt.Errorf("rt: 运行时已关闭")
	}
	r.windows[winID] = w
	r.mu.Unlock()

	var instID string
	err := r.callOnLoopString(&instID, "mountWindow", winID, component, mustJSON(props))
	if err != nil {
		r.mu.Lock()
		delete(r.windows, winID)
		r.mu.Unlock()
		return nil, fmt.Errorf("rt: 挂载窗口 %s 失败: %w", winID, err)
	}
	w.InstID = instID
	w.mu.Lock()
	w.mounted = true
	w.mu.Unlock()
	r.scheduleRender() // 首帧
	return w, nil
}

// Fire 触发一个事件(handler id 来自最近一次渲染树)。任意 goroutine 可调。
func (r *Runtime) Fire(hid string, payload any) {
	payloadJSON := ""
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			payloadJSON = string(b)
		}
	}
	if !r.post(func() {
		_, _ = r.callString("fireHandler", hid, payloadJSON)
		// 事件处理可能改状态;安排渲染(notify 也会触发,这里保底)
		r.scheduleRender()
	}) {
		return
	}
}

// UnmountWindow 移除窗口:surface 侧 dispose 根实例(连带子实例、
// 事件订阅与 handler),Go 侧清注册与渲染树。可从任意 goroutine 调。
func (r *Runtime) UnmountWindow(winID string) {
	// 先在 loop 上 dispose JS 侧(surface 的 windows/instances/handlers)
	_, _ = r.runOnLoopString("unmountWindow", winID)
	r.mu.Lock()
	w := r.windows[winID]
	delete(r.windows, winID)
	r.mu.Unlock()
	if w == nil {
		return
	}
	w.mu.Lock()
	w.tree = nil
	w.mu.Unlock()
	if r.opts.OnTree != nil {
		r.opts.OnTree(winID, nil)
	}
}

// Stats 返回 surface 注册表计数(实例/handler/事件订阅/窗口/渲染 pass 数,
// 键名 renders)。泄漏测试、单次渲染回归与 Inspector 用;值为 JSON 反序列化后的浮点数。
func (r *Runtime) Stats() (map[string]float64, error) {
	out, err := r.runOnLoopString("stats")
	if err != nil {
		return nil, err
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return nil, fmt.Errorf("rt: stats 不是合法 JSON: %w", err)
	}
	return m, nil
}

// —— 服务桥(ctx.api.call / Go→组件事件) ——

// Expose 把一个 Go 函数暴露为组件可调用的服务方法:
// 组件里 `ctx.api.call(service, method, ...args)`(同步,在 loop 上执行)
// 或 `await ctx.api.callAsync(...)`(独立 goroutine,返回 Promise,不阻塞渲染)。
// 参数与返回值走 JSON;Go error 变成 JS 异常 / Promise 拒绝。可在任意时刻调用。
// 注意:callAsync 的实现跑在调用方 goroutine 上,服务实现必须自行保证并发安全。
func (r *Runtime) Expose(service, method string, fn any) error {
	inv, err := newInvoker(service, method, fn)
	if err != nil {
		return err
	}
	ready := make(chan error, 1)
	if !r.post(func() {
		syncVal := r.raw.Function("api_"+service+"_"+method, 1, r.nativeServiceCall(inv))
		asyncVal := r.raw.Function("api_"+service+"_"+method+"_async", 1, r.nativeServiceCallAsync(inv))
		nameV, err1 := r.raw.FromGo(service)
		methodV, err2 := r.raw.FromGo(method)
		if err1 != nil || err2 != nil {
			ready <- fmt.Errorf("rt: 服务名/方法名转换失败: %v/%v", err1, err2)
			return
		}
		svcFn, err3 := r.surfaceFn("registerService")
		if err3 != nil {
			ready <- err3
			return
		}
		_, err4 := r.raw.CallFunction(svcFn, engine.Undefined(), nameV, methodV, syncVal, asyncVal)
		ready <- err4
	}) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("rt: 注册服务 %s.%s 失败: %w", service, method, err)
		}
		return nil
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}
}

// nativeServiceCallAsync:异步版本。参数在 loop 上解析,调用跑在独立
// goroutine,结果经 NewPromise 的 settle 回到 loop settle 掉 Promise;
// Go error 使 Promise 以该错误拒绝(JS 侧 await/catch 收到)。
func (r *Runtime) nativeServiceCallAsync(inv *serviceInvoker) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		var argsJSON []byte
		if len(args) > 0 {
			v, err := r.raw.ToGo(args[0])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的参数无法读取: %w", inv.service, inv.method, err)
			}
			if s, ok := v.(string); ok {
				argsJSON = []byte(s)
			}
		}
		var decoded []any
		if len(argsJSON) > 0 {
			if err := json.Unmarshal(argsJSON, &decoded); err != nil {
				return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的参数不是合法 JSON: %w", inv.service, inv.method, err)
			}
		}
		p, settle := r.loop.NewPromise()
		go func() {
			out, err := inv.call(decoded)
			settle(func(rt *moejs.Runtime) (moejs.Value, error) {
				if err != nil {
					// settle 把返回的 error 转成 Promise 拒绝(ThrownValue)
					return engine.Undefined(), err
				}
				if out == nil {
					return engine.Undefined(), nil
				}
				b, merr := json.Marshal(out)
				if merr != nil {
					return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的返回值无法序列化: %w", inv.service, inv.method, merr)
				}
				return rt.ParseJSON(b)
			})
		}()
		return p, nil
	}
}

// nativeServiceCall 生成一个服务方法的原生函数:入参 [argsJSON 字符串],
// 出参为结果 JSON;Go error 直接作为 JS 异常抛出。
func (r *Runtime) nativeServiceCall(inv *serviceInvoker) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		var argsJSON []byte
		if len(args) > 0 {
			v, err := r.raw.ToGo(args[0])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的参数无法读取: %w", inv.service, inv.method, err)
			}
			if s, ok := v.(string); ok {
				argsJSON = []byte(s)
			}
		}
		var decoded []any
		if len(argsJSON) > 0 {
			if err := json.Unmarshal(argsJSON, &decoded); err != nil {
				return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的参数不是合法 JSON: %w", inv.service, inv.method, err)
			}
		}
		out, err := inv.call(decoded)
		if err != nil {
			return engine.Undefined(), err
		}
		if out == nil {
			return engine.Undefined(), nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			return engine.Undefined(), fmt.Errorf("rt: 服务 %s.%s 的返回值无法序列化: %w", inv.service, inv.method, err)
		}
		return r.raw.ParseJSON(b)
	}
}

// Notify 向所有组件广播一个事件(组件用 ctx.on 订阅);随后安排渲染。
// 任意 goroutine 可调。载荷走 JSON。
func (r *Runtime) Notify(event string, payload any) {
	payloadJSON := ""
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			payloadJSON = string(b)
		}
	}
	if !r.post(func() {
		_, _ = r.callString("fireHostEvent", event, payloadJSON)
		// 处理器常会改状态,保底安排渲染(notify 也会触发,这里合并)
		r.scheduleRender()
	}) {
		return
	}
}

// 整世界重载不由本层提供:组件级热重载用 HotReload;需要重置全部状态时,
// 直接构造新的 App/Runtime(旧实例 Close),避免在已停止的 loop 上做原地交换。

// Close 停止运行时。可重复调用。
// 先关闭 closedCh 唤醒所有等待 loop 结果的调用方,再停止 loop——
// 否则 Stop 留下未执行任务时,那些调用方会永久阻塞。
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() { close(r.closedCh) })
	r.closed.Store(true)
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if r.loop != nil {
		return r.loop.Stop()
	}
	return nil
}

// —— Hook 调用辅助(只在 loop goroutine 上) ——

// callOnLoopString 在 loop 上调用 surface 导出函数,取字符串返回值。
func (r *Runtime) callOnLoopString(out *string, fn string, args ...string) error {
	v, err := r.runOnLoopString(fn, args...)
	if err == nil {
		*out = v
	}
	return err
}

// runOnLoopString 在 loop 上调用 surface 导出函数并把结果带回调用方 goroutine。
func (r *Runtime) runOnLoopString(fn string, args ...string) (string, error) {
	type res struct {
		v   string
		err error
	}
	ch := make(chan res, 1)
	if !r.post(func() {
		v, err := r.callString(fn, args...)
		ch <- res{v, err}
	}) {
		return "", fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case got := <-ch:
		return got.v, got.err
	case <-r.closedCh:
		return "", fmt.Errorf("rt: 运行时已关闭")
	}
}

// callString 在 loop goroutine 上调用 surface 全局函数,返回字符串(或空)。
func (r *Runtime) callString(fn string, args ...string) (string, error) {
	fnVal, err := r.surfaceFn(fn)
	if err != nil {
		return "", err
	}
	vals := make([]moejs.Value, 0, len(args))
	for _, a := range args {
		v, cerr := r.raw.FromGo(a)
		if cerr != nil {
			return "", fmt.Errorf("参数 %q 无法转换: %w", a, cerr)
		}
		vals = append(vals, v)
	}
	res, err := r.raw.CallFunction(fnVal, engine.Undefined(), vals...)
	if err != nil {
		return "", r.wrap(err)
	}
	if res == engine.Undefined() {
		return "", nil
	}
	s, err := r.raw.ToGo(res)
	if err != nil {
		return "", fmt.Errorf("返回值无法转换: %w", err)
	}
	switch v := s.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	default:
		return fmt.Sprint(v), nil
	}
}

// surfaceFn 取 globalThis.__adatSurface[name] 的函数值(loop goroutine 上调用)。
func (r *Runtime) surfaceFn(name string) (moejs.Value, error) {
	realm := r.raw.Realm()
	objV, err := realm.Global.GetProp(realm, realm.KeyFromGoString("__adatSurface"))
	if err != nil {
		return moejs.Value{}, fmt.Errorf("rt: 读取 __adatSurface 失败: %w", err)
	}
	if !objV.IsObject() {
		return moejs.Value{}, fmt.Errorf("rt: __adatSurface 不是对象(surface 未加载?)")
	}
	fn, err := objV.AsObject().GetProp(realm, realm.KeyFromGoString(name))
	if err != nil {
		return moejs.Value{}, fmt.Errorf("rt: 读取 __adatSurface.%s 失败: %w", name, err)
	}
	if !engine.IsCallable(fn) {
		return moejs.Value{}, fmt.Errorf("rt: __adatSurface.%s 不是函数", name)
	}
	return fn, nil
}

func (r *Runtime) wrap(err error) error {
	var exc *moejs.Exception
	if ok := asException(err, &exc); ok {
		return fmt.Errorf("JS 异常: %s", r.raw.StackTrace(exc))
	}
	return err
}

func asException(err error, target **moejs.Exception) bool {
	for err != nil {
		if e, ok := err.(*moejs.Exception); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// —— 组件级 HMR ——

// HotReload 热重载组件包:
//   - 模板/样式/i18n 变更 → 只换数据,实例与组件状态完整保留
//   - 脚本变更/新增组件 → CJS 化经 RunScript 装载,重跑 setup(props 保留,组件内部状态重置)
//   - 脚本未变的组件不重载;若其依赖的相对模块变了,同时改一下该组件脚本(加个注释)即可触发重载
//
// 整段 HMR 脚本先编译后执行:编译失败时报错且旧世界不受影响。
// 编译失败的包不要传进来。任意 goroutine 可调。
func (r *Runtime) HotReload(newPkg *compile.Package) error {
	done := make(chan error, 1)
	if !r.post(func() { done <- r.hotReload(newPkg, false) }) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-done:
		if err != nil {
			return err
		}
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}
	r.mu.Lock()
	r.pkg = newPkg
	r.mu.Unlock()
	r.scheduleRender()
	return nil
}

// LoadAdditional 装载附加组件包(如 Inspector):注册其组件与模块,
// 不影响主包的热重载基准与组件校验。任意 goroutine 可调。
func (r *Runtime) LoadAdditional(pkg *compile.Package) error {
	done := make(chan error, 1)
	if !r.post(func() { done <- r.hotReload(pkg, true) }) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		return nil
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}
}

// hotReload 在 loop goroutine 上组装并执行 HMR 脚本。
// additional=true(附加包,如 Inspector):全部组件按新增装载,不影响主包热重载基准。
func (r *Runtime) hotReload(newPkg *compile.Package, additional bool) error {
	oldPkg := r.pkg
	oldComps := map[string]*compile.Component{}
	if !additional {
		for _, c := range oldPkg.Components {
			oldComps[c.Name] = c
		}
	}

	// 变更检测
	var adopts []string // 脚本变更/新增的组件名
	var dataOnly []string
	changedMods := map[string]bool{}
	for _, comp := range newPkg.Components {
		old, existed := oldComps[comp.Name]
		if !existed {
			adopts = append(adopts, comp.Name)
			changedMods[comp.Module] = true
			continue
		}
		scriptChanged := !bytes.Equal(old.Script, comp.Script) || old.HasScript != comp.HasScript
		dataChanged := !bytes.Equal(old.Plan, comp.Plan) || !bytes.Equal([]byte(mustJSON(old.Style)), []byte(mustJSON(comp.Style))) ||
			!bytes.Equal([]byte(mustJSON(old.I18n)), []byte(mustJSON(comp.I18n)))
		if scriptChanged {
			adopts = append(adopts, comp.Name)
			changedMods[comp.Module] = true
		} else if dataChanged {
			dataOnly = append(dataOnly, comp.Name)
		}
	}
	for name, code := range newPkg.Modules {
		if oldCode, existed := oldPkg.Modules[name]; !existed || !bytes.Equal(oldCode, code) {
			changedMods[name] = true
		}
	}
	// 传递依赖:若 A import 了变更模块 B,则 A 也必须重新定义——否则组件
	// 重新 adopt 时 require('./A') 仍返回用旧 B 计算出的 exports。
	expandImporters(newPkg, changedMods)
	// 依赖发生变化(即使自身脚本未变)的组件也要重跑 setup,才能拿到新导出。
	adoptSet := map[string]bool{}
	for _, name := range adopts {
		adoptSet[name] = true
	}
	for _, comp := range newPkg.Components {
		if _, existed := oldComps[comp.Name]; !existed {
			continue // 新增组件已在 adopts 里
		}
		if changedMods[comp.Module] && !adoptSet[comp.Name] {
			adopts = append(adopts, comp.Name)
			adoptSet[comp.Name] = true
		}
	}

	// 组装 HMR 脚本
	var b strings.Builder
	// 1. 每个模块的相对导入映射(Go 侧预解析)
	names := make([]string, 0, len(newPkg.Modules))
	for name := range newPkg.Modules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		specs := scanImports(newPkg.Modules[name])
		m := map[string]string{}
		for _, spec := range specs {
			if resolved, ok := newPkg.Resolve(name, spec); ok && resolved != name {
				m[spec] = resolved
			}
		}
		if len(m) > 0 {
			b.WriteString("__adatRequireMap(" + jsStr(name) + ", " + jsStr(mustJSON(m)) + ");\n")
		}
	}
	// 2. CJS define:变更模块 + 尚未进入注册表的模块。
	// 不能只按「首次/后续」区分:附加包与主包各自装载,附加包先装载时
	// 主包模块还没登记,必须在这里补登记,否则 adopt 的 require 会失败。
	defineNames := make([]string, 0, len(names))
	for _, name := range names {
		if !r.defined[name] || changedMods[name] {
			defineNames = append(defineNames, name)
		}
	}
	for _, name := range defineNames {
		res, err := scriptc.TransformCJS(name, newPkg.Modules[name])
		if err != nil {
			return fmt.Errorf("rt: HMR 降级 %s 失败: %w", name, err)
		}
		b.WriteString("__adatDefine(" + jsStr(name) + ", function (module, exports, require) {\n")
		b.Write(res.Code)
		b.WriteString("\n});\n")
	}
	// 3. 组件 adopt(脚本变更)/ data(仅模板样式 i18n)
	sort.Strings(adopts)
	for _, name := range adopts {
		comp := newCompByName(newPkg, name)
		if comp == nil {
			continue
		}
		b.WriteString(hmrAdoptCall(comp))
	}
	sort.Strings(dataOnly)
	for _, name := range dataOnly {
		comp := newCompByName(newPkg, name)
		if comp == nil {
			continue
		}
		b.WriteString(hmrDataCall(comp))
	}

	// 4. 整段脚本先编译再执行(原子:失败不落地)
	script, err := moejs.CompileScript("hmr", b.String())
	if err != nil {
		return fmt.Errorf("rt: HMR 脚本编译失败: %w", err)
	}
	if _, err := r.raw.RunScript(script); err != nil {
		return r.wrap(err)
	}
	// 脚本执行成功才登记已定义模块(失败时下次 HMR 仍会重新定义)。
	for _, name := range defineNames {
		r.defined[name] = true
	}
	// 登记新装载的组件名(MountWindow 校验用)
	r.knownMu.Lock()
	for _, comp := range newPkg.Components {
		r.knownComps[comp.Name] = true
	}
	r.knownMu.Unlock()
	return nil
}

func hmrAdoptCall(comp *compile.Component) string {
	planStr, _ := json.Marshal(string(comp.Plan))
	styleStr, _ := json.Marshal("")
	if comp.Style != nil {
		styleStr, _ = json.Marshal(mustJSON(comp.Style))
	}
	i18nStr, _ := json.Marshal("")
	if comp.I18n != nil {
		i18nStr, _ = json.Marshal(mustJSON(comp.I18n))
	}
	return "__adatHMRAdopt(" + jsStr(comp.Name) + ", " + string(planStr) + ", " + string(styleStr) + ", " + string(i18nStr) +
		", __adatGetExports(" + jsStr(comp.Module) + ").default);\n"
}

func hmrDataCall(comp *compile.Component) string {
	planStr, _ := json.Marshal(string(comp.Plan))
	styleStr, _ := json.Marshal("")
	if comp.Style != nil {
		styleStr, _ = json.Marshal(mustJSON(comp.Style))
	}
	i18nStr, _ := json.Marshal("")
	if comp.I18n != nil {
		i18nStr, _ = json.Marshal(mustJSON(comp.I18n))
	}
	return "__adatHMRData(" + jsStr(comp.Name) + ", " + string(planStr) + ", " + string(styleStr) + ", " + string(i18nStr) + ");\n"
}

func newCompByName(pkg *compile.Package, name string) *compile.Component {
	for _, c := range pkg.Components {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// expandImporters 把变更集沿「谁 import 了它」反向闭包扩展:
// 一个模块的依赖变了,它自己也要重新定义,否则 CJS require 会拿到旧 exports。
func expandImporters(pkg *compile.Package, changed map[string]bool) {
	for {
		added := false
		for name, code := range pkg.Modules {
			if changed[name] {
				continue
			}
			for _, spec := range scanImports(code) {
				resolved, ok := pkg.Resolve(name, spec)
				if ok && resolved != name && changed[resolved] {
					changed[name] = true
					added = true
					break
				}
			}
		}
		if !added {
			return
		}
	}
}

// scanImports 从 ESM 源码里扫 import 来源(静态 import/export-from 与动态 import)。
func scanImports(src []byte) []string {
	re := regexp.MustCompile(`(?:from|import)\s*\(?\s*["']([^"']+)["']`)
	matches := re.FindAllSubmatch(src, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		spec := string(m[1])
		if !seen[spec] {
			seen[spec] = true
			out = append(out, spec)
		}
	}
	return out
}

// TreeDump 返回窗口当前 widget 树的调试文本(无树返回 "")。
// 任意 goroutine 可调。
func (r *Runtime) TreeDump(winID string) string {
	r.mu.Lock()
	w := r.windows[winID]
	r.mu.Unlock()
	if w == nil {
		return ""
	}
	w.mu.RLock()
	tree := w.tree
	w.mu.RUnlock()
	if tree == nil {
		return ""
	}
	return tree.Dump()
}

// FlatRows 返回窗口 widget 树的扁平化行(Inspector 用;无树返回 nil)。
// 任意 goroutine 可调。
func (r *Runtime) FlatRows(winID string) []widget.FlatRow {
	r.mu.Lock()
	w := r.windows[winID]
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	w.mu.RLock()
	tree := w.tree
	w.mu.RUnlock()
	if tree == nil {
		return nil
	}
	return widget.Flatten(tree)
}

// RenderOnce 同步执行一次窗口渲染并返回 widget JSON(基准测试/调试用)。
// 任意 goroutine 可调;窗口未挂载返回错误。
func (r *Runtime) RenderOnce(winID string) (string, error) {
	return r.runOnLoopString("renderWindow", winID)
}
