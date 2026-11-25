// Package templatec 把 .aui 的 <template> 编译为 RenderPlan(JSON AST)。
//
// 受控 HTML 子集 + Ada 指令(§4.2):a-if/a-else-if/a-else、a-for(+a-key)、
// a-on:event、a-model、a-bind:name、a-class:name、a-prop:name。
// 标签词表是 native 控件面(Column/Row/Text/Button/...),大写标签解析为
// 包内组件。裸文本自动包一层 Text。产物是 planVersion=1 的 JSON,
// 由 surface.js(moejs)解释渲染。
package templatec

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/issueye/ada_t/internal/colorval"
	"github.com/issueye/ada_t/internal/expr"
)

// Code 是模板诊断码。
type Code string

const (
	CodeTemplate Code = "EUITEMPLATE"
	CodeStruct   Code = "EUISTRUCT"
	CodeSecurity Code = "EUISECURITY"
	CodeExpr     Code = "EUIEXPR"
)

// Error 是模板诊断(带文件行列)。
type Error struct {
	Code    Code
	Line    int
	Col     int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %d:%d: %s", e.Code, e.Line, e.Col, e.Message)
}

// Diagnostic 是非致命告警(缺 a-key 等)。
type Diagnostic struct {
	Code    Code
	Line    int
	Message string
}

// EventDecl 是节点上的事件绑定。
type EventDecl struct {
	Ev     string     `json:"ev"`
	Action *expr.Node `json:"action"`
	Mods   []string   `json:"mods,omitempty"`
}

// ModelDecl 是 a-model 的编译产物:双向绑定目标 + 值种类。
type ModelDecl struct {
	Target *expr.Node `json:"target"`
	Kind   string     `json:"kind"` // text | number | bool
}

// TextPart 是文本节点的组成:静态 value 或动态表达式。
type TextPart struct {
	V string     `json:"v,omitempty"`
	E *expr.Node `json:"e,omitempty"`
}

// Branch 是 if 节点的一个分支(cond 为 nil 表示 else)。
type Branch struct {
	Cond     *expr.Node `json:"cond,omitempty"`
	Children []*Node    `json:"children"`
}

// Node 是 Plan 节点。k 决定取用哪些字段:
//
//	el:   tag/class/attrs/binds/on/model/children
//	text: parts(k=text 的中间节点,编译完成后只出现在 el.Text 的 parts 里)
//	if:   branches
//	for:  src/item/index/key/children
//	comp: name/attrs/props/on
type Node struct {
	K        string                `json:"k"`
	ID       string                `json:"id"`
	Tag      string                `json:"tag,omitempty"`
	Class    []string              `json:"class,omitempty"`
	Attrs    map[string]any        `json:"attrs,omitempty"`
	Binds    map[string]*expr.Node `json:"binds,omitempty"`
	On       []EventDecl           `json:"on,omitempty"`
	Model    *ModelDecl            `json:"model,omitempty"`
	Parts    []TextPart            `json:"parts,omitempty"`
	Branches []Branch              `json:"branches,omitempty"`
	Src      *expr.Node            `json:"src,omitempty"`
	Item     string                `json:"item,omitempty"`
	Index    string                `json:"index,omitempty"`
	Key      *expr.Node            `json:"key,omitempty"`
	Name     string                `json:"name,omitempty"`
	Props    map[string]*expr.Node `json:"props,omitempty"`
	Children []*Node               `json:"children,omitempty"`

	// 内部:分支归一化中间态,不序列化
	ifCond   *expr.Node `json:"-"`
	elseIf   *expr.Node `json:"-"`
	isElse   bool       `json:"-"`
	branchLn int        `json:"-"`
}

// Plan 是一个组件的渲染计划。
type Plan struct {
	PlanVersion int    `json:"planVersion"`
	Component   string `json:"component"`
	Root        *Node  `json:"root"`
}

// Options 是模板编译参数;Component 为空时用 "Component"。
type Options struct {
	Component string
}

// Compile 解析并编译模板文本,返回 Plan 与告警。
func Compile(src string, opts Options) (*Plan, []Diagnostic, error) {
	c := &compiler{opts: opts}
	root, err := c.build(src)
	if err != nil {
		return nil, c.warns, err
	}
	component := opts.Component
	if component == "" {
		component = "Component"
	}
	plan := &Plan{PlanVersion: 1, Component: component, Root: root}
	return plan, c.warns, nil
}

