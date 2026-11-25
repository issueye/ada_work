package rt

// 插件装载:把一个独立编译的组件包装进现有运行时,并给它
// 「模块命名空间 + 能力白名单 + 事件前缀 + 槽位视图」这四层边界。
//
// 边界在运行时层,不在进程层:插件与宿主同进程、同 goroutine 模型,
// 安全靠编译期(受限表达式、无 eval)与能力白名单,而不是操作系统隔离。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/plugin"
)

// loadedPlugin 是一个已装载插件的运行时记录。
type loadedPlugin struct {
	manifest   *plugin.Manifest
	prefix     string   // 模块名前缀(plugin.<id>/)
	components []string // 该插件注册的组件名
}

// PluginInfo 是一个已装载插件的公开摘要。
type PluginInfo struct {
	ID           string
	Name         string
	Version      string
	Entry        string
	Components   []string
	Capabilities []string
	Slots        []plugin.Contribution
}

// LoadPlugin 装载一个插件包。manifest 必须已通过校验;组件名不能与
// 已装载组件(宿主或其它插件)重名——组件注册表按名字全局唯一。
func (r *Runtime) LoadPlugin(m *plugin.Manifest, pkg *compile.Package) error {
	if m == nil || pkg == nil {
		return fmt.Errorf("rt: 插件清单与组件包都不能为 nil")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if _, exists := r.plugin(m.ID); exists {
		return fmt.Errorf("rt: 插件 %s 已装载", m.ID)
	}
	comps := make([]string, 0, len(pkg.Components))
	for _, c := range pkg.Components {
		r.knownMu.Lock()
		dup := r.knownComps[c.Name]
		r.knownMu.Unlock()
		if dup {
			return fmt.Errorf("rt: 插件 %s 的组件 %q 与已装载组件重名(组件名全局唯一)", m.ID, c.Name)
		}
		comps = append(comps, c.Name)
	}
	sort.Strings(comps)

	prefix := "plugin." + m.ID + "/"
	prefixed := prefixPackage(pkg, prefix)

	done := make(chan error, 1)
	if !r.post(func() {
		if err := r.hotReload(prefixed, true); err != nil {
			done <- err
			return
		}
		capsJSON, _ := json.Marshal(m.Capabilities)
		compsJSON, _ := json.Marshal(comps)
		_, err := r.callString("bindPlugin", m.ID, string(capsJSON), string(compsJSON))
		done <- err
	}) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-done:
		if err != nil {
			r.rollbackLoad(prefix, comps)
			return fmt.Errorf("rt: 装载插件 %s 失败: %w", m.ID, err)
		}
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}

	r.pluginMu.Lock()
	r.plugins[m.ID] = &loadedPlugin{manifest: m, prefix: prefix, components: comps}
	r.pluginMu.Unlock()
	r.scheduleRender()
	return nil
}

// rollbackLoad 回滚一次失败的装载(组件已注册但绑定插件失败时的兜底)。
func (r *Runtime) rollbackLoad(prefix string, comps []string) {
	namesJSON, _ := json.Marshal(comps)
	_, _ = r.runOnLoopString("unregisterComponents", string(namesJSON))
	_, _ = r.runOnLoopString("forgetModules", prefix)
	r.post(func() {
		for name := range r.defined {
			if strings.HasPrefix(name, prefix) {
				delete(r.defined, name)
			}
		}
	})
	r.knownMu.Lock()
	for _, name := range comps {
		delete(r.knownComps, name)
	}
	r.knownMu.Unlock()
}

// UnloadPlugin 卸载插件:先卸载它的槽位视图,再注销组件与模块。
// 宿主必须先把该插件的视图从自己的绘制列表里移除(不再 Draw)。
func (r *Runtime) UnloadPlugin(id string) error {
	pl, ok := r.plugin(id)
	if !ok {
		return fmt.Errorf("rt: 插件 %s 未装载", id)
	}
	// 1. 卸载该插件拥有的窗口/槽位视图
	r.mu.Lock()
	owned := make([]string, 0, len(pl.components))
	for winID, w := range r.windows {
		if w.Owner == id {
			owned = append(owned, winID)
		}
	}
	r.mu.Unlock()
	sort.Strings(owned)
	for _, winID := range owned {
		r.UnmountWindow(winID)
	}

	// 2. 注销组件与模块(surface 侧释放实例、handler、事件订阅)
	namesJSON, _ := json.Marshal(pl.components)
	done := make(chan error, 1)
	if !r.post(func() {
		_, err := r.callString("unregisterComponents", string(namesJSON))
		if err == nil {
			_, err = r.callString("forgetModules", pl.prefix)
		}
		if err == nil {
			_, err = r.callString("unbindPlugin", id)
		}
		if err == nil {
			for name := range r.defined {
				if strings.HasPrefix(name, pl.prefix) {
					delete(r.defined, name)
				}
			}
		}
		done <- err
	}) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("rt: 卸载插件 %s 失败: %w", id, err)
		}
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}

	// 3. Go 侧登记清理
	r.knownMu.Lock()
	for _, name := range pl.components {
		delete(r.knownComps, name)
	}
	r.knownMu.Unlock()
	r.pluginMu.Lock()
	delete(r.plugins, id)
	r.pluginMu.Unlock()
	r.scheduleRender()
	return nil
}

