// Package mygoui 把 widget 树重放为 mygo 原生控件(GPU 绘制,无 webview)。
//
// 线程模型:
//   - rt 渲染桥在 loop goroutine 上 Apply 最新树(本包存引用 + Invalidate);
//   - mygo 主线程每帧调用 view:把当前树同步重放为 ui.* 调用(immediate mode);
//   - Clicked()/输入变化在本帧检测,经 Dispatch 回传 rt(跨 goroutine 投递)。
//
// 输入绑定(a-model):后端持有每绑定键的 *string/*bool/*float64(跨帧稳定);
// 每帧先检测用户编辑(回传事件),再同步 JS 值(无未上报编辑时)。
package mygoui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/internal/colorval"
	"github.com/issueye/ada_t/widget"
)

// Backend 是 mygo 原生渲染后端。
type Backend struct {
	mu          sync.Mutex
	trees       map[string]*widget.Node
	dispatch    backend.DispatchFunc
	toastSource func(winID string) []string // 每帧取走待显示 toast(nil 忽略)
}

// New 创建 mygo 后端。
func New() *Backend {
	return &Backend{trees: map[string]*widget.Node{}}
}

// SetToastSource 注入 toast 来源:重放帧时按窗口取走待显示的提示文案
// (App 会把它接到 rt 的 toast 队列上)。
func (b *Backend) SetToastSource(fn func(winID string) []string) {
	b.mu.Lock()
	b.toastSource = fn
	b.mu.Unlock()
}

// SetDispatch 注入事件回传(backend.Dispatcher)。
func (b *Backend) SetDispatch(fn backend.DispatchFunc) {
	b.mu.Lock()
	b.dispatch = fn
	b.mu.Unlock()
}

// windowReg 保存已创建的原生窗口,供 Apply 请求重绘。
var (
	regMu     sync.Mutex
	windowReg = map[string]*mygo.Window{}
)

// Apply 保存窗口最新树并请求重绘(Invalidate 协程安全)。
// tree == nil 表示窗口已移除:删除树与注册,并关闭原生窗口
// (契约见 backend.Backend.Apply;rt.UnmountWindow 会发 nil 树)。
func (b *Backend) Apply(winID string, tree *widget.Node) {
	b.mu.Lock()
	if tree == nil {
		delete(b.trees, winID)
	} else {
		b.trees[winID] = tree
	}
	b.mu.Unlock()
	regMu.Lock()
	w := windowReg[winID]
	if tree == nil {
		delete(windowReg, winID)
	}
	regMu.Unlock()
	if w == nil {
		return
	}
	if tree == nil {
		w.Close()
		return
	}
	w.Invalidate()
}

// Run 启动 mygo 应用并阻塞(占主线程)。
func (b *Backend) Run(wins []backend.WindowSpec) error {
	inputs := map[string]*winInput{}
	for _, w := range wins {
		inputs[w.ID] = newWinInput()
	}

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
			in := inputs[w.ID]
			id := w.ID
			view := func(c *ui.Context) {
				b.mu.Lock()
				tree := b.trees[id]
				src := b.toastSource
				b.mu.Unlock()
				if tree == nil {
					ui.Text(c, "…").Center()
					return
				}
				replay(c, tree, id, in, fire)
				// 组件 ctx.toast 入队的提示:帧末下发(几秒自动消失)
				if src != nil {
					for _, msg := range src(id) {
						c.Toast(msg)
					}
				}
			}
			win := mygo.NewWindow(mygo.WindowOptions{
				Title:           w.Title,
				Width:           w.Width,
				Height:          w.Height,
				BackgroundColor: w.BackgroundColor,
				Content:         ui.View(view),
			})
			regMu.Lock()
			windowReg[w.ID] = win
			regMu.Unlock()
		}
	})
	return mygo.App.Run()
}

// Stop 请求退出 mygo 主循环。
func (b *Backend) Stop() error {
	mygo.App.Quit()
	return nil
}

// —— 输入绑定状态(a-model 双向同步) ——