// nativeTag: 控件词表与其合法属性(§4.2 白名单的 native 版)。
var nativeTags = map[string]map[string]bool{
	"Column":        {"fill": true, "fill-x": true, "fill-y": true, "center": true, "gap": true, "gap-x": true, "gap-y": true, "padding": true, "width": true, "height": true, "grow": true, "align": true, "justify": true, "background": true, "radius": true},
	"Row":           {"fill": true, "fill-x": true, "fill-y": true, "center": true, "gap": true, "gap-x": true, "gap-y": true, "padding": true, "width": true, "height": true, "grow": true, "align": true, "justify": true, "background": true, "radius": true},
	"Text":          {"font-size": true, "bold": true, "color": true, "width": true, "height": true, "grow": true, "align": true, "padding": true},
	"Button":        {"width": true, "height": true, "grow": true},
	"PrimaryButton": {"width": true, "height": true, "grow": true},
	"TextInput":     {"placeholder": true, "width": true, "height": true, "grow": true},
	"NumberInput":   {"min": true, "max": true, "step": true, "width": true, "grow": true},
	"Switch":        {},
	"Checkbox":      {"label": true},
	"Progress":      {"value": true, "width": true, "grow": true},
	"Scroll":        {"fill": true, "fill-x": true, "fill-y": true, "grow": true, "width": true, "height": true, "background": true, "radius": true},
	"AlertDialog":   {"title": true, "message": true, "buttons": true},
	"Slider":        {"min": true, "max": true, "width": true, "height": true, "grow": true},
	"Stepper":       {"min": true, "max": true, "step": true, "width": true, "height": true, "grow": true},
	"Segmented":     {"options": true, "width": true, "height": true, "grow": true, "fill-x": true},
	"Rating":        {"max": true, "width": true, "height": true, "grow": true},
	"Tabs":          {"options": true, "fill-x": true, "width": true, "height": true, "grow": true},
	"Split":         {"size": true, "vertical": true, "fill": true, "fill-x": true, "fill-y": true, "grow": true},
	"Table":         {"rows": true, "columns": true, "fields": true, "follow-end": true, "fill": true, "fill-x": true, "fill-y": true, "grow": true, "height": true},
	"Spacer":        {},
	"Divider":       {},
}

// containers 是可以有子节点的标签;其余标签的子节点只能是文本(标签文本)。
var containers = map[string]bool{"Column": true, "Row": true, "Scroll": true, "Split": true}

var eventsAllowed = map[string]bool{"click": true, "input": true, "change": true, "submit": true, "action": true, "dismiss": true}

// modelTags 是支持 a-model 双向绑定的控件;其余标签上的 a-model 是用户
// 笔误(旧实现会静默生成一个永不触发的 handler),编译期直接拒绝。
var modelTags = map[string]bool{
	"TextInput": true, "NumberInput": true, "Switch": true, "Checkbox": true,
	"Slider": true, "Stepper": true, "Segmented": true, "Tabs": true,
	"Table": true, "Rating": true, "AlertDialog": true, "Split": true,
}

type attr struct {
	key  string
	val  string
	line int
	col  int
}

type token struct {
	kind string // text | start | end
	name string
	// start 标签
	selfClose bool
	attrs     []attr
	// text
	text      string
	textParts []TextPart
	line      int
	col       int
}

type compiler struct {
	opts  Options
	warns []Diagnostic
}

func (c *compiler) warnf(line int, format string, args ...any) {
	c.warns = append(c.warns, Diagnostic{Code: CodeTemplate, Line: line, Message: fmt.Sprintf(format, args...)})
}

// —— 入口:tokenize → 建树 → 分支归一化 → 编号 ——

func (c *compiler) build(src string) (*Node, error) {
	toks, err := c.tokenize(src)
	if err != nil {
		return nil, err
	}
	root, err := c.buildTree(toks)
	if err != nil {
		return nil, err
	}
	// 根元素不允许分支/循环指令(会破坏单根约束)
	if root.ifCond != nil || root.elseIf != nil || root.isElse || root.Src != nil {
		return nil, &Error{Code: CodeStruct, Line: 1, Col: 1,
			Message: "根元素不能带 a-if/a-else/a-for:包一层 Column 再写"}
	}
	kids, err := c.normalize(root.Children)
	if err != nil {
		return nil, err
	}
	root.Children = kids
	// <Split> 必须恰好两个子节点(first/second 面板)
	if err := checkSplitPanes(root); err != nil {
		return nil, err
	}
	c.assignIDs(root)
	return root, nil
}

