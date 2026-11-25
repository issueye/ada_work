package mygoui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/widget"
)

func TestReplayCheckboxAndSegmented(t *testing.T) {
	checkbox := &widget.Node{ID: "c", Tag: "Checkbox", Model: "state.on",
		Props:  map[string]any{"checked": false, "label": "On"},
		Events: map[string]string{"change": "hc"}}
	seg := &widget.Node{ID: "s", Tag: "Segmented", Model: "state.choice",
		Props:  map[string]any{"options": "A,B,C", "value": "A"},
		Events: map[string]string{"change": "hs"}}
	tree := &widget.Node{Tag: "Column", Children: []*widget.Node{checkbox, seg}}
	var payloads []any
	fire := func(_ string, hid string, payload any) {
		if hid == "hc" || hid == "hs" {
			payloads = append(payloads, payload)
		}
	}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, fire) }, 400, 240)

	if err := tt.Click("On"); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 1 {
		t.Fatalf("Checkbox 事件 = %v", payloads)
	}
	if m, ok := payloads[0].(map[string]any); !ok || m["value"] != true {
		t.Fatalf("Checkbox payload = %#v", payloads[0])
	}

	if err := tt.Click("B"); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 2 {
		t.Fatalf("Segmented 事件 = %v", payloads)
	}
	if m, ok := payloads[1].(map[string]any); !ok || m["value"] != "B" {
		t.Fatalf("Segmented payload = %#v", payloads[1])
	}
}

func TestReplayTextInputModel(t *testing.T) {
	var values []any
	fire := func(_ string, hid string, payload any) {
		if hid != "in" {
			return
		}
		if m, ok := payload.(map[string]any); ok {
			values = append(values, m["value"])
		} else {
			values = append(values, payload)
		}
	}
	row := &widget.Node{ID: "in", Tag: "TextInput", Model: "state.q",
		Props:  map[string]any{"value": "abc", "placeholder": "type here"},
		Events: map[string]string{"input": "in"}}
	tree := &widget.Node{Tag: "Column", Children: []*widget.Node{row}}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, fire) }, 320, 240)

	// 用户输入 → 后端回报 input {value}
	// Tester 的文本查询不包含输入框内容,这里直接点控件区域。
	tt.ClickAt(20, 20)
	tt.Key(0, ui.KeyEnd)
	tt.Type("d")
	tt.Frame()
	if len(values) == 0 || values[len(values)-1] != "abcd" {
		t.Fatalf("输入事件 = %v", values)
	}

	// JS 侧改值 → 下一帧下发给控件(不回报 input)
	before := len(values)
	row.Props["value"] = "from-js"
	tt.Frame()
	in.mu.Lock()
	got := *in.texts["state.q"]
	in.mu.Unlock()
	if got != "from-js" {
		t.Fatalf("JS 值未同步到控件指针: %q", got)
	}
	if len(values) != before {
		t.Fatalf("JS 侧改值不应回报 input: %v", values[before:])
	}
}

func TestReplayButtonClick(t *testing.T) {
	var fired []string
	fire := func(winID, hid string, payload any) { fired = append(fired, winID+":"+hid) }
	tree := &widget.Node{
		Tag: "Column",
		Children: []*widget.Node{
			{ID: "t", Tag: "Text", Props: map[string]any{"text": "count 0"}},
			{ID: "b", Tag: "Button", Props: map[string]any{"label": "Increment"}, Events: map[string]string{"click": "h1"}},
		},
	}
	in := newWinInput()
	tt := ui.NewTester(func(c *ui.Context) { replay(c, tree, "w", in, fire) }, 320, 240)
	if !tt.HasText("count 0") {
		t.Fatalf("首帧文本缺失: %v", tt.Texts())
	}
	if err := tt.Click("Increment"); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 || fired[0] != "w:h1" {
		t.Fatalf("点击事件 = %v", fired)
	}
	tree.Children[0].Props["text"] = "count 1"
	tt.Frame()
	if !tt.HasText("count 1") {
		t.Fatalf("更新后的文本未渲染: %v", tt.Texts())
	}
}