type winInput struct {
	mu    sync.Mutex
	texts map[string]*string
	nums  map[string]*float64
	bools map[string]*bool
	idxs  map[string]*int
	f32s  map[string]*float32
	lists map[string]*ui.ListState
	sText map[string]string
	sNum  map[string]float64
	sBool map[string]bool
	sIdx  map[string]int
	sF32  map[string]float32
}

func newWinInput() *winInput {
	return &winInput{
		texts: map[string]*string{}, nums: map[string]*float64{}, bools: map[string]*bool{},
		idxs: map[string]*int{}, f32s: map[string]*float32{}, lists: map[string]*ui.ListState{},
		sText: map[string]string{}, sNum: map[string]float64{}, sBool: map[string]bool{},
		sIdx: map[string]int{}, sF32: map[string]float32{},
	}
}

// syncText 取文本绑定指针;先回报用户编辑,再同步 JS 值。
func (w *winInput) syncText(key, jsVal string, report func(any)) *string {
	w.mu.Lock()
	defer w.mu.Unlock()
	ptr, ok := w.texts[key]
	if !ok {
		s := jsVal
		w.texts[key] = &s
		w.sText[key] = jsVal
		return &s
	}
	cur := *ptr
	shadow := w.sText[key]
	if cur != shadow {
		w.sText[key] = cur
		if report != nil {
			report(map[string]any{"value": cur})
		}
		return ptr
	}
	if jsVal != shadow {
		*ptr = jsVal
		w.sText[key] = jsVal
	}
	return ptr
}

func (w *winInput) syncNum(key string, jsVal float64, report func(any)) *float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ptr, ok := w.nums[key]
	if !ok {
		s := jsVal
		w.nums[key] = &s
		w.sNum[key] = jsVal
		return &s
	}
	cur := *ptr
	shadow := w.sNum[key]
	if cur != shadow {
		w.sNum[key] = cur
		if report != nil {
			report(map[string]any{"value": cur})
		}
		return ptr
	}
	if jsVal != shadow {
		*ptr = jsVal
		w.sNum[key] = jsVal
	}
	return ptr
}

func (w *winInput) syncBool(key string, jsVal bool, report func(any)) *bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	ptr, ok := w.bools[key]
	if !ok {
		s := jsVal
		w.bools[key] = &s
		w.sBool[key] = jsVal
		return &s
	}
	cur := *ptr
	shadow := w.sBool[key]
	if cur != shadow {
		w.sBool[key] = cur
		if report != nil {
			report(map[string]any{"value": cur})
		}
		return ptr
	}
	if jsVal != shadow {
		*ptr = jsVal
		w.sBool[key] = jsVal
	}
	return ptr
}

// syncIdx 是 Segmented 的整数选择绑定(镜像 syncBool 的双向语义)。
func (w *winInput) syncIdx(key string, jsVal int, report func(any)) *int {
	w.mu.Lock()
	defer w.mu.Unlock()
	ptr, ok := w.idxs[key]
	if !ok {
		s := jsVal
		w.idxs[key] = &s
		w.sIdx[key] = jsVal
		return &s
	}
	cur := *ptr
	shadow := w.sIdx[key]
	if cur != shadow {
		w.sIdx[key] = cur
		if report != nil {
			report(map[string]any{"value": cur})
		}
		return ptr
	}
	if jsVal != shadow {
		*ptr = jsVal
		w.sIdx[key] = jsVal
	}
	return ptr
}

// —— 重放 ——

// replay 重放一个节点为新帧的原生控件;新增实例标记(siv)滚入视野。
func replay(c *ui.Context, n *widget.Node, winID string, in *winInput, fire backend.DispatchFunc) {
	el := replayEl(c, n, winID, in, fire)
	if n.ScrollIntoView {
		el = el.ScrollIntoView()
	}
	_ = el
}

