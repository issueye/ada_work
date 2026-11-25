package adat

// 插件宿主 API:清单校验 → 运行时装载 → 槽位视图 → 能力白名单/事件前缀。
//
// 设计取舍(见 docs/plugins.md):
//   - 插件是「一个组件包 + 一份 plugin.json」,组件名全局唯一;
//   - 插件只声明槽位与组件,尺寸/位置/主题由宿主决定;
//   - 插件能调的宿主服务由 capabilities 白名单限定(默认拒绝);
//   - 插件事件名自动加 plugin.<id>. 前缀,多插件不会串流;
//   - 安全边界在编译期(受限表达式/无 eval)+ 能力白名单,不是进程隔离。

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"strings"

	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/rt"
	"github.com/issueye/ada_t/plugin"
)

// PluginInfo 是插件摘要(与运行时记录同构)。
type PluginInfo = rt.PluginInfo

// PluginTrust 是宿主对插件的信任策略。
type PluginTrust struct {
	// RequireDigest 要求清单必须带 sha256(内部插件发布时写入)。
	RequireDigest bool
	// PublicKeys 非空时要求清单签名可被其中一把公钥验证。
	PublicKeys []ed25519.PublicKey
}

func (t PluginTrust) verify(pkg *compile.Package, m *plugin.Manifest) error {
	digest := pkg.Digest()
	if m.SHA256 != "" && m.SHA256 != digest {
		return fmt.Errorf("adat: 插件 %s 的产物摘要与清单不符(清单 %s,实际 %s)", m.ID, m.SHA256, digest)
	}
	if t.RequireDigest && m.SHA256 == "" {
		return fmt.Errorf("adat: 插件 %s 缺少 sha256 摘要(信任策略要求)", m.ID)
	}
	if len(t.PublicKeys) > 0 {
		if err := m.VerifySignature(t.PublicKeys...); err != nil {
			return fmt.Errorf("adat: %w", err)
		}
	}
	return nil
}

// SlotView 是一个已挂载的插件槽位视图。宿主用自己的 ui.Context 绘制它
// (backend/embed 的 Draw),或在测试里用 Fire 注入事件。
type SlotView struct {
	ID        string // 视图 id(渲染窗口键;传给 embed.Draw)
	Slot      string
	Plugin    string
	Component string
	Width     int
	Height    int
	Order     int
	Title     string
}

// PluginStats 是一个插件的运行统计。
type PluginStats struct {
	PluginInfo
	Views     int
	SlotCount int
	Runtime   map[string]float64 // 全局注册表计数(实例/handler/订阅/窗口/渲染)
}

// RegisterSlot 注册一个宿主插槽:插件只能往已注册的插槽投放贡献。
// 幂等(重复注册不报错)。
func (a *App) RegisterSlot(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("adat: 插槽名不能为空")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.slots {
		if s == name {
			return nil
		}
	}
	a.slots = append(a.slots, name)
	return nil
}

// Slots 返回已注册的插槽名。
func (a *App) Slots() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.slots...)
	sort.Strings(out)
	return out
}

func (a *App) slotRegistered(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.slots {
		if s == name {
			return true
		}
	}
	return false
}