// checkSplitPanes 递归校验 Split 子节点数。
func checkSplitPanes(n *Node) error {
	if n.K == "el" && n.Tag == "Split" && len(n.Children) != 2 {
		return &Error{Code: CodeTemplate, Message: "<Split> 必须恰好两个子节点(first/second 面板)"}
	}
	for _, ch := range n.Children {
		if err := checkSplitPanes(ch); err != nil {
			return err
		}
	}
	for _, br := range n.Branches {
		for _, ch := range br.Children {
			if err := checkSplitPanes(ch); err != nil {
				return err
			}
		}
	}
	return nil
}

// assignIDs 给所有节点分配稳定 id(n1, n2, ...)。
func (c *compiler) assignIDs(root *Node) {
	id := 0
	var walk func(list []*Node)
	walk = func(list []*Node) {
		for _, n := range list {
			id++
			n.ID = fmt.Sprintf("n%d", id)
			walk(n.Children)
			for _, br := range n.Branches {
				walk(br.Children)
			}
		}
	}
	walk([]*Node{root})
}

func lineCol(src string, off int) (int, int) {
	line, col := 1, 1
	for i := 0; i < off && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

// —— tokenize ——

func (c *compiler) tokenize(src string) ([]token, error) {
	var toks []token
	pos := 0
	for pos < len(src) {
		if strings.HasPrefix(src[pos:], "<!--") {
			end := strings.Index(src[pos+4:], "-->")
			if end < 0 {
				l, col := lineCol(src, pos)
				return nil, &Error{Code: CodeTemplate, Line: l, Col: col, Message: "注释没有闭合"}
			}
			pos += 4 + end + 3
			continue
		}
		if src[pos] == '<' {
			if strings.HasPrefix(src[pos:], "</") {
				line, col := lineCol(src, pos)
				pos += 2
				start := pos
				for pos < len(src) && src[pos] != '>' {
					pos++
				}
				if pos >= len(src) {
					return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "关闭标签没有闭合"}
				}
				name := strings.TrimSpace(src[start:pos])
				pos++
				toks = append(toks, token{kind: "end", name: name, line: line, col: col})
				continue
			}
			line, col := lineCol(src, pos)
			pos++ // <
			tok := token{kind: "start", line: line, col: col}
			nstart := pos
			for pos < len(src) && !isSpace(src[pos]) && src[pos] != '>' && src[pos] != '/' {
				pos++
			}
			if pos >= len(src) {
				return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "标签没有闭合"}
			}
			tok.name = src[nstart:pos]
			for {
				for pos < len(src) && isSpace(src[pos]) {
					pos++
				}
				if pos >= len(src) {
					return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "标签没有闭合"}
				}
				if src[pos] == '/' {
					pos++
					if pos < len(src) && src[pos] == '>' {
						pos++
						tok.selfClose = true
						break
					}
					return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "自闭合标签缺少 '>'"}
				}
				if src[pos] == '>' {
					pos++
					break
				}
				ks := pos
				for pos < len(src) && !isSpace(src[pos]) && src[pos] != '=' && src[pos] != '>' && src[pos] != '/' {
					pos++
				}
				key := src[ks:pos]
				kl, kc := lineCol(src, ks)
				if key == "" {
					return nil, &Error{Code: CodeTemplate, Line: kl, Col: kc, Message: "标签内出现非法字符"}
				}
				a := attr{key: key, line: kl, col: kc}
				p2 := pos
				for p2 < len(src) && isSpace(src[p2]) {
					p2++
				}
				if p2 < len(src) && src[p2] == '=' {
					p2++
					for p2 < len(src) && isSpace(src[p2]) {
						p2++
					}
					if p2 < len(src) && (src[p2] == '"' || src[p2] == '\'') {
						q := src[p2]
						p2++
						vs := p2
						for p2 < len(src) && src[p2] != q {
							p2++
						}
						if p2 >= len(src) {
							return nil, &Error{Code: CodeTemplate, Line: kl, Col: kc, Message: "属性值没有闭合"}
						}
						a.val = src[vs:p2]
						pos = p2 + 1
					} else {
						vs := p2
						for p2 < len(src) && !isSpace(src[p2]) && src[p2] != '>' {
							p2++
						}
						a.val = src[vs:p2]
						pos = p2
					}
				}
				tok.attrs = append(tok.attrs, a)
			}
			toks = append(toks, tok)
			continue
		}
		// 文本(到下一个 '<' 或结尾)
		start := pos
		for pos < len(src) && src[pos] != '<' {
			pos++
		}
		line, col := lineCol(src, start)
		raw := src[start:pos]
		parts, err := c.splitMustache(raw, line, col)
		if err != nil {
			return nil, err
		}
		if len(parts) > 0 {
			toks = append(toks, token{kind: "text", text: raw, textParts: parts, line: line, col: col})
		}
	}
	return toks, nil
}