func replayEl(c *ui.Context, n *widget.Node, winID string, in *winInput, fire backend.DispatchFunc) ui.Element {
	switch n.Tag {
	case "Column", "Row":
		var el ui.Element
		if n.Tag == "Column" {
			el = ui.Column(c)
		} else {
			el = ui.Row(c)
		}
		el = applyLayout(el, n.Props)
		el.Children(func() {
			for _, ch := range n.Children {
				replay(c, ch, winID, in, fire)
			}
		})
		// 只有显式绑定 click 的容器才注册可点击命中。若无条件调用 Clicked(),
		// 每个布局容器都会成为最内层命中目标,吞掉外层容器的点击
		// (如 Sidebar 的会话行:Row[click] > Column > Text)。
		if n.Events["click"] != "" && el.Clicked() {
			fireClick(n, winID, fire)
		}
		return el
	case "Text":
		el := ui.Text(c, n.Text())
		if v, ok := propFloat(n.Props, "font-size"); ok {
			el = el.FontSize(float32(v))
		}
		if propBool(n.Props, "bold") {
			el = el.Bold()
		}
		if v, ok := n.Props["color"].(string); ok {
			el = el.TextColor(parseColor(v))
		}
		// align 对 Text 是行内文本对齐(TextAlign);applyLayout 的 AlignItems
		// 是子项对齐,对没有子节点的 Text 无效。
		if v, ok := n.Props["align"].(string); ok {
			el = el.TextAlign(parseAlign(v))
		}
		return applyLayout(el, n.Props)
	case "Button":
		el := applyLayout(ui.Button(c, n.Text()), n.Props)
		if el.Clicked() {
			fireClick(n, winID, fire)
		}
		return el
	case "PrimaryButton":
		el := applyLayout(ui.PrimaryButton(c, n.Text()), n.Props)
		if el.Clicked() {
			fireClick(n, winID, fire)
		}
		return el
	case "TextInput":
		key := modelKeyOr(n, "text:")
		jsVal := strProp(n.Props, "value")
		ptr := in.syncText(key, jsVal, func(p any) { fireEvent(n, winID, "input", p, fire) })
		el := ui.TextInput(c, ptr)
		if v, ok := n.Props["placeholder"].(string); ok {
			el = el.Placeholder(v)
		}
		el = applyLayout(el, n.Props)
		// 回车提交(a-on:submit):Submitted 是本帧内的查询
		if el.Submitted() {
			fireEvent(n, winID, "submit", map[string]any{"value": *ptr}, fire)
		}
		return el
	case "NumberInput":
		key := modelKeyOr(n, "num:")
		lo, hi, step := 0.0, 100.0, 1.0
		if v, ok := propFloat(n.Props, "min"); ok {
			lo = v
		}
		if v, ok := propFloat(n.Props, "max"); ok {
			hi = v
		}
		if v, ok := propFloat(n.Props, "step"); ok {
			step = v
		}
		ptr := in.syncNum(key, numProp(n.Props, "value"), func(p any) { fireEvent(n, winID, "input", p, fire) })
		return applyLayout(ui.NumberInput(c, ptr, lo, hi, step), n.Props)
	case "Switch":
		key := modelKeyOr(n, "bool:")
		ptr := in.syncBool(key, propBool(n.Props, "checked"), func(p any) { fireEvent(n, winID, "change", p, fire) })
		return ui.Switch(c, ptr)
	case "Checkbox":
		key := modelKeyOr(n, "bool:")
		ptr := in.syncBool(key, propBool(n.Props, "checked"), func(p any) { fireEvent(n, winID, "change", p, fire) })
		return ui.Checkbox(c, ptr, n.Text())
	case "Progress":
		return applyLayout(ui.Progress(c, numProp(n.Props, "value")), n.Props)
	case "Slider":
		// 数值滑杆:拖动经 Changed 帧查询回传 input 事件
		key := modelKeyOr(n, "num:")
		lo, hi := 0.0, 100.0
		if v, ok := propFloat(n.Props, "min"); ok {
			lo = v
		}
		if v, ok := propFloat(n.Props, "max"); ok {
			hi = v
		}
		ptr := in.syncNum(key, numProp(n.Props, "value"), nil)
		el := applyLayout(ui.Slider(c, ptr, lo, hi), n.Props)
		if el.Changed() {
			fireEvent(n, winID, "input", map[string]any{"value": *ptr}, fire)
		}
		return el
	case "Stepper":
		// 步进器:箭头点按/按住连续步进
		key := modelKeyOr(n, "num:")
		lo, hi, step := 0.0, 100.0, 1.0
		if v, ok := propFloat(n.Props, "min"); ok {
			lo = v
		}
		if v, ok := propFloat(n.Props, "max"); ok {
			hi = v
		}
		if v, ok := propFloat(n.Props, "step"); ok {
			step = v
		}
		ptr := in.syncNum(key, numProp(n.Props, "value"), nil)
		el := applyLayout(ui.Stepper(c, ptr, lo, hi, step), n.Props)
		if el.Changed() {
			fireEvent(n, winID, "input", map[string]any{"value": *ptr}, fire)
		}
		return el
	case "Segmented", "Tabs":
		// 分段/页签:options 逗号分隔;model 值 = 选中项文本(choice,change 事件回传)
		var labels []string
		if opts, ok := n.Props["options"].(string); ok && opts != "" {
			labels = strings.Split(opts, ",")
		}
		if len(labels) == 0 {
			return ui.Spacer(c)
		}
		key := modelKeyOr(n, "choice:")
		jsVal := strProp(n.Props, "value")
		idx := 0
		for i, l := range labels {
			if l == jsVal {
				idx = i
				break
			}
		}
		// 用户选择 → change 事件带选中文本
		fireChoice := func(i int) {
			if i >= 0 && i < len(labels) {
				fireEvent(n, winID, "change", map[string]any{"value": labels[i]}, fire)
			}
		}
		// 用户选择由 el.Changed() 统一上报(syncIdx 不再重复回报)。
		ptr := in.syncIdx(key, idx, nil)
		var el ui.Element
		if n.Tag == "Segmented" {
			el = applyLayout(ui.Segmented(c, ptr, labels...), n.Props)
		} else {
			el = applyLayout(ui.Tabs(c, ptr, labels...), n.Props)
		}
		if el.Changed() {
			fireChoice(*ptr)
		}
		return el
	case "Split":
		// 可拖拽分栏:size = 第一面板宽度(DIP,a-model 双向);
		// vertical 属性变上下分栏;必须恰好两个容器子节点
		key := modelKeyOr(n, "split:")
		def := numProp(n.Props, "size")
		if v, ok := propFloat(n.Props, "value"); ok {
			def = v // a-model 的值优先
		}
		ptr := in.syncNum32(key, def, nil)
		if len(n.Children) != 2 {
			return ui.Text(c, "[Split 需要两个子节点]")
		}
		first, second := n.Children[0], n.Children[1]
		var el ui.Element
		if propBool(n.Props, "vertical") {
			el = ui.SplitVertical(c, ptr,
				func() { replay(c, first, winID, in, fire) },
				func() { replay(c, second, winID, in, fire) })
		} else {
			el = ui.Split(c, ptr,
				func() { replay(c, first, winID, in, fire) },
				func() { replay(c, second, winID, in, fire) })
		}
		el = applyLayout(el, n.Props)
		if el.Changed() {
			fireEvent(n, winID, "input", map[string]any{"value": *ptr}, fire)
		}
		return el
	case "Scroll":
		// 垂直滚动容器;内容多时滚动,配合 grow 填满剩余空间
		el := applyLayout(ui.Scroll(c), n.Props)
		el.Children(func() {
			for _, ch := range n.Children {
				replay(c, ch, winID, in, fire)
			}
		})
		return el
	case "Table":
		// 虚拟化表格:rows(对象数组)× columns(标题)× fields(行字段);
		// a-model = 选中行下标(双向);follow-end 使新行自动滚入视野。
		// ListState 按绑定键保留(每表一个,跨帧持有选中与滚动位置)。
		var rows []any
		if rv, ok := n.Props["rows"].([]any); ok {
			rows = rv
		}
		var cols []ui.TableColumn
		var fields []string
		if cs, ok := n.Props["columns"].(string); ok && cs != "" {
			fs := ""
			if f, ok2 := n.Props["fields"].(string); ok2 {
				fs = f
			}
			titles := strings.Split(cs, ",")
			fields = strings.Split(fs, ",")
			for i, t := range titles {
				col := ui.TableColumn{Title: strings.TrimSpace(t)}
				if i < len(fields) {
					fields[i] = strings.TrimSpace(fields[i])
				} else {
					fields = append(fields, "")
				}
				cols = append(cols, col)
			}
		}
		if len(cols) == 0 {
			return ui.Spacer(c)
		}
		key := modelKeyOr(n, "table:")
		s, ok := in.lists[key]
		if !ok {
			s = &ui.ListState{}
			in.lists[key] = s
		}
		s.FollowEnd = propBool(n.Props, "follow-end")
		selPtr := in.syncIdx(key+"", int(numProp(n.Props, "selected")), nil)
		s.Selected = selPtr
		el := applyLayout(ui.Table(c, s, cols, len(rows), func(row int, col int) {
			if row < 0 || row >= len(rows) || col < 0 || col >= len(fields) {
				return
			}
			var text string
			if obj, ok2 := rows[row].(map[string]any); ok2 {
				text = fmt.Sprint(obj[fields[col]])
			} else {
				text = fmt.Sprint(rows[row])
			}
			ui.Text(c, text)
		}), n.Props)
		if el.Changed() {
			fireEvent(n, winID, "change", map[string]any{"value": *selPtr}, fire)
		}
		return el
	case "Rating":
		// 星级评分:a-model = 星数(int,双向);max 属性为星级上限
		key := modelKeyOr(n, "idx:")
		maxStars := 5.0
		if v, ok := propFloat(n.Props, "max"); ok {
			maxStars = v
		}
		ptr := in.syncIdx(key, int(numProp(n.Props, "selected")), nil)
		el := applyLayout(ui.Rating(c, ptr, int(maxStars)), n.Props)
		if el.Changed() {
			fireEvent(n, winID, "change", map[string]any{"value": *ptr}, fire)
		}
		return el
	case "Spacer":
		return ui.Spacer(c)
	case "Divider":
		return ui.Divider(c)
	case "AlertDialog":
		// 模态确认框:open 双向绑定(a-model),按钮点击/Escape 经事件回传
		key := modelKeyOr(n, "open:")
		title, _ := n.Props["title"].(string)
		message, _ := n.Props["message"].(string)
		var labels []string
		if bs, ok := n.Props["buttons"].(string); ok && bs != "" {
			labels = strings.Split(bs, ",")
		}
		ptr := in.syncBool(key, propBool(n.Props, "open"), func(p any) { fireEvent(n, winID, "change", p, fire) })
		idx := ui.AlertDialog(c, ptr, title, message, labels...)
		switch {
		case idx >= 0 && idx < len(labels):
			fireEvent(n, winID, "action", map[string]any{"index": float64(idx), "label": labels[idx]}, fire)
		case idx == ui.AlertDismissed:
			fireEvent(n, winID, "dismiss", nil, fire)
		}
		return ui.Spacer(c) // 弹层本体由 mygo 管理;布局占位为零高
	default:
		// 未知标签:占位文本,不崩溃
		return ui.Text(c, "["+n.Tag+"]")
	}
}

