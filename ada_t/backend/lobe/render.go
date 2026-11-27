package lobeui

// render.go:把 ada_t widget 树映射到 lobe-mygo 组件(而非仅套 lobe 主题的 mygo 基础控件)。
//
// 已映射核心子集(其余 tag 回退 mygoui 重放,保留 a-model 双向绑定等完整语义):
//   - 容器 Column / Row:布局属性复用 mygoui.ApplyLayout,递归渲染子节点,容器 click 与 mygoui 同规则;
//   - Text:lobe-mygo display.Text(字号/粗体/颜色/对齐保留);
//   - Button / PrimaryButton:lobe-mygo action.Button(Default/Primary),click 事件回传。
//
// 未映射 tag(TextInput/Switch/Checkbox/Table/Scroll/...):fallback.Render 走 mygoui。

import (
	"github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/backend/mygoui"
	"github.com/issueye/ada_t/widget"
)

// renderLobeNode 把 n 渲染为 lobe-mygo 组件;未映射 tag 回退 mygoui 重放。
func renderLobeNode(c *ui.Context, n *widget.Node, slot string, dispatch backend.DispatchFunc, fallback *mygoui.Renderer) ui.Element {
	if n == nil {
		return ui.Element{}
	}
	switch n.Tag {
	case "Column", "Row":
		var el ui.Element
		if n.Tag == "Column" {
			el = ui.Column(c)
		} else {
			el = ui.Row(c)
		}
		el = mygoui.ApplyLayout(el, n.Props)
		el.Children(func() {
			for _, ch := range n.Children {
				renderLobeNode(c, ch, slot, dispatch, fallback)
			}
		})
		// 与 mygoui 一致:只有显式绑定 click 的容器才注册可点击命中。
		if n.Events["click"] != "" && el.Clicked() {
			fireClickLobe(n, slot, dispatch)
		}
		return applySiv(el, n)

	case "Text":
		tp := display.TextProps{}
		if v, ok := pfloat(n.Props, "font-size"); ok {
			tp.FontSize = float32(v)
		}
		if pbool(n.Props, "bold") {
			tp.Strong = true
		}
		el := display.Text(c, n.Text(), tp)
		if v, ok := n.Props["color"].(string); ok {
			el = el.TextColor(mygoui.ParseColor(v))
		}
		if v, ok := n.Props["align"].(string); ok {
			el = el.TextAlign(mygoui.ParseAlign(v))
		}
		el = mygoui.ApplyLayout(el, n.Props)
		return applySiv(el, n)

	case "Button", "PrimaryButton":
		variant := action.Default
		if n.Tag == "PrimaryButton" {
			variant = action.Primary
		}
		el := action.Button(c, n.Text(), action.ButtonProps{Variant: variant})
		el = mygoui.ApplyLayout(el, n.Props)
		if el.Clicked() {
			fireClickLobe(n, slot, dispatch)
		}
		return applySiv(el, n)

	default:
		// 未映射 tag 回退 mygoui(mygoui 自行递归子节点;保留 a-model/事件/布局完整语义)。
		fallback.Render(c, slot, n)
		return ui.Element{}
	}
}

func applySiv(el ui.Element, n *widget.Node) ui.Element {
	if n.ScrollIntoView {
		return el.ScrollIntoView()
	}
	return el
}

func fireClickLobe(n *widget.Node, winID string, fire backend.DispatchFunc) {
	if hid, ok := n.Events["click"]; ok && hid != "" {
		fire(winID, hid, nil)
	}
}

func pfloat(props map[string]any, key string) (float64, bool) {
	v, ok := props[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func pbool(props map[string]any, key string) bool {
	v, ok := props[key].(bool)
	return ok && v
}
