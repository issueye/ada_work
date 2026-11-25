// Package plugins 是 ada_chat 的插件宿主:把 ada_t 的组件包(插件)嵌进
// 这个窗口的侧栏、输入框、会话头部与设置页签。
//
// 边界:
//   - 插件只声明「槽位 + 组件」,位置/尺寸/主题由本包与 ui 决定;
//   - 插件只能调用 plugin.json 里声明过的 host.* 服务(默认拒绝);
//   - 插件事件名带 plugin.<id>. 前缀,多个插件不会串;
//   - 插件与宿主同进程,安全靠编译期受限表达式 + 能力白名单,不是进程隔离。
package plugins

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/embed"
	"github.com/issueye/ada_t/plugin"

	"ada_chat/internal/app"
)

// 宿主注册给插件的槽位。
var slots = []string{
	plugin.SlotSidebarItem,
	plugin.SlotComposerAction,
	plugin.SlotChatHeader,
	plugin.SlotSettingsTab,
	plugin.SlotCommand,
}

// BotInfo 是暴露给插件的机器人摘要。
type BotInfo struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	State     int    `json:"state"`
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
}

// SessionInfo 是暴露给插件的会话摘要。
type SessionInfo struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Preview string `json:"preview"`
	Unread  int    `json:"unread"`
	BotID   int    `json:"botId"`
}