func modelKeyOr(n *widget.Node, prefix string) string {
	if n.Model != "" {
		return n.Model
	}
	return prefix + n.ID
}

func fireClick(n *widget.Node, winID string, fire backend.DispatchFunc) {
	if hid, ok := n.Events["click"]; ok && hid != "" {
		fire(winID, hid, nil)
	}
}

func fireEvent(n *widget.Node, winID, ev string, payload any, fire backend.DispatchFunc) {
	if hid, ok := n.Events[ev]; ok && hid != "" {
		fire(winID, hid, payload)
	}
}

// —— 属性映射 ——

func applyLayout(el ui.Element, props map[string]any) ui.Element {
	if propBool(props, "fill") {
		el = el.Fill()
	}
	if propBool(props, "fill-x") {
		el = el.FillWidth()
	}
	if propBool(props, "fill-y") {
		el = el.FillHeight()
	}
	if propBool(props, "center") {
		el = el.Center()
	}
	if v, ok := propFloat(props, "gap"); ok {
		el = el.Gap(float32(v))
	}
	if v, ok := propFloat(props, "gap-x"); ok {
		el = el.GapX(float32(v))
	}
	if v, ok := propFloat(props, "gap-y"); ok {
		el = el.GapY(float32(v))
	}
	if v, ok := propFloat(props, "padding"); ok {
		el = el.Padding(float32(v))
	}
	if v, ok := propFloat(props, "grow"); ok {
		el = el.Grow(float32(v))
	}
	if v, ok := props["align"].(string); ok {
		el = el.AlignItems(parseAlign(v))
	}
	if v, ok := props["justify"].(string); ok {
		el = el.Justify(parseAlign(v))
	}
	if v, ok := props["background"].(string); ok {
		el = el.Background(parseColor(v))
	}
	if v, ok := propFloat(props, "radius"); ok {
		el = el.Radius(float32(v))
	}
	return applySize(el, props)
}