// splitMustache 把文本切成静态与 {{ expr }} 混合段。
// 整段先去首尾空白(模板缩进);段内空白保留(插值之间的 ": " 有意义),
// 纯换行缩进段丢弃,纯空格段保留。
func (c *compiler) splitMustache(raw string, line, col int) ([]TextPart, error) {
	raw = strings.TrimSpace(raw)
	var parts []TextPart
	pos := 0
	for pos < len(raw) {
		open := strings.Index(raw[pos:], "{{")
		if open < 0 {
			parts = appendText(parts, raw[pos:])
			break
		}
		close := strings.Index(raw[pos+open:], "}}")
		if close < 0 {
			return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "{{ 表达式没有闭合"}
		}
		parts = appendText(parts, raw[pos:pos+open])
		esrc := strings.TrimSpace(raw[pos+open+2 : pos+open+close])
		if esrc == "" {
			return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "空的 {{ }} 表达式"}
		}
		ast, err := expr.Parse(esrc)
		if err != nil {
			if e, ok := err.(*expr.Error); ok {
				return nil, &Error{Code: CodeTemplate, Line: line, Col: col, Message: "插值表达式错误: " + e.Message}
			}
			return nil, err
		}
		parts = append(parts, TextPart{E: ast})
		pos += open + close + 2
	}
	return parts, nil
}

func appendText(parts []TextPart, s string) []TextPart {
	if s == "" {
		return parts
	}
	// 纯空白:含换行/制表的缩进段丢弃;纯空格(词间)保留
	if strings.TrimSpace(s) == "" {
		if strings.ContainsAny(s, "\n\r\t") {
			return parts
		}
	}
	return append(parts, TextPart{V: decodeEntities(s)})
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// —— 建树 ——

// buildTree 把 token 流变成原始节点树(if/for 中间态挂在节点内部字段上)。
func (c *compiler) buildTree(toks []token) (*Node, error) {
	var stack []*Node // 容器
	var root *Node
	var rootChildren []*Node

	appendNode := func(n *Node) error {
		if len(stack) == 0 {
			if root != nil {
				return &Error{Code: CodeStruct, Line: 1, Col: 1, Message: "根节点必须是单个元素(出现了多个顶层节点)"}
			}
			root = n
			return nil
		}
		parent := stack[len(stack)-1]
		if parent.K != "el" {
			return &Error{Code: CodeTemplate, Message: fmt.Sprintf("<%s> 不能有子节点", parent.Tag)}
		}
		if err := c.validateChild(parent, n); err != nil {
			return err
		}
		// <Text> 的裸文本直接并入自身 parts(不再包一层,渲染端直出 text)
		if parent.Tag == "Text" && len(n.Parts) > 0 {
			parent.Parts = append(parent.Parts, n.Parts...)
			return nil
		}
		parent.Children = append(parent.Children, n)
		return nil
	}

	for _, tok := range toks {
		switch tok.kind {
		case "text":
			// 裸文本自动包一层 Text(容器子节点与标签文本统一走 el.Text)
			n := &Node{K: "el", Tag: "Text", Parts: tok.textParts}
			if err := appendNode(n); err != nil {
				return nil, err
			}
		case "end":
			if len(stack) == 0 {
				return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
					Message: fmt.Sprintf("多余的关闭标签 </%s>", tok.name)}
			}
			top := stack[len(stack)-1]
			if top.Tag != tok.name && top.Name != tok.name {
				return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
					Message: fmt.Sprintf("关闭标签 </%s> 与打开标签 <%s> 不匹配", tok.name, top.Tag+top.Name)}
			}
			stack = stack[:len(stack)-1]
		case "start":
			var n *Node
			var err error
			if isComponentTag(tok.name) {
				n, err = c.buildComp(tok)
			} else if spec, ok := nativeTags[tok.name]; ok {
				n, err = c.buildElement(tok, spec)
			} else {
				return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
					Message: fmt.Sprintf("未知标签 <%s>:可用控件 %v;大写标签为包内组件", tok.name, tagNames())}
			}
			if err != nil {
				return nil, err
			}
			if tok.selfClose {
				if err := appendNode(n); err != nil {
					return nil, err
				}
			} else {
				if err := appendNode(n); err != nil {
					return nil, err
				}
				stack = append(stack, n)
			}
		}
	}
	_ = rootChildren
	if len(stack) > 0 {
		top := stack[len(stack)-1]
		return nil, &Error{Code: CodeTemplate, Line: 1, Col: 1,
			Message: fmt.Sprintf("标签 <%s> 没有闭合", top.Tag+top.Name)}
	}
	if root == nil {
		return nil, &Error{Code: CodeStruct, Line: 1, Col: 1, Message: "模板为空"}
	}
	if root.K != "el" {
		return nil, &Error{Code: CodeStruct, Line: 1, Col: 1, Message: "根节点必须是单个元素"}
	}
	return root, nil
}