// LoadPlugin 装载插件:校验清单与信任策略、检查组件名冲突,再交给运行时。
// SlotMode 下会为每个槽位贡献自动挂载一个视图(卸载时自动回收)。
func (a *App) LoadPlugin(pkg *compile.Package, m *plugin.Manifest) (PluginInfo, error) {
	if m == nil || pkg == nil {
		return PluginInfo{}, fmt.Errorf("adat: 插件清单与组件包都不能为 nil")
	}
	if err := m.Validate(); err != nil {
		return PluginInfo{}, err
	}
	if err := a.opts.PluginTrust.verify(pkg, m); err != nil {
		return PluginInfo{}, err
	}
	for _, c := range m.Slots {
		if !a.slotRegistered(c.Slot) {
			return PluginInfo{}, fmt.Errorf("adat: 插件 %s 投放到未注册的插槽 %q(先调用 RegisterSlot;已注册: %v)",
				m.ID, c.Slot, a.Slots())
		}
	}
	if a.rt == nil {
		return PluginInfo{}, fmt.Errorf("adat: 应用未初始化")
	}
	if err := a.rt.LoadPlugin(m, pkg); err != nil {
		return PluginInfo{}, err
	}
	info, _ := a.rt.PluginInfo(m.ID)

	if !a.opts.SlotMode || len(m.Slots) == 0 {
		return info, nil
	}
	views := make([]SlotView, 0, len(m.Slots))
	for i, c := range m.Slots {
		viewID := fmt.Sprintf("slot:%s:%s:%d", c.Slot, m.ID, i)
		if _, err := a.rt.MountWindowOwned(viewID, c.Component, nil, m.ID); err != nil {
			_ = a.rt.UnloadPlugin(m.ID)
			return PluginInfo{}, fmt.Errorf("adat: 挂载插件 %s 的槽位视图 %s 失败: %w", m.ID, c.Slot, err)
		}
		views = append(views, SlotView{
			ID: viewID, Slot: c.Slot, Plugin: m.ID, Component: c.Component,
			Width: c.Width, Height: c.Height, Order: c.Order, Title: c.Title,
		})
	}
	a.mu.Lock()
	a.views = append(a.views, views...)
	a.mu.Unlock()
	return info, nil
}

// LoadAdditional 装载一个附加组件包(不带插件清单,例如宿主的调试面板):
// 组件进入全局注册表、可直接挂载;需要「可卸载 + 能力白名单」时用 LoadPlugin。
func (a *App) LoadAdditional(pkg *compile.Package) error {
	if a.rt == nil {
		return fmt.Errorf("adat: 应用未初始化")
	}
	return a.rt.LoadAdditional(pkg)
}

// UnloadPlugin 卸载插件并回收它的全部槽位视图。
// 宿主应停止 Draw 这些视图(它们已从 SlotViews 移除)。
func (a *App) UnloadPlugin(id string) error {
	if a.rt == nil {
		return fmt.Errorf("adat: 应用未初始化")
	}
	if err := a.rt.UnloadPlugin(id); err != nil {
		return err
	}
	a.mu.Lock()
	kept := a.views[:0]
	for _, v := range a.views {
		if v.Plugin != id {
			kept = append(kept, v)
		}
	}
	a.views = kept
	a.mu.Unlock()
	return nil
}

// Plugins 返回已装载插件摘要。
func (a *App) Plugins() []PluginInfo {
	if a.rt == nil {
		return nil
	}
	return a.rt.Plugins()
}

// PluginInfo 返回单个插件摘要。
func (a *App) PluginInfo(id string) (PluginInfo, bool) {
	if a.rt == nil {
		return PluginInfo{}, false
	}
	return a.rt.PluginInfo(id)
}

// SlotViews 返回槽位视图;slot 为空返回全部。按槽位、order、id 排序,
// 宿主据此决定绘制顺序。
func (a *App) SlotViews(slot string) []SlotView {
	a.mu.Lock()
	out := make([]SlotView, 0, len(a.views))
	for _, v := range a.views {
		if slot == "" || v.Slot == slot {
			out = append(out, v)
		}
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// PluginStats 返回插件运行统计(含全局注册表计数)。
func (a *App) PluginStats(id string) (PluginStats, error) {
	info, ok := a.PluginInfo(id)
	if !ok {
		return PluginStats{}, fmt.Errorf("adat: 插件 %s 未装载", id)
	}
	views := a.SlotViews("")
	n := 0
	for _, v := range views {
		if v.Plugin == id {
			n++
		}
	}
	m, _ := a.rt.Stats()
	return PluginStats{PluginInfo: info, Views: n, SlotCount: len(info.Slots), Runtime: m}, nil
}

// SetTheme 设置主题令牌并下发到插件(ctx.theme())。
func (a *App) SetTheme(tokens map[string]any) error {
	if a.rt == nil {
		return fmt.Errorf("adat: 应用未初始化")
	}
	if err := a.rt.SetTheme(tokens); err != nil {
		return err
	}
	a.mu.Lock()
	a.theme = tokens
	a.mu.Unlock()
	return nil
}

// Theme 返回当前主题令牌。
func (a *App) Theme() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]any, len(a.theme))
	for k, v := range a.theme {
		out[k] = v
	}
	return out
}