func applySize(el ui.Element, props map[string]any) ui.Element {
	if v, ok := propFloat(props, "width"); ok {
		el = el.Width(float32(v))
	}
	if v, ok := propFloat(props, "height"); ok {
		el = el.Height(float32(v))
	}
	return el
}

func parseAlign(s string) ui.Align {
	switch s {
	case "start":
		return ui.Start
	case "center":
		return ui.Center
	case "end":
		return ui.End
	case "stretch":
		return ui.Stretch
	case "space-between":
		return ui.SpaceBetween
	case "space-around":
		return ui.SpaceAround
	case "space-evenly":
		return ui.SpaceEvenly
	case "baseline":
		return ui.Baseline
	}
	return ui.Start
}

// parseColor 支持 #rgb/#rgba/#rrggbb/#rrggbbaa 与有限命名色(令牌化之前的过渡)。
// 非法 #hex 在编译期已拒绝;这里再兜底一次,避免 ui.Hex panic 带走主线程
// (动态 a-bind 的颜色值绕过了编译期校验)。
func parseColor(s string) ui.Color {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "#") {
		if colorval.ValidHex(s) {
			return ui.Hex(s)
		}
		return ui.RGB(130, 130, 130)
	}
	switch strings.ToLower(s) {
	case "white":
		return ui.RGB(255, 255, 255)
	case "black":
		return ui.RGB(10, 10, 10)
	case "red":
		return ui.RGB(220, 60, 60)
	case "green":
		return ui.RGB(60, 170, 90)
	case "blue":
		return ui.RGB(60, 110, 220)
	case "gray", "grey":
		return ui.RGB(130, 130, 130)
	}
	return ui.RGB(0, 0, 0)
}