func isComponentTag(name string) bool {
	return len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z' && nativeTags[name] == nil
}

func tagNames() []string {
	names := make([]string, 0, len(nativeTags))
	for k := range nativeTags {
		names = append(names, k)
	}
	sortStrings(names)
	return names
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// validateChild 校验父子合法性。
func (c *compiler) validateChild(parent *Node, child *Node) error {
	if child.K == "comp" {
		if parent.Tag == "Column" || parent.Tag == "Row" || parent.Tag == "Scroll" || parent.Tag == "Split" {
			return nil
		}
		return &Error{Code: CodeTemplate, Message: "组件标签只能出现在 Column/Row/Scroll/Split 容器里"}
	}
	switch parent.Tag {
	case "Column", "Row", "Scroll", "Split":
		return nil
	case "Text":
		// 裸文本已被包成 el.Text(Parts 承载内容)
		if len(child.Parts) > 0 {
			return nil
		}
		return &Error{Code: CodeTemplate, Message: "<Text> 里只能是文本"}
	case "Button", "PrimaryButton", "Checkbox":
		if child.Tag == "Text" && len(child.Parts) > 0 {
			return nil
		}
		return &Error{Code: CodeTemplate, Message: fmt.Sprintf("<%s> 里只能是标签文本", parent.Tag)}
	}
	return &Error{Code: CodeTemplate, Message: fmt.Sprintf("<%s> 不能有子节点", parent.Tag)}
}

// buildElement 编译一个 native 标签(属性收集 + 指令解析)。
func (c *compiler) buildElement(tok token, spec map[string]bool) (*Node, error) {
	n := &Node{K: "el", Tag: tok.name}
	for _, a := range tok.attrs {
		switch {
		case a.key == "a-if":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.ifCond = ast
			n.branchLn = a.line
		case a.key == "a-else-if":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.elseIf = ast
			n.branchLn = a.line
		case a.key == "a-else":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			n.isElse = true
			n.branchLn = a.line
		case a.key == "a-for":
			if n.Src != nil {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "重复的 a-for"}
			}
			item, index, src, err := parseFor(a.val)
			if err != nil {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: err.Error()}
			}
			n.Src = src
			n.Item = item
			n.Index = index
		case a.key == "a-key":
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.Key = ast
		case strings.HasPrefix(a.key, "a-on:"):
			rest := a.key[len("a-on:"):]
			ev, mods := splitMods(rest)
			if !eventsAllowed[ev] {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("不支持的事件 %q:%v", ev, eventNames())}
			}
			if ev == "submit" && tok.name != "TextInput" {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: "submit 事件只能用于 <TextInput>(回车提交)"}
			}
			if (ev == "action" || ev == "dismiss") && tok.name != "AlertDialog" {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: "action/dismiss 事件只能用于 <AlertDialog>"}
			}
			if len(mods) > 0 {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("事件修饰符 .%s 尚未支持", strings.Join(mods, "."))}
			}
			for _, prev := range n.On {
				if prev.Ev == ev {
					return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
						Message: fmt.Sprintf("重复的事件 a-on:%s", ev)}
				}
			}
			action, err := expr.ParseAction(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.On = append(n.On, EventDecl{Ev: ev, Action: action, Mods: mods})
		case a.key == "a-model":
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			if !modelTags[tok.name] {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("<%s> 不支持 a-model:只有输入/选择类控件可双向绑定", tok.name)}
			}
			if ast.T != "path" || len(ast.P) == 0 {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "a-model 目标必须是状态路径(如 state.query)"}
			}
			for _, seg := range ast.P {
				if seg.Idx != nil {
					return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
						Message: "a-model 目标只能是点路径(绑定键需跨帧稳定)"}
				}
			}
			if n.Model != nil {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "重复的 a-model"}
			}
			n.Model = &ModelDecl{Target: ast, Kind: modelKind(tok.name)}
		case strings.HasPrefix(a.key, "a-bind:"):
			name := a.key[len("a-bind:"):]
			if !spec[name] {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("<%s> 不支持动态属性 %q:%v", tok.name, name, propNames(spec))}
			}
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			if n.Binds == nil {
				n.Binds = map[string]*expr.Node{}
			}
			if _, dup := n.Binds[name]; dup {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: fmt.Sprintf("重复的 a-bind:%s", name)}
			}
			n.Binds[name] = ast
		case strings.HasPrefix(a.key, "a-class:"):
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			if n.Binds == nil {
				n.Binds = map[string]*expr.Node{}
			}
			n.Binds["class:"+a.key[len("a-class:"):]] = ast
		case strings.HasPrefix(a.key, "a-prop:"):
			return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "a-prop 只能用于组件标签(大写)"}
		case a.key == "a-ref", a.key == "a-html", strings.HasPrefix(a.key, "a-html:"):
			return nil, &Error{Code: CodeSecurity, Line: a.line, Col: a.col, Message: fmt.Sprintf("%s 不被支持(无动态 HTML)", a.key)}
		case strings.HasPrefix(a.key, "on") && len(a.key) > 2:
			return nil, &Error{Code: CodeSecurity, Line: a.line, Col: a.col, Message: "on* 属性被禁止:事件必须走 a-on:"}
		default:
			if !spec[a.key] && a.key != "class" {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("<%s> 不支持属性 %q:%v", tok.name, a.key, propNames(spec))}
			}
			if a.key == "class" {
				n.Class = append(n.Class, strings.Fields(decodeEntities(a.val))...)
				continue
			}
			if n.Attrs == nil {
				n.Attrs = map[string]any{}
			}
			if _, dup := n.Attrs[a.key]; dup {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: fmt.Sprintf("重复的属性 %q", a.key)}
			}
			val := coerce(decodeEntities(a.val))
			if (a.key == "color" || a.key == "background") && !validStaticColor(val) {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("%q 不是合法颜色(支持 #rgb/#rgba/#rrggbb/#rrggbbaa)", a.val)}
			}
			n.Attrs[a.key] = val
		}
	}
	// a-model 会占用控件的输入事件:再写同事件 a-on 会被模型 handler
	// 覆盖(旧实现静默丢弃用户 handler),编译期直接报错。
	if n.Model != nil {
		want := modelEvent(n.Model.Kind)
		for _, on := range n.On {
			if on.Ev == want {
				return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
					Message: fmt.Sprintf("a-model 已占用 %s 事件:请用其他事件或去掉 a-on:%s", want, want)}
			}
		}
	}
	// a-for 与分支指令同标签:语义歧义,直接拒绝
	if n.Src != nil && (n.ifCond != nil || n.elseIf != nil || n.isElse) {
		return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
			Message: "a-for 与 a-if/a-else 不能同时用于一个元素:包一层元素分开写"}
	}
	if n.Src != nil {
		if n.Item == "" {
			n.Item = "item"
		}
		if n.Key == nil {
			c.warnf(tok.line, "a-for 建议提供 a-key(对象项必须提供)")
		}
	} else if n.Key != nil {
		return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
			Message: "a-key 只能与 a-for 一起使用"}
	}
	return n, nil
}

