// Package headless 是无 GUI 的渲染后端:保存每窗口最新树快照,
// 记录帧日志(Apply 序列),供测试与 `adat render` 在无显示环境验证编译层闭环。
//
// 事件由测试注入:Fire(nodeID, event, payload) 按当前树查 handler id,
// 经 DispatchFunc 送回 rt(等价于真实后端把原生事件翻译成 handler 调用)。
package headless

import (
	"fmt"
	"sync"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/widget"
)

// Backend 是 headless 实现。
type Backend struct {
	mu       sync.Mutex
	trees    map[string]*widget.Node
	frames   map[string]int
	log      []string
	dispatch backend.DispatchFunc
	running  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New 创建 headless 后端。
func New() *Backend {
	return &Backend{
		trees:  map[string]*widget.Node{},
		frames: map[string]int{},
		stopCh: make(chan struct{}),
	}
}

// SetDispatch 注入事件回传(backend.Dispatcher)。
func (b *Backend) SetDispatch(fn backend.DispatchFunc) {
	b.mu.Lock()
	b.dispatch = fn
	b.mu.Unlock()
}

// Apply 保存窗口树并记一帧。
func (b *Backend) Apply(winID string, tree *widget.Node) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trees[winID] = tree
	b.frames[winID]++
	b.log = append(b.log, fmt.Sprintf("apply %s frame=%d", winID, b.frames[winID]))
}

// Run 阻塞到 Stop(headless 没有主线程事件循环)。
func (b *Backend) Run(windows []backend.WindowSpec) error {
	b.mu.Lock()
	b.running = true
	b.mu.Unlock()
	<-b.stopCh
	return nil
}

// Stop 退出 Run。可重复调用;在 Run 之前调用也安全(Run 会立即返回),
// 不会留下一个永远等不到信号的阻塞。
func (b *Backend) Stop() error {
	b.mu.Lock()
	b.running = false
	b.mu.Unlock()
	b.stopOnce.Do(func() { close(b.stopCh) })
	return nil
}

// Tree 返回窗口当前树快照(nil 表示没有)。
func (b *Backend) Tree(winID string) *widget.Node {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trees[winID]
}

// FrameCount 返回窗口收到的 Apply 次数。
func (b *Backend) FrameCount(winID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frames[winID]
}

// Log 返回操作日志副本。
func (b *Backend) Log() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.log))
	copy(out, b.log)
	return out
}

// Fire 按「节点 id + 事件名」注入事件:在当前树里查 handler id 回传。
// 返回是否找到对应 handler。
func (b *Backend) Fire(winID, nodeID, event string, payload any) bool {
	b.mu.Lock()
	tree := b.trees[winID]
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
	fn(winID, hid, payload)
	return true
}