func strProp(props map[string]any, key string) string {
	if v, ok := props[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func numProp(props map[string]any, key string) float64 {
	if v, ok := propFloat(props, key); ok {
		return v
	}
	return 0
}

func propFloat(props map[string]any, key string) (float64, bool) {
	switch v := props[key].(type) {
	case float64:
		return v, true
	case bool:
		// 无值属性(grow)coerce 成 true;grow=true 即 1 倍
		if v {
			return 1, true
		}
		return 0, false
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func propBool(props map[string]any, key string) bool {
	switch v := props[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// syncNum32 是 float32 数值绑定(Split 的面板尺寸;mygo 用 *float32)。
// 镜像 syncBool 的双向语义。
func (w *winInput) syncNum32(key string, jsVal float64, report func(any)) *float32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	ptr, ok := w.f32s[key]
	if !ok {
		s := float32(jsVal)
		w.f32s[key] = &s
		w.sF32[key] = float32(jsVal)
		return &s
	}
	cur := *ptr
	shadow := w.sF32[key]
	if cur != shadow {
		w.sF32[key] = cur
		if report != nil {
			report(map[string]any{"value": cur})
		}
		return ptr
	}
	if float32(jsVal) != shadow {
		*ptr = float32(jsVal)
		w.sF32[key] = float32(jsVal)
	}
	return ptr
}
