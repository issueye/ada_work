package mygoui

// Renderer 把 widget 树画进「宿主自己提供的 ui.Context」,而不是新建窗口。
//
// 用途:宿主(
// 例如 ada_chat)已经有一个 mygo 窗口与 immediate-mode 视图,想把插件的
// 槽位内容嵌进去;此时插件树的输入状态(文本/数值/选中)按 slot id 独立保存,
// 点击/输入事件仍然经 Dispatch 回传给 JS。
//
// 线程:Render 只在宿主主线程(view 回调)调用;输入状态表自带互斥。

import (
	"sync"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/lobe"
	"github.com/issueye/ada_t/widget"
)

// Renderer 是「渲染进现有 Context」的重放器。
type Renderer struct {
	mu        sync.Mutex
	inputs    map[string]*winInput
	dispatch  backend.DispatchFunc
	noop      backend.DispatchFunc
	lobeOn    bool        // 渲染前应用 lobe-mygo 原生主题(默认关闭:宿主自管 mygo 主题)
	lobeTheme *lobe.Theme // nil 用 lobe.Default()
}

// NewRenderer 创建渲染器。
func NewRenderer() *Renderer {
	return &Renderer{
		inputs: map[string]*winInput{},
		noop:   func(string, string, any) {},
	}
}

// SetDispatch 注入事件回传(槽位 id 会作为 winID 传给回调;通常再转 rt.Fire)。
func (r *Renderer) SetDispatch(fn backend.DispatchFunc) {
	r.mu.Lock()
	r.dispatch = fn
	r.mu.Unlock()
}

// SetLobeTheme 开启 lobe-mygo 原生主题:每帧渲染前应用;
// t 为 nil 时用 lobe.Default()。关闭见 DisableLobeTheme。
func (r *Renderer) SetLobeTheme(t *lobe.Theme) {
	r.mu.Lock()
	r.lobeOn = true
	r.lobeTheme = t
	r.mu.Unlock()
}

// DisableLobeTheme 关闭 lobe-mygo 原生主题(默认即关闭)。
func (r *Renderer) DisableLobeTheme() {
	r.mu.Lock()
	r.lobeOn = false
	r.mu.Unlock()
}

func (r *Renderer) inputFor(slot string) *winInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, ok := r.inputs[slot]
	if !ok {
		in = newWinInput()
		r.inputs[slot] = in
	}
	return in
}

// Render 把 n 画进 c;n 为 nil 时画占位文本(槽位未就绪/已卸载)。
// 返回是否画了真实树。
func (r *Renderer) Render(c *ui.Context, slot string, n *widget.Node) bool {
	r.mu.Lock()
	dispatch := r.dispatch
	if dispatch == nil {
		dispatch = r.noop
	}
	lobeOn := r.lobeOn
	lobeTheme := r.lobeTheme
	r.mu.Unlock()
	if lobeOn {
		if lobeTheme == nil {
			lobeTheme = lobe.Default()
		}
		lobeTheme.ApplyNative(c)
	}
	if n == nil {
		ui.Text(c, "…").Center()
		return false
	}
	replay(c, n, slot, r.inputFor(slot), dispatch)
	return true
}

// Forget 释放一个槽位的输入状态(插件卸载时调用,避免绑定表泄漏)。
func (r *Renderer) Forget(slot string) {
	r.mu.Lock()
	delete(r.inputs, slot)
	r.mu.Unlock()
}