func branchDupErr(a attr) error {
	return &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "一个元素只能有一个分支指令(a-if/a-else-if/a-else)"}
}

// normalize 把兄弟序列里的 a-if/a-else-if/a-else 归并为 if 节点,
// 把带 a-for 的元素包成 for 节点;递归处理容器子节点与分支子树。
// 返回归一化后的兄弟列表。
func (c *compiler) normalize(list []*Node) ([]*Node, error) {
	var out []*Node
	i := 0
	for i < len(list) {
		n := list[i]
		switch {
		case n.ifCond != nil:
			// 开始一条链:向后吃相邻的 a-else-if / a-else
			// (先捕获 cond 再剥,理由同 a-for)
			cond := n.ifCond
			branches := []Branch{{Cond: cond, Children: []*Node{stripBranch(n)}}}
			j := i + 1
			for j < len(list) && (list[j].elseIf != nil || list[j].isElse) {
				m := list[j]
				if m.elseIf != nil {
					c2 := m.elseIf
					branches = append(branches, Branch{Cond: c2, Children: []*Node{stripBranch(m)}})
				} else {
					branches = append(branches, Branch{Cond: nil, Children: []*Node{stripBranch(m)}})
					j++
					break // else 之后不能再有分支
				}
				j++
			}
			// 分支元素自身可能是容器,继续归一化它们的子树
			for bi := range branches {
				for _, el := range branches[bi].Children {
					if err := c.normalizeSub(el); err != nil {
						return nil, err
					}
				}
			}
			ifNode := &Node{K: "if", Branches: branches}
			out = append(out, ifNode)
			i = j
		case n.elseIf != nil || n.isElse:
			return nil, &Error{Code: CodeStruct, Line: n.branchLn, Col: 1,
				Message: "a-else-if/a-else 必须紧跟在 a-if 元素之后"}
		default:
			if err := c.normalizeSub(n); err != nil {
				return nil, err
			}
			// a-for 归一化:包一层 for 节点
			// (先捕获字段再剥:组合字面量里的函数调用可能先于字段读取执行)
			if n.Src != nil {
				srcAST, item, index, key := n.Src, n.Item, n.Index, n.Key
				inner := stripFor(n)
				forNode := &Node{K: "for", Src: srcAST, Item: item, Index: index, Key: key, Children: []*Node{inner}}
				out = append(out, forNode)
			} else {
				out = append(out, n)
			}
			i++
		}
	}
	return out, nil
}

