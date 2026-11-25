package mygoui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/widget"
)

// TestReplaySplitKeepsBinding 回归:Split 的 float32 绑定表必须初始化,
// 否则首帧写 sF32 会 panic(assignment to entry in nil map)。
func TestReplaySplitKeepsBinding(t *testing.T) {
	tree := &widget.Node{
		ID: "split", Tag: "Split", Model: "state.size",
		Props: map[string]any{"size": float64(120), "value": float64(120)},
		Children: []*widget.Node{
			{ID: "left", Tag: "Column", Children: []*widget.Node{{ID: "lt", Tag: "Text", Props: map[string]any{"text": "左"}}}},
			{ID: "right", Tag: "Column", Children: []*widget.Node{{ID: "rt", Tag: "Text", Props: map[string]any{"text": "右"}}}},
		},
	}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, nil) }, 400, 240)
	if !tt.HasText("左") || !tt.HasText("右") {
		t.Fatalf("Split 两个面板未渲染: %v", tt.Texts())
	}
	in.mu.Lock()
	ptr := in.f32s["state.size"]
	in.mu.Unlock()
	if ptr == nil {
		t.Fatal("Split 的 float32 绑定未建立")
	}
	if *ptr != 120 {
		t.Fatalf("Split 绑定初值 = %v, want 120", *ptr)
	}
}

// TestReplayContainerClickReachesOuterRow 回归:布局容器只有在绑定 click 时
// 才是命中目标,内层无 handler 的 Column 不能吞掉外层 Row 的点击。
func TestReplayContainerClickReachesOuterRow(t *testing.T) {
	var fired []string
	fire := func(_ string, hid string, _ any) { fired = append(fired, hid) }
	tree := &widget.Node{
		ID: "row", Tag: "Row", Events: map[string]string{"click": "outer"},
		Children: []*widget.Node{
			{ID: "col", Tag: "Column", Props: map[string]any{"grow": true},
				Children: []*widget.Node{{ID: "txt", Tag: "Text", Props: map[string]any{"text": "会话名称"}}}},
		},
	}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, fire) }, 400, 240)
	if err := tt.Click("会话名称"); err != nil {
		t.Fatalf("点击失败: %v", err)
	}
	if len(fired) != 1 || fired[0] != "outer" {
		t.Fatalf("外层 Row 点击事件 = %v, want [outer]", fired)
	}
}

// TestReplayInnerButtonDoesNotFireOuterRow:内层按钮仍应吞掉点击,不冒泡到 Row。
func TestReplayInnerButtonDoesNotFireOuterRow(t *testing.T) {
	var fired []string
	fire := func(_ string, hid string, _ any) { fired = append(fired, hid) }
	tree := &widget.Node{
		ID: "row", Tag: "Row", Events: map[string]string{"click": "outer"},
		Children: []*widget.Node{
			{ID: "btn", Tag: "Button", Props: map[string]any{"label": "点我"}, Events: map[string]string{"click": "inner"}},
		},
	}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, fire) }, 400, 240)
	if err := tt.Click("点我"); err != nil {
		t.Fatalf("点击失败: %v", err)
	}
	if len(fired) != 1 || fired[0] != "inner" {
		t.Fatalf("按钮点击事件 = %v, want [inner](不应冒泡到外层)", fired)
	}
}

// TestParseColorFallsBackOnInvalidHex:非法 #hex 不能让 ui.Hex panic。
func TestParseColorFallsBackOnInvalidHex(t *testing.T) {
	got := parseColor("#12")
	want := ui.RGB(130, 130, 130)
	if got != want {
		t.Fatalf("非法颜色应回退 %v,得到 %v", want, got)
	}
	if parseColor("#abc") != ui.Hex("#abc") {
		t.Fatal("#abc 应正常解析")
	}
	if parseColor("#2f6fed") != ui.Hex("#2f6fed") {
		t.Fatal("#2f6fed 应正常解析")
	}
}

// TestReplayGrowLayoutApplied:输入控件的 grow 必须参与布局(此前被 applySize 丢弃)。
func TestReplayGrowLayoutApplied(t *testing.T) {
	mk := func(props map[string]any) *ui.Tester {
		tree := &widget.Node{ID: "row", Tag: "Row", Props: map[string]any{"width": float64(400)},
			Children: []*widget.Node{
				{ID: "in", Tag: "TextInput", Model: "state.q", Props: props},
				{ID: "tail", Tag: "Text", Props: map[string]any{"text": "|"}},
			}}
		in := newWinInput()
		return ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, nil) }, 420, 200)
	}
	grow := mk(map[string]any{"value": "abc", "grow": true})
	noGrow := mk(map[string]any{"value": "abc"})
	gr, ok1 := grow.Find("|")
	nr, ok2 := noGrow.Find("|")
	if !ok1 || !ok2 {
		t.Fatal("未找到尾部文本")
	}
	if gr.X <= nr.X+50 {
		t.Fatalf("grow 未生效:尾部文本 x=%v(无 grow 时 %v)", gr.X, nr.X)
	}
}

