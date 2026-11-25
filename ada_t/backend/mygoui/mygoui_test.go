package mygoui

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/issueye/ada_t/widget"
)

// 纯函数测试:属性映射不依赖 ui.Context,是重放正确性的第一道门。
// (grow 布尔失效那个 bug 就该在这里被拦住。)

func TestPropFloat(t *testing.T) {
	props := map[string]any{
		"n":    2.5,
		"s":    "12",
		"bad":  "abc",
		"on":   true,
		"off":  false,
		"miss": nil,
	}
	cases := []struct {
		key  string
		want float64
		ok   bool
	}{
		{"n", 2.5, true},
		{"s", 12, true},
		{"bad", 0, false},
		{"on", 1, true}, // 无值属性(grow)coerce 成 true → 1 倍
		{"off", 0, false},
		{"miss", 0, false},
		{"absent", 0, false},
	}
	for _, c := range cases {
		got, ok := propFloat(props, c.key)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("propFloat(%q) = (%v, %v), want (%v, %v)", c.key, got, ok, c.want, c.ok)
		}
	}
}

func TestPropBool(t *testing.T) {
	props := map[string]any{"t": true, "f": false, "s": "true", "n": 1.0}
	if !propBool(props, "t") || propBool(props, "f") {
		t.Fatal("布尔直读失败")
	}
	if !propBool(props, "s") {
		t.Fatal("字符串 \"true\" 应为真")
	}
	if propBool(props, "n") || propBool(map[string]any{}, "absent") {
		t.Fatal("非布尔缺省应为假")
	}
}

func TestParseAlign(t *testing.T) {
	cases := map[string]ui.Align{
		"start": ui.Start, "center": ui.Center, "end": ui.End,
		"stretch": ui.Stretch, "space-between": ui.SpaceBetween,
		"space-around": ui.SpaceAround, "space-evenly": ui.SpaceEvenly,
		"baseline": ui.Baseline, "": ui.Start, "junk": ui.Start,
	}
	for in, want := range cases {
		if got := parseAlign(in); got != want {
			t.Errorf("parseAlign(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseColor(t *testing.T) {
	white := parseColor("#ffffff")
	if white.R != 255 || white.G != 255 || white.B != 255 {
		t.Errorf("#ffffff = %v", white)
	}
	// 三位缩写
	short := parseColor("#2f6fed")
	if short.R == 0 && short.G == 0 && short.B == 0 {
		t.Error("#2f6fed 解析为全黑")
	}
	if parseColor("red").R == 0 {
		t.Error("命名色 red 失效")
	}
	if parseColor("unknown-color") == (ui.Color{}) {
		t.Error("未知命名色应有回退值")
	}
}

func TestModelKeyOr(t *testing.T) {
	withModel := &widget.Node{Model: "state.query", ID: "n1"}
	if got := modelKeyOr(withModel, "text:"); got != "state.query" {
		t.Fatalf("有 model 应直用: %q", got)
	}
	noModel := &widget.Node{ID: "n9"}
	if got := modelKeyOr(noModel, "text:"); got != "text:n9" {
		t.Fatalf("无 model 应回退节点键: %q", got)
	}
}

func TestStrNumProp(t *testing.T) {
	props := map[string]any{"s": 3.5, "t": "x"}
	if strProp(props, "s") != "3.5" || strProp(props, "t") != "x" || strProp(props, "absent") != "" {
		t.Fatal("strProp 失败")
	}
	if numProp(props, "s") != 3.5 || numProp(props, "absent") != 0 {
		t.Fatal("numProp 失败")
	}
}