// PluginInfo 返回单个插件摘要。
func (r *Runtime) PluginInfo(id string) (PluginInfo, bool) {
	pl, ok := r.plugin(id)
	if !ok {
		return PluginInfo{}, false
	}
	return infoOf(pl), true
}

// Plugins 返回全部已装载插件(按 id 排序)。
func (r *Runtime) Plugins() []PluginInfo {
	r.pluginMu.Lock()
	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]PluginInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, infoOf(r.plugins[id]))
	}
	r.pluginMu.Unlock()
	return out
}

// PluginViews 返回插件拥有的视图 id(槽位挂载的窗口,按 id 排序)。
func (r *Runtime) PluginViews(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, 4)
	for winID, w := range r.windows {
		if w.Owner == id {
			out = append(out, winID)
		}
	}
	sort.Strings(out)
	return out
}

// SetTheme 设置主题令牌(插件通过 ctx.theme() 读取;宿主可传 Lobe MyGo tokens)。
func (r *Runtime) SetTheme(tokens map[string]any) error {
	if tokens == nil {
		tokens = map[string]any{}
	}
	b, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("rt: 主题令牌无法序列化: %w", err)
	}
	r.themeMu.Lock()
	r.theme = tokens
	r.themeMu.Unlock()
	done := make(chan error, 1)
	if !r.post(func() { _, e := r.callString("setTheme", string(b)); done <- e }) {
		return fmt.Errorf("rt: eventloop 未运行")
	}
	select {
	case err := <-done:
		return err
	case <-r.closedCh:
		return fmt.Errorf("rt: 运行时已关闭")
	}
}

// Theme 返回当前主题令牌(副本)。
func (r *Runtime) Theme() map[string]any {
	r.themeMu.Lock()
	defer r.themeMu.Unlock()
	out := make(map[string]any, len(r.theme))
	for k, v := range r.theme {
		out[k] = v
	}
	return out
}

// OnHostEvent 注册宿主事件监听:插件 ctx.emitHost(ev, payload) 会以
// "plugin.<id>.<ev>" 的名字回调;宿主也可据此订阅插件命令。
// 回调在独立 goroutine 上执行(不在 JS loop 上,可安全调用 rt 方法)。
func (r *Runtime) OnHostEvent(fn func(name string, payload any)) {
	if fn == nil {
		return
	}
	r.hostMu.Lock()
	r.hostHooks = append(r.hostHooks, fn)
	r.hostMu.Unlock()
}

// Capabilities 返回已暴露的服务方法(service.method),供宿主文档化插件能力。
func (r *Runtime) Capabilities() []string {
	done := make(chan []string, 1)
	if !r.post(func() {
		out, _ := r.callString("serviceCapabilities")
		var names []string
		_ = json.Unmarshal([]byte(out), &names)
		sort.Strings(names)
		done <- names
	}) {
		return nil
	}
	select {
	case names := <-done:
		return names
	case <-r.closedCh:
		return nil
	}
}

func (r *Runtime) plugin(id string) (*loadedPlugin, bool) {
	r.pluginMu.Lock()
	defer r.pluginMu.Unlock()
	pl, ok := r.plugins[id]
	return pl, ok
}

func infoOf(pl *loadedPlugin) PluginInfo {
	m := pl.manifest
	return PluginInfo{
		ID:           m.ID,
		Name:         m.Name,
		Version:      m.Version,
		Entry:        m.Entry,
		Components:   append([]string(nil), pl.components...),
		Capabilities: append([]string(nil), m.Capabilities...),
		Slots:        append([]plugin.Contribution(nil), m.Slots...),
	}
}

// prefixPackage 复制组件包并把模块名统一前缀化,避免插件模块与宿主重名。
func prefixPackage(p *compile.Package, prefix string) *compile.Package {
	out := &compile.Package{
		Dir:      p.Dir,
		Entry:    p.Entry,
		Warnings: p.Warnings,
		Modules:  make(map[string][]byte, len(p.Modules)),
	}
	for name, code := range p.Modules {
		out.Modules[prefix+name] = code
	}
	for _, c := range p.Components {
		cc := *c
		cc.Module = prefix + c.Module
		out.Components = append(out.Components, &cc)
	}
	return out
}
