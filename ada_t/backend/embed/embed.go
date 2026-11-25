// Package embed 是「宿主自绘窗口」的渲染后端:插件的槽位树不新建原生窗口,
// 而是交给宿主在自己的 mygo ui.Context 里绘制。
//
// 用法(宿主侧):
//
//	b := embed.New(embed.Options{Theme: tokens})
//	app, _ := adat.NewApp(pkg, adat.AppOptions{Backend: b, SlotMode: true})
//	app.RegisterSlot(plugin.SlotSidebarItem)
//	app.LoadPlugin(...)          // 贡献自动挂载成 slot 视图
//	go app.Run()                 // embed.Run 阻塞到 App.Stop
//	// 宿主自己的 view:
//	func(c *ui.Context) { b.Draw(c, viewID) }
//
// 事件链路:Draw 里检测到点击/输入 → Dispatch(slotID, hid, payload) → rt.Fire。
package embed

import (
	"fmt"
	"sort"
	"sync"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/backend/mygoui"
	"github.com/issueye/ada_t/widget"
)

// Options 是后端参数。
type Options struct {
	// Theme 是宿主主题令牌(Palette/设计 token);由 App.SetTheme 下发到 JS,
	// 这里的副本只用于宿主查询与文档。
	Theme map[string]any
}

// Backend 是 embed 后端。
type Backend struct {
	mu       sync.Mutex
	trees    map[string]*widget.Node // slotID → 最新树
	dispatch backend.DispatchFunc
	theme    map[string]any
	renderer *mygoui.Renderer
	// toastSource 由 App 注入(rt 的 toast 队列);宿主可在帧末拉取并展示。
	toastSource func(slotID string) []string

	stopCh   chan struct{}
	stopOnce sync.Once
	stopped  bool
}

// New 创建 embed 后端。
func New(opts Options) *Backend {
	return &Backend{
		trees:    map[string]*widget.Node{},
		theme:    opts.Theme,
		renderer: mygoui.NewRenderer(),
		stopCh:   make(chan struct{}),
	}
}

// SetDispatch 注入事件回传(backend.Dispatcher)。
func (b *Backend) SetDispatch(fn backend.DispatchFunc) {
	b.mu.Lock()
	b.dispatch = fn
	b.renderer.SetDispatch(func(slotID, hid string, payload any) {
		b.mu.Lock()
		d := b.dispatch
		b.mu.Unlock()
		if d != nil {
			d(slotID, hid, payload)
		}
	})
	b.mu.Unlock()
}

// Apply 保存一个槽位视图的最新树(tree == nil 表示视图已卸载)。
func (b *Backend) Apply(slotID string, tree *widget.Node) {
	b.mu.Lock()
	if tree == nil {
		delete(b.trees, slotID)
	} else {
		b.trees[slotID] = tree
	}
	b.mu.Unlock()
	if tree == nil {
		b.renderer.Forget(slotID)
	}
}

// Draw 在宿主的 view 里绘制一个槽位视图;返回是否画了真实树。
// 只应在 mygo 主线程(view 回调)调用。
func (b *Backend) Draw(c *ui.Context, slotID string) bool {
	b.mu.Lock()
	tree := b.trees[slotID]
	b.mu.Unlock()
	return b.renderer.Render(c, slotID, tree)
}

// Tree 返回槽位当前树(nil 表示未就绪)。
func (b *Backend) Tree(slotID string) *widget.Node {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trees[slotID]
}

// Slots 返回当前有树的槽位视图 id(按 id 排序)。
func (b *Backend) Slots() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.trees))
	for id := range b.trees {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SetToastSource 实现 backend.ToastSource:App 注入 rt 的 toast 队列。
// 宿主负责把取到的提示画出来(例如窗口底部的 toast 条)。
func (b *Backend) SetToastSource(fn func(slotID string) []string) {
	b.mu.Lock()
	b.toastSource = fn
	b.mu.Unlock()
}

// DrainToasts 取走某槽位待显示的提示(插件 ctx.toast 入队)。
func (b *Backend) DrainToasts(slotID string) []string {
	b.mu.Lock()
	src := b.toastSource
	b.mu.Unlock()
	if src == nil {
		return nil
	}
	return src(slotID)
}

// Renderer 暴露底层渲染器(宿主需要自定义绘制/主题时使用)。
func (b *Backend) Renderer() *mygoui.Renderer { return b.renderer }

// Run 阻塞到 Stop:窗口与事件循环由宿主负责,后端只等待生命周期。
func (b *Backend) Run(specs []backend.WindowSpec) error {
	if len(specs) > 0 {
		return fmt.Errorf("embed: 不能创建原生窗口(%d 个 WindowSpec);槽位由宿主 Draw 承载", len(specs))
	}
	<-b.stopCh
	return nil
}

// Stop 结束 Run(可重复调用)。
func (b *Backend) Stop() error {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	b.stopOnce.Do(func() { close(b.stopCh) })
	return nil
}

// Fire 按「槽位视图 + 节点 id + 事件名」注入事件(测试/宿主模拟输入)。
func (b *Backend) Fire(slotID, nodeID, event string, payload any) bool {
	b.mu.Lock()
	tree := b.trees[slotID]
	fn := b.dispatch
	b.mu.Unlock()
	if tree == nil || fn == nil {
		return false
	}
	var hid string
	tree.Walk(func(n *widget.Node) bool {
		if hid != "" {
			return false
		}
		if n.ID == nodeID {
			hid = n.Events[event]
			return false
		}
		return true
	})
	if hid == "" {
		return false
	}
	fn(slotID, hid, payload)
	return true
}

// Theme 返回宿主主题令牌副本。
func (b *Backend) Theme() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]any, len(b.theme))
	for k, v := range b.theme {
		out[k] = v
	}
	return out
}

// SetTheme 更新并下发主题令牌。
func (b *Backend) SetTheme(tokens map[string]any) {
	b.mu.Lock()
	b.theme = tokens
	b.mu.Unlock()
}