// TestReplayTextAlignUsesTextAlign:Text 的 align 走 TextAlign,
// 通过文字墨迹的水平位置验证(start/center/end 明显不同)。
func TestReplayTextAlignUsesTextAlign(t *testing.T) {
	ink := func(align string) (int, int) {
		tree := &widget.Node{ID: "col", Tag: "Column", Props: map[string]any{"width": float64(200)},
			Children: []*widget.Node{
				{ID: "t", Tag: "Text", Props: map[string]any{"text": "abc", "align": align}},
			}}
		tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", newWinInput(), nil) }, 220, 100)
		return inkBounds(tt)
	}
	startMin, _ := ink("start")
	centerMin, _ := ink("center")
	endMin, _ := ink("end")
	if !(startMin < centerMin && centerMin < endMin) {
		t.Fatalf("TextAlign 未生效:start/x=%d center/x=%d end/x=%d", startMin, centerMin, endMin)
	}
}

// TestReplayAllTagsSmoke:21 个原生标签都能在 Tester 里重放(布局属性统一走
// applyLayout 后,确认没有控件因新增调用而崩溃)。
func TestReplayAllTagsSmoke(t *testing.T) {
	col := func(id, text string) *widget.Node {
		return &widget.Node{ID: id, Tag: "Column", Children: []*widget.Node{{ID: id + "t", Tag: "Text", Props: map[string]any{"text": text}}}}
	}
	cases := []*widget.Node{
		{ID: "c", Tag: "Column", Props: map[string]any{"fill": true, "gap": float64(4), "padding": float64(4)}, Children: []*widget.Node{{ID: "ct", Tag: "Text", Props: map[string]any{"text": "col"}}}},
		{ID: "r", Tag: "Row", Props: map[string]any{"grow": true, "fill-x": true}, Children: []*widget.Node{{ID: "rt", Tag: "Text", Props: map[string]any{"text": "row"}}}},
		{ID: "s", Tag: "Scroll", Props: map[string]any{"grow": true}, Children: []*widget.Node{{ID: "st", Tag: "Text", Props: map[string]any{"text": "scroll"}}}},
		{ID: "t", Tag: "Text", Props: map[string]any{"text": "text", "font-size": float64(14), "bold": true, "align": "center", "grow": true}},
		{ID: "b", Tag: "Button", Props: map[string]any{"label": "btn", "grow": true}},
		{ID: "pb", Tag: "PrimaryButton", Props: map[string]any{"label": "pbtn"}},
		{ID: "ti", Tag: "TextInput", Model: "state.ti", Props: map[string]any{"value": "v", "placeholder": "p", "grow": true}},
		{ID: "ni", Tag: "NumberInput", Model: "state.ni", Props: map[string]any{"value": float64(1), "min": float64(0), "max": float64(9), "step": float64(1), "grow": true}},
		{ID: "sw", Tag: "Switch", Model: "state.sw", Props: map[string]any{"checked": true}},
		{ID: "cb", Tag: "Checkbox", Model: "state.cb", Props: map[string]any{"checked": false, "label": "check"}},
		{ID: "pr", Tag: "Progress", Props: map[string]any{"value": 0.5, "width": float64(120)}},
		{ID: "sl", Tag: "Slider", Model: "state.sl", Props: map[string]any{"value": float64(5), "min": float64(0), "max": float64(10), "grow": true}},
		{ID: "sp", Tag: "Stepper", Model: "state.sp", Props: map[string]any{"value": float64(2), "grow": true}},
		{ID: "seg", Tag: "Segmented", Model: "state.seg", Props: map[string]any{"options": "A,B", "value": "A", "fill-x": true}},
		{ID: "tab", Tag: "Tabs", Model: "state.tab", Props: map[string]any{"options": "X,Y", "value": "X", "fill-x": true}},
		{ID: "split", Tag: "Split", Model: "state.split", Props: map[string]any{"value": float64(100)},
			Children: []*widget.Node{col("sl1", "left"), col("sr1", "right")}},
		{ID: "tb", Tag: "Table", Model: "state.tb",
			Props: map[string]any{"rows": []any{map[string]any{"a": "r1"}, map[string]any{"a": "r2"}}, "columns": "A", "fields": "a", "selected": float64(0), "grow": true}},
		{ID: "ra", Tag: "Rating", Model: "state.ra", Props: map[string]any{"selected": float64(3), "max": float64(5)}},
		{ID: "ad", Tag: "AlertDialog", Model: "state.ad", Props: map[string]any{"open": false, "title": "t", "message": "m", "buttons": "OK"}},
		{ID: "sp2", Tag: "Spacer"},
		{ID: "dv", Tag: "Divider"},
	}
	for _, node := range cases {
		node := node
		t.Run(node.Tag, func(t *testing.T) {
			in := newWinInput()
			tt := ui.NewTester(func(c *ui.Context) { replay(c, node, "w", in, nil) }, 400, 240)
			if len(tt.Texts()) == 0 && node.Tag != "Spacer" && node.Tag != "Divider" {
				// 部分控件(输入/选择)不产生文本标签,能渲染完一帧即通过。
				t.Logf("%s 无文本标签(允许)", node.Tag)
			}
		})
	}
}

// inkBounds 返回画面中深色像素(文字墨迹)的水平范围。
func inkBounds(tt *ui.Tester) (minX, maxX int) {
	img := tt.Image()
	b := img.Bounds()
	minX, maxX = -1, -1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if a == 0 || !(r < 0x8000 && g < 0x8000 && bl < 0x8000) {
				continue
			}
			if minX == -1 || x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
		}
	}
	return minX, maxX
}