// ExposeHost 暴露一个宿主服务方法给插件调用(ctx.api.call(service, method))。
// 插件必须在 plugin.json 的 capabilities 里声明 service.method 才能调用;
// 未声明即拒绝(capability denied)。
func (a *App) ExposeHost(service, method string, fn any) error {
	return a.Expose(service, method, fn)
}

// HostCapabilities 返回已暴露的宿主服务方法(service.method),供宿主
// 生成插件能力白名单文档或校验插件清单。
func (a *App) HostCapabilities() []string {
	if a.rt == nil {
		return nil
	}
	return a.rt.Capabilities()
}

// EmitTo 向指定插件广播事件;插件里 ctx.on(event, fn) 接收。
// 事件名在运行时被加上 plugin.<id>. 前缀,其它插件收不到。
func (a *App) EmitTo(pluginID, event string, payload any) error {
	if _, ok := a.PluginInfo(pluginID); !ok {
		return fmt.Errorf("adat: 插件 %s 未装载", pluginID)
	}
	a.rt.Notify("plugin."+pluginID+"."+event, payload)
	return nil
}

// OnPluginEvent 监听插件经 ctx.emitHost 发来的事件(回调在独立 goroutine 上)。
func (a *App) OnPluginEvent(fn func(pluginID, event string, payload any)) {
	if a.rt == nil || fn == nil {
		return
	}
	a.rt.OnHostEvent(func(name string, payload any) {
		if !strings.HasPrefix(name, "plugin.") {
			return
		}
		rest := strings.TrimPrefix(name, "plugin.")
		i := strings.IndexByte(rest, '.')
		if i <= 0 {
			return
		}
		fn(rest[:i], rest[i+1:], payload)
	})
}

// —— 编译入口(宿主无需 import internal 包) ——

// CompileSource 从内存源码编译一个组件包(宿主不需要为「占位组件」落盘)。
// entryFile 形如 "Host.aui"。
func CompileSource(entryFile, src string) (*compile.Package, error) {
	return compile.CompileFiles(entryFile, map[string]string{entryFile: src}, compile.Options{})
}

// CompileDir 编译一个组件目录(entry 为空时目录内只能有一个 .aui)。
func CompileDir(dir, entry string) (*compile.Package, error) {
	return compile.CompileDir(dir, compile.Options{Entry: entry})
}

// CompilePlugin 编译一个插件目录:读取 plugin.json,按清单的 entry 编译,
// 并校验清单里的槽位组件确实存在。
func CompilePlugin(dir string) (*compile.Package, *plugin.Manifest, error) {
	m, err := plugin.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	pkg, err := compile.CompileDir(dir, compile.Options{Entry: m.Entry})
	if err != nil {
		return nil, nil, err
	}
	if _, ok := componentByName(pkg, m.Entry); !ok {
		return nil, nil, fmt.Errorf("adat: 插件 %s 的入口组件 %q 不存在", m.ID, m.Entry)
	}
	for _, c := range m.Slots {
		if _, ok := componentByName(pkg, c.Component); !ok {
			return nil, nil, fmt.Errorf("adat: 插件 %s 的槽位 %s 引用了不存在的组件 %q", m.ID, c.Slot, c.Component)
		}
	}
	return pkg, m, nil
}

// PluginDigest 返回编译产物摘要("sha256:…"),供打包工具写入 manifest。
func PluginDigest(pkg *compile.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Digest()
}

// LoadPluginDir 编译并装载一个插件目录(等价 CompilePlugin + LoadPlugin)。
func (a *App) LoadPluginDir(dir string) (PluginInfo, error) {
	pkg, m, err := CompilePlugin(dir)
	if err != nil {
		return PluginInfo{}, err
	}
	return a.LoadPlugin(pkg, m)
}

func componentByName(pkg *compile.Package, name string) (*compile.Component, bool) {
	for _, c := range pkg.Components {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}