// normalizeSub 归一化一个元素节点的内部(容器子节点或 for 元素的子树)。
func (c *compiler) normalizeSub(n *Node) error {
	if n.K != "el" {
		return nil
	}
	if containers[n.Tag] || n.Src != nil {
		kids, err := c.normalize(n.Children)
		if err != nil {
			return err
		}
		n.Children = kids
	}
	return nil
}

// stripBranch 返回去掉分支中间态的节点(归一化后分支内容就是它自己)。
func stripBranch(n *Node) *Node {
	// 分支指令只影响中间态,元素字段原样保留
	n.ifCond, n.elseIf, n.isElse = nil, nil, false
	return n
}

// stripFor 返回去掉 for 中间态的节点(for 包裹节点持有子列表)。
func stripFor(n *Node) *Node {
	n.Src, n.Item, n.Index, n.Key = nil, "", "", nil
	return n
}

func (c *compiler) exprErr(err error, a attr) error {
	if e, ok := err.(*expr.Error); ok {
		// 表达式错误保留自己的诊断码(EUIEXPR),与模板/安全错误区分。
		return &Error{Code: CodeExpr, Line: a.line, Col: a.col, Message: e.Message}
	}
	return err
}

func splitMods(rest string) (string, []string) {
	segs := strings.Split(rest, ".")
	return segs[0], segs[1:]
}

func eventNames() []string {
	return []string{"click", "input", "change", "submit", "action", "dismiss"}
}

func modelKind(tag string) string {
	switch tag {
	case "NumberInput", "Slider", "Stepper", "Split":
		return "number"
	case "Switch", "Checkbox":
		return "bool"
	case "AlertDialog":
		return "open" // 开关弹层(双向:Escape/按钮关闭会写回)
	case "Segmented", "Tabs":
		return "choice"
	case "Table", "Rating":
		return "index" // 选中下标/星级数(双向,change 事件回传)
	default:
		return "text"
	}
}

// modelEvent 是 a-model 自动注册的事件名(与 surface.js 的 evName 规则一致)。
func modelEvent(kind string) string {
	switch kind {
	case "choice", "bool", "open", "index":
		return "change"
	default:
		return "input"
	}
}

// parseFor 解析 "item, index in expr"。
func parseFor(v string) (item, index string, src *expr.Node, err error) {
	idx := strings.Index(v, " in ")
	if idx < 0 {
		return "", "", nil, fmt.Errorf("a-for 语法应为 \"item, index in list\"")
	}
	head := strings.TrimSpace(v[:idx])
	tail := strings.TrimSpace(v[idx+4:])
	head = strings.TrimSuffix(strings.TrimPrefix(head, "("), ")")
	parts := strings.Split(head, ",")
	item = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		index = strings.TrimSpace(parts[1])
	}
	if item == "" || !isIdentName(item) {
		return "", "", nil, fmt.Errorf("a-for 项名非法: %q", item)
	}
	if index != "" && !isIdentName(index) {
		return "", "", nil, fmt.Errorf("a-for 下标名非法: %q", index)
	}
	src, perr := expr.Parse(tail)
	if perr != nil {
		return "", "", nil, fmt.Errorf("a-for 源表达式错误: %s", perr.Error())
	}
	return item, index, src, nil
}

func isIdentName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && (r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			continue
		}
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// validStaticColor 校验静态颜色属性:#hex 必须是合法写法;命名色与动态值
// 放行(命名色的最终映射在重放后端,动态值由 a-bind 在运行期给)。
func validStaticColor(v any) bool {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "#") {
		return true
	}
	return colorval.ValidHex(s)
}

