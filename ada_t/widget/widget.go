// Package widget 定义 JS 渲染器与 Go 后端之间的 widget 树契约。
//
// surface.js(moejs 里的 Plan 解释器)每次渲染产出这棵树(JSON),
// Go 侧 bridge 反序列化后交给 Backend 重放为原生控件。
// 树是纯数据:标签、属性、事件(-handler id)、模型绑定与子节点;
// handler id 指向 JS 侧注册表里的动作,事件由后端经 bridge.Fire 送回。
package widget

import (
	"fmt"
	"sort"
	"strings"
)

// PlanVersion 是 RenderPlan 与 widget 树的版本号;不匹配即拒绝。
const PlanVersion = 1

// Node 是 widget 树的一个节点。
//
// Props 的值只能是 JSON 标量(string/float64/bool)或它们的数组;
// Model 是 a-model 的目标路径(如 "state.query"),作为跨帧稳定的绑定键:
// 输入类控件用它保存 *string/*bool/*float64,并检测用户编辑。
// ScrollIntoView(siv)由渲染器标记在新挂载的子组件实例上,
// 后端重放该节点时调 ScrollIntoView 让其滚入视野(聊天新消息等)。
type Node struct {
	ID             string            `json:"id"`
	Tag            string            `json:"tag"`
	Props          map[string]any    `json:"props,omitempty"`
	Events         map[string]string `json:"events,omitempty"`
	Model          string            `json:"model,omitempty"`
	ScrollIntoView bool              `json:"siv,omitempty"`
	Children       []*Node           `json:"children,omitempty"`
}

// Prop 返回属性值(缺失时返回零值与 false)。
func (n *Node) Prop(name string) (any, bool) {
	v, ok := n.Props[name]
	return v, ok
}

// Text 返回节点的文本内容(text 或 label 属性)。
func (n *Node) Text() string {
	if v, ok := n.Props["text"]; ok {
		return fmt.Sprint(v)
	}
	if v, ok := n.Props["label"]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

// Walk 以先序遍历树,fn 返回 false 时跳过该子树。
func (n *Node) Walk(fn func(n *Node) bool) {
	if !fn(n) {
		return
	}
	for _, c := range n.Children {
		c.Walk(fn)
	}
}

// Find 按 ID 查找节点(先序,首个命中)。
func (n *Node) Find(id string) *Node {
	var hit *Node
	n.Walk(func(m *Node) bool {
		if hit != nil {
			return false
		}
		if m.ID == id {
			hit = m
			return false
		}
		return true
	})
	return hit
}

// Dump 输出树的调试文本(缩进两格一层)。
func (n *Node) Dump() string {
	var b strings.Builder
	dump(&b, n, 0)
	return b.String()
}

func dump(b *strings.Builder, n *Node, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s%s#%s", indent, n.Tag, n.ID)
	if len(n.Props) > 0 {
		keys := make([]string, 0, len(n.Props))
		for k := range n.Props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(b, " {")
		for i, k := range keys {
			if i > 0 {
				fmt.Fprintf(b, ", ")
			}
			fmt.Fprintf(b, "%s=%v", k, n.Props[k])
		}
		fmt.Fprintf(b, "}")
	}
	if n.Model != "" {
		fmt.Fprintf(b, " model=%s", n.Model)
	}
	if len(n.Events) > 0 {
		evs := make([]string, 0, len(n.Events))
		for e := range n.Events {
			evs = append(evs, e)
		}
		sort.Strings(evs)
		fmt.Fprintf(b, " events=%v", evs)
	}
	b.WriteByte('\n')
	for _, c := range n.Children {
		dump(b, c, depth+1)
	}
}

// FlatRow 是 Flatten 输出的一行:树深度与该节点的单行描述。
// JSON 字段名小写:跨边界契约(Inspector 组件读 r.depth / r.label)。
type FlatRow struct {
	Depth int    `json:"depth"`
	Label string `json:"label"`
}

// lineOf 生成节点的单行描述(与 Dump 的行格式一致)。
func lineOf(n *Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s#%s", n.Tag, n.ID)
	if len(n.Props) > 0 {
		keys := make([]string, 0, len(n.Props))
		for k := range n.Props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, " {")
		for i, k := range keys {
			if i > 0 {
				fmt.Fprintf(&b, ", ")
			}
			fmt.Fprintf(&b, "%s=%v", k, n.Props[k])
		}
		fmt.Fprintf(&b, "}")
	}
	if n.Model != "" {
		fmt.Fprintf(&b, " model=%s", n.Model)
	}
	if len(n.Events) > 0 {
		evs := make([]string, 0, len(n.Events))
		for e := range n.Events {
			evs = append(evs, e)
		}
		sort.Strings(evs)
		fmt.Fprintf(&b, " events=%v", evs)
	}
	return b.String()
}

// Flatten 把 widget 树拍平成缩进行列表(Inspector 等调试 UI 用)。
func Flatten(n *Node) []FlatRow {
	var out []FlatRow
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		out = append(out, FlatRow{Depth: depth, Label: lineOf(n)})
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return out
}