// PrefsInfo 是暴露给插件的设置(脱敏:不含 API Key)。
type PrefsInfo struct {
	Name        string `json:"name"`
	EnterToSend bool   `json:"enterToSend"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Endpoint    string `json:"endpoint"`
}

// Snapshot 是每帧从 UI 线程发布给插件的只读快照;服务只读它,避免跨线程读 App。
type Snapshot struct {
	Bots        []BotInfo
	Sessions    []SessionInfo
	ActiveID    int
	ActiveTitle string
	Messages    int
	Prefs       PrefsInfo
}

// Host 持有 ada_t 应用、embed 后端与插件生命周期。
type Host struct {
	app      *app.App
	dispatch func(func())
	embed    *embed.Backend
	adat     *adat.App

	snap atomic.Pointer[Snapshot]

	mu      sync.Mutex
	themeID string
	stopped bool
}

// New 创建插件宿主(尚不装载插件)。dispatch 是宿主用来把改动送回 UI 线程的函数。
func New(a *app.App, dispatch func(func())) (*Host, error) {
	pkg, err := adat.CompileSource("Host.aui", "<template><Column><Text>host</Text></Column></template>")
	if err != nil {
		return nil, fmt.Errorf("编译宿主占位组件: %w", err)
	}
	b := embed.New(embed.Options{})
	aa, err := adat.NewApp(pkg, adat.AppOptions{
		Backend:  b,
		SlotMode: true,
		Logf:     func(f string, args ...any) { log.Printf("[plugin] "+f, args...) },
	})
	if err != nil {
		return nil, fmt.Errorf("创建插件运行时: %w", err)
	}
	h := &Host{app: a, dispatch: dispatch, embed: b, adat: aa}
	for _, s := range slots {
		if err := aa.RegisterSlot(s); err != nil {
			_ = aa.Stop()
			return nil, fmt.Errorf("注册槽位 %s: %w", s, err)
		}
	}
	h.exposeServices()
	aa.OnPluginEvent(h.onPluginEvent)
	aa.SetTheme(defaultTokens()) // 首帧前给一份默认主题,后续随外观切换
	return h, nil
}

// Start 启动 JS 世界(embed.Run 阻塞到 Stop)。
func (h *Host) Start() { go func() { _ = h.adat.Run() }() }

// Stop 停止 JS 世界并回收插件。
func (h *Host) Stop() {
	h.mu.Lock()
	stopped := h.stopped
	h.stopped = true
	h.mu.Unlock()
	if stopped {
		return
	}
	_ = h.adat.Stop()
}

// —— 插件发现与装载 ——

// LoadRoots 扫描若干目录:每个含 plugin.json 的子目录是一个插件;
// 根目录自身含 plugin.json 时也作为一个插件。
// 返回装载成功的插件 id 与每个失败项的错误(宿主不因单个插件失败而退出)。
func (h *Host) LoadRoots(roots ...string) ([]string, []error) {
	var loaded []string
	var errs []error
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, plugin.ManifestName)); err == nil {
			if id, err := h.LoadDir(root); err != nil {
				errs = append(errs, err)
			} else {
				loaded = append(loaded, id)
			}
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("读取插件目录 %s: %w", root, err))
			}
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(dir, plugin.ManifestName)); err != nil {
				continue
			}
			if id, err := h.LoadDir(dir); err != nil {
				errs = append(errs, err)
			} else {
				loaded = append(loaded, id)
			}
		}
	}
	sort.Strings(loaded)
	return loaded, errs
}

// LoadDir 编译并装载一个插件目录。
func (h *Host) LoadDir(dir string) (string, error) {
	pkg, man, err := adat.CompilePlugin(dir)
	if err != nil {
		return "", fmt.Errorf("编译插件 %s: %w", dir, err)
	}
	info, err := h.adat.LoadPlugin(pkg, man)
	if err != nil {
		return "", fmt.Errorf("装载插件 %s: %w", dir, err)
	}
	log.Printf("插件已装载: %s v%s(%d 个槽位贡献)", info.ID, info.Version, len(info.Slots))
	return info.ID, nil
}

// Unload 卸载一个插件(UI 下一帧起不再画它的槽位)。
func (h *Host) Unload(id string) error { return h.adat.UnloadPlugin(id) }

// Plugins 返回已装载插件摘要。
func (h *Host) Plugins() []adat.PluginInfo { return h.adat.Plugins() }

// —— 绘制与主题 ——

// Has 报告某槽位是否有插件内容。
func (h *Host) Has(slot string) bool {
	return h != nil && len(h.adat.SlotViews(slot)) > 0
}

// Draw 把某槽位的全部插件视图画进宿主当前的 ui.Context。
// 只在 mygo 主线程调用。
func (h *Host) Draw(c *ui.Context, slot string) {
	if h == nil {
		return
	}
	for _, v := range h.adat.SlotViews(slot) {
		h.embed.Draw(c, v.ID)
		// 插件 ctx.toast 的提示由宿主展示
		for _, msg := range h.embed.DrainToasts(v.ID) {
			feedback.ShowToast(c, feedback.ToastProps{Title: msg, Type: feedback.ToastInfo})
		}
	}
}

// SyncTheme 在主题(明暗)变化时把宿主 token 下发给插件。
// 异步执行,避免 UI 线程等待 JS loop(插件服务可能正等 UI 线程)。
func (h *Host) SyncTheme(tk *theme.Tokens) {
	key := "light"
	if tk.Dark {
		key = "dark"
	}
	h.mu.Lock()
	changed := h.themeID != key
	h.themeID = key
	h.mu.Unlock()
	if !changed {
		return
	}
	tokens := tokensOf(tk)
	go func() {
		if err := h.adat.SetTheme(tokens); err != nil {
			log.Printf("[plugin] 下发主题失败: %v", err)
		}
	}()
}

// Publish 在 UI 线程把当前 App 状态发布成快照(服务只读快照),
// 并在当前会话切换时向插件广播 session 事件。
func (h *Host) Publish(a *app.App) {
	s := &Snapshot{Prefs: PrefsInfo{
		Name: a.Prefs.Name, EnterToSend: a.Prefs.EnterToSend,
		Provider: a.Prefs.Provider, Model: a.Prefs.Model, Endpoint: a.Prefs.Endpoint,
	}}
	for _, b := range a.Bots {
		s.Bots = append(s.Bots, BotInfo{ID: b.ID, Name: b.Name, State: int(b.State), Workspace: b.Workspace, Model: b.Model})
	}
	for _, se := range a.Sessions {
		s.Sessions = append(s.Sessions, SessionInfo{ID: se.ID, Title: se.Title, Preview: se.Preview, Unread: se.Unread, BotID: se.BotID})
	}
	if cur := a.Current(); cur != nil {
		s.ActiveID = cur.ID
		s.ActiveTitle = cur.Title
		s.Messages = len(cur.Messages)
	}
	old := h.snap.Swap(s)
	if old != nil && old.ActiveID != s.ActiveID {
		for _, info := range h.adat.Plugins() {
		 _ = h.adat.EmitTo(info.ID, "session", map[string]any{"id": s.ActiveID, "title": s.ActiveTitle})
		}
	}
}

func (h *Host) snapshot() *Snapshot {
	if s := h.snap.Load(); s != nil {
		return s
	}
	return &Snapshot{}
}

// —— 宿主服务(插件按 capabilities 声明后调用) ——

func (h *Host) exposeServices() {
	_ = h.adat.ExposeHost("host", "bots.list", func() []BotInfo { return h.snapshot().Bots })
	_ = h.adat.ExposeHost("host", "sessions.list", func() []SessionInfo { return h.snapshot().Sessions })
	_ = h.adat.ExposeHost("host", "sessions.current", func() SessionInfo {
		s := h.snapshot()
		for _, se := range s.Sessions {
			if se.ID == s.ActiveID {
				return se
			}
		}
		return SessionInfo{}
	})
	_ = h.adat.ExposeHost("host", "chat.append", func(text string) (bool, error) {
		text = strings.TrimSpace(text)
		if text == "" {
			return false, fmt.Errorf("消息不能为空")
		}
		var ok bool
		h.onUI(func() { ok = h.app.AppendExternal(text, app.RoleAssistant) })
		if !ok {
			return false, fmt.Errorf("当前没有可用的会话")
		}
		return true, nil
	})
	_ = h.adat.ExposeHost("host", "settings.get", func() PrefsInfo { return h.snapshot().Prefs })
}

// onUI 把 fn 送到 UI 线程并等待完成(超时只记日志,不阻塞 JS loop 太久)。
// UI 线程从不反向等待 JS loop(主题走异步),因此不会互相死锁。
func (h *Host) onUI(fn func()) {
	if h.dispatch == nil {
		fn()
		return
	}
	done := make(chan struct{}, 1)
	h.dispatch(func() {
		fn()
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("[plugin] UI 线程 2s 未执行插件请求")
	}
}

// onPluginEvent 处理插件 ctx.emitHost 发来的事件(独立 goroutine)。
func (h *Host) onPluginEvent(pluginID, event string, payload any) {
	switch event {
	case "command":
		log.Printf("[plugin:%s] command %v", pluginID, payload)
	default:
		log.Printf("[plugin:%s] event %s %v", pluginID, event, payload)
	}
}

// —— 主题 token 映射 ——

func defaultTokens() map[string]any {
	return map[string]any{
		"dark":   false,
		"bg":     "#f8f8f8",
		"panel":  "#ffffff",
		"fg":     "#1f1f1f",
		"muted":  "#666666",
		"accent": "#333333",
		"border": "#e5e5e5",
		"radius": 6,
	}
}

func tokensOf(tk *theme.Tokens) map[string]any {
	return map[string]any{
		"dark":    tk.Dark,
		"bg":      hexOf(tk.BgLayout),
		"panel":   hexOf(tk.BgContainer),
		"elevated": hexOf(tk.BgElevated),
		"fg":      hexOf(tk.Text),
		"muted":   hexOf(tk.TextSecondary),
		"tertiary": hexOf(tk.TextTertiary),
		"accent":  hexOf(tk.Primary),
		"accentHover": hexOf(tk.PrimaryHover),
		"border":  hexOf(tk.Border),
		"radius":  float64(tk.BorderRadiusSM),
	}
}

func hexOf(c ui.Color) string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}