// coerce 把静态属性字符串转成 number/bool/string(空值 = 布尔开关)。
func coerce(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	case "":
		return true // 无值属性(fill center)即开关
	}
	if isNumericLiteral(v) {
		// ParseFloat 严格校验:1.2.3 这类输入保持字符串,不再被 Sscanf 静默截断。
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return v
}

func isNumericLiteral(v string) bool {
	if v == "" {
		return false
	}
	for i, c := range v {
		if (c >= '0' && c <= '9') || c == '.' || ((c == '-' || c == '+') && i == 0) {
			continue
		}
		return false
	}
	return true
}

func propNames(spec map[string]bool) []string {
	names := make([]string, 0, len(spec))
	for k := range spec {
		names = append(names, k)
	}
	sortStrings(names)
	return names
}

// buildComp 编译组件标签(大写)。
func (c *compiler) buildComp(tok token) (*Node, error) {
	n := &Node{K: "comp", Name: tok.name}
	for _, a := range tok.attrs {
		switch {
		case strings.HasPrefix(a.key, "a-prop:"):
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			if n.Props == nil {
				n.Props = map[string]*expr.Node{}
			}
			name := a.key[len("a-prop:"):]
			if _, dup := n.Props[name]; dup {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: fmt.Sprintf("重复的 a-prop:%s", name)}
			}
			n.Props[name] = ast
		case a.key == "a-if":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.ifCond = ast
			n.branchLn = a.line
		case a.key == "a-else-if":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.elseIf = ast
			n.branchLn = a.line
		case a.key == "a-else":
			if n.ifCond != nil || n.elseIf != nil || n.isElse {
				return nil, branchDupErr(a)
			}
			n.isElse = true
			n.branchLn = a.line
		case a.key == "a-for":
			if n.Src != nil {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "重复的 a-for"}
			}
			item, index, src, err := parseFor(a.val)
			if err != nil {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: err.Error()}
			}
			n.Src = src
			n.Item = item
			n.Index = index
		case a.key == "a-key":
			ast, err := expr.Parse(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.Key = ast
		case strings.HasPrefix(a.key, "a-on:"):
			rest := a.key[len("a-on:"):]
			ev, mods := splitMods(rest)
			if len(mods) > 0 {
				return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
					Message: fmt.Sprintf("事件修饰符 .%s 尚未支持", strings.Join(mods, "."))}
			}
			// 组件事件是组件自己的契约(select/change/…自定义),不受 native 白名单限制;
			// 白名单只管原生控件标签。
			action, err := expr.ParseAction(a.val)
			if err != nil {
				return nil, c.exprErr(err, a)
			}
			n.On = append(n.On, EventDecl{Ev: ev, Action: action, Mods: mods})
		case a.key == "a-model" || strings.HasPrefix(a.key, "a-bind:"):
			return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col, Message: "组件用 a-prop: 传参"}
		case a.key == "class":
			c.warnf(tok.line, "组件标签上的 class 被忽略(样式属于组件自身)")
		case a.key == "a-html" || strings.HasPrefix(a.key, "a-html:"):
			return nil, &Error{Code: CodeSecurity, Line: a.line, Col: a.col, Message: fmt.Sprintf("%s 不被支持(无动态 HTML)", a.key)}
		case a.key == "a-ref":
			return nil, &Error{Code: CodeSecurity, Line: a.line, Col: a.col, Message: "a-ref 不被支持(无 DOM 引用)"}
		case strings.HasPrefix(a.key, "on") && len(a.key) > 2:
			return nil, &Error{Code: CodeSecurity, Line: a.line, Col: a.col,
				Message: "组件上的 on* 属性会被当成普通 prop;事件请用 a-on:,普通属性请改名"}
		case strings.HasPrefix(a.key, "a-"):
			return nil, &Error{Code: CodeTemplate, Line: a.line, Col: a.col,
				Message: fmt.Sprintf("组件不支持指令 %q:动态 props 用 a-prop:,事件用 a-on:", a.key)}
		default:
			if n.Attrs == nil {
				n.Attrs = map[string]any{}
			}
			n.Attrs[a.key] = coerce(decodeEntities(a.val))
		}
	}
	if n.Src == nil && n.Key != nil {
		return nil, &Error{Code: CodeTemplate, Line: tok.line, Col: tok.col,
			Message: "a-key 只能与 a-for 一起使用"}
	}
	return n, nil
}

// Marshal 把 Plan 序列化为紧凑 JSON。
func Marshal(p *Plan) ([]byte, error) {
	return json.Marshal(p)
}
