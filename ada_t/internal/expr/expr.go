// Package expr 编译 .aui 模板里的受限表达式(§4.2)。
//
// 语法:字面量、数组、标识符路径、成员/下标、一元 !/-、比较、逻辑、??、
// 三元、括号与「作用域内函数」调用。刻意拒绝赋值、new、await、this 与
// 任意全局:表达式是纯求值,副作用只允许出现在 a-on 动作里。
// 产物是 JSON 可序列化的 AST,由 surface.js 在 moejs 里解释求值——
// 不拼接字符串、不 eval(§4.1 硬规则)。
package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// Code 是诊断码(附录 A)。
type Code string

const (
	CodeSyntax Code = "EUIEXPR"
)

// Node 是表达式 AST。字段按 t 取用:
//
//	lit   V=字面值(string/float64/bool/nil)
//	path  P=路径段;每段是 string(成员)或 *Node(下标)
//	un    Op(! -) E=操作数
//	bin   Op(+ - * / % == != < > <= >= && || ??) L R
//	tern  E=条件 A=then B=else
//	call  Fn=被调路径 A(必须以 path 起头) Args
//	arr   Args=元素
type Node struct {
	T    string  `json:"t"`
	V    any     `json:"v,omitempty"`
	P    []*Seg  `json:"p,omitempty"`
	Op   string  `json:"op,omitempty"`
	E    *Node   `json:"e,omitempty"`
	L    *Node   `json:"l,omitempty"`
	R    *Node   `json:"r,omitempty"`
	A    *Node   `json:"a,omitempty"`
	B    *Node   `json:"b,omitempty"`
	Fn   *Node   `json:"fn,omitempty"`
	Args []*Node `json:"args,omitempty"`
}

// Seg 是路径的一段:Key 是成员名(K=true),Idx 是下标表达式(K=false)。
type Seg struct {
	Key string `json:"key,omitempty"`
	Idx *Node  `json:"idx,omitempty"`
}

// Pos 是表达式在 .aui 文件里的位置(诊断用)。
type Pos struct {
	Line, Col int
}

// Error 是带位置的诊断。
type Error struct {
	Code    Code
	Line    int
	Col     int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %d:%d: %s", e.Code, e.Line, e.Col, e.Message)
}

type parser struct {
	src  string
	pos  int
	line int
	col  int
}

// Parse 解析一个表达式;src 允许前后空白。
func Parse(src string) (*Node, error) {
	p := &parser{src: src, line: 1, col: 1}
	p.skipWS()
	n, err := p.parseTernary()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos < len(p.src) {
		return nil, p.failf("表达式后有多余内容 %q", p.rest())
	}
	return n, nil
}

// ParseAction 解析 a-on 动作:函数名(路径)或函数调用。
// 返回 AST;调用形如 select(item),裸名形如 inc。
func ParseAction(src string) (*Node, error) {
	n, err := Parse(src)
	if err != nil {
		return nil, err
	}
	switch n.T {
	case "path", "call":
		return n, nil
	}
	return nil, &Error{Code: CodeSyntax, Message: fmt.Sprintf("动作只能是函数或函数调用,得到 %q", src)}
}

func (p *parser) rest() string {
	if len(p.src)-p.pos > 24 {
		return p.src[p.pos:p.pos+24] + "…"
	}
	return p.src[p.pos:]
}

func (p *parser) failf(format string, args ...any) error {
	return &Error{Code: CodeSyntax, Line: p.line, Col: p.col, Message: fmt.Sprintf(format, args...)}
}

func (p *parser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) peekAt(off int) byte {
	if p.pos+off >= len(p.src) {
		return 0
	}
	return p.src[p.pos+off]
}

func (p *parser) advance() byte {
	c := p.src[p.pos]
	p.pos++
	if c == '\n' {
		p.line++
		p.col = 1
	} else {
		p.col++
	}
	return c
}

func (p *parser) skipWS() {
	for p.pos < len(p.src) {
		switch c := p.peek(); c {
		case ' ', '\t', '\r', '\n':
			p.advance()
		default:
			return
		}
	}
}

// accept 吃掉 s(调用前必须已在非空白处);失败返回 false 且不回退已推进的字符。
func (p *parser) accept(s string) bool {
	if strings.HasPrefix(p.src[p.pos:], s) {
		for range len(s) {
			p.advance()
		}
		return true
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '$' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *parser) ident() (string, error) {
	if !isIdentStart(p.peek()) {
		return "", p.failf("需要标识符,得到 %q", p.rest())
	}
	start := p.pos
	for p.pos < len(p.src) && isIdentChar(p.peek()) {
		p.advance()
	}
	return p.src[start:p.pos], nil
}

func (p *parser) parseTernary() (*Node, error) {
	cond, err := p.parseNullish()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.peek() != '?' || p.peekAt(1) == '?' {
		return cond, nil
	}
	p.advance() // ?
	thenN, err := p.parseNullish()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.peek() != ':' {
		return nil, p.failf("三元表达式缺少 ':'")
	}
	p.advance()
	elseN, err := p.parseTernary()
	if err != nil {
		return nil, err
	}
	return &Node{T: "tern", E: cond, A: thenN, B: elseN}, nil
}

func (p *parser) parseNullish() (*Node, error) {
	l, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.peek() == '?' && p.peekAt(1) == '?' {
			p.advance()
			p.advance()
			r, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			l = &Node{T: "bin", Op: "??", L: l, R: r}
			continue
		}
		return l, nil
	}
}

func (p *parser) parseOr() (*Node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.accept("||") {
			r, err := p.parseAnd()
			if err != nil {
				return nil, err
			}
			l = &Node{T: "bin", Op: "||", L: l, R: r}
			continue
		}
		return l, nil
	}
}

func (p *parser) parseAnd() (*Node, error) {
	l, err := p.parseEq()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		if p.accept("&&") {
			r, err := p.parseEq()
			if err != nil {
				return nil, err
			}
			l = &Node{T: "bin", Op: "&&", L: l, R: r}
			continue
		}
		return l, nil
	}
}

func (p *parser) parseEq() (*Node, error) {
	l, err := p.parseRel()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		var op string
		switch {
		case p.accept("==="):
			op = "=="
		case p.accept("!=="):
			op = "!="
		case p.accept("=="):
			op = "=="
		case p.accept("!="):
			op = "!="
		default:
			return l, nil
		}
		r, err := p.parseRel()
		if err != nil {
			return nil, err
		}
		l = &Node{T: "bin", Op: op, L: l, R: r}
	}
}

func (p *parser) parseRel() (*Node, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		var op string
		switch {
		case p.accept("<="):
			op = "<="
		case p.accept(">="):
			op = ">="
		case p.accept("<"):
			op = "<"
		case p.accept(">"):
			op = ">"
		default:
			return l, nil
		}
		r, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		l = &Node{T: "bin", Op: op, L: l, R: r}
	}
}

func (p *parser) parseAdd() (*Node, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		var op string
		switch p.peek() {
		case '+':
			op = "+"
		case '-':
			op = "-"
		default:
			return l, nil
		}
		p.advance()
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = &Node{T: "bin", Op: op, L: l, R: r}
	}
}

func (p *parser) parseMul() (*Node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipWS()
		var op string
		switch p.peek() {
		case '*':
			op = "*"
		case '/':
			op = "/"
		case '%':
			op = "%"
		default:
			return l, nil
		}
		p.advance()
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &Node{T: "bin", Op: op, L: l, R: r}
	}
}

func (p *parser) parseUnary() (*Node, error) {
	p.skipWS()
	switch p.peek() {
	case '!':
		p.advance()
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Node{T: "un", Op: "!", E: e}, nil
	case '-':
		p.advance()
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &Node{T: "un", Op: "-", E: e}, nil
	}
	return p.parsePostfix()
}

// parsePostfix 解析主表达式与其后的 .成员 /[下标]/(调用) 链。
// 调用只允许出现在「到当前位置为止没有调用」的路径上(禁 foo()())。
func (p *parser) parsePostfix() (*Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	called := false
	for {
		p.skipWS()
		switch p.peek() {
		case '.':
			p.advance()
			name, err := p.ident()
			if err != nil {
				return nil, err
			}
			if n.T == "path" {
				n.P = append(n.P, &Seg{Key: name})
			} else {
				// utool:把已解析的 n 变成下标链不支持——成员只属于路径;
				// 这里意味着 (a?b:c).x 这类写法,直接拒绝,保持语法面受控。
				return nil, p.failf("成员访问只能作用于标识符路径")
			}
		case '[':
			p.advance()
			idx, err := p.parseTernary()
			if err != nil {
				return nil, err
			}
			p.skipWS()
			if p.peek() != ']' {
				return nil, p.failf("缺少 ']'")
			}
			p.advance()
			if n.T == "path" {
				n.P = append(n.P, &Seg{Idx: idx})
			} else {
				return nil, p.failf("下标只能作用于标识符路径")
			}
		case '(':
			if called {
				return nil, p.failf("不允许链式调用函数的结果")
			}
			if n.T != "path" {
				return nil, p.failf("只能调用作用域内的函数(路径)")
			}
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			n = &Node{T: "call", Fn: n, Args: args}
			called = true
		default:
			return n, nil
		}
	}
}

func (p *parser) parseArgs() ([]*Node, error) {
	p.advance() // (
	var args []*Node
	p.skipWS()
	if p.peek() == ')' {
		p.advance()
		return args, nil
	}
	for {
		a, err := p.parseTernary()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		p.skipWS()
		if p.accept(",") {
			p.skipWS()
			continue
		}
		if p.peek() == ')' {
			p.advance()
			return args, nil
		}
		return nil, p.failf("参数列表缺少 ',' 或 ')'")
	}
}

func (p *parser) parsePrimary() (*Node, error) {
	p.skipWS()
	switch c := p.peek(); {
	case c == 0:
		return nil, p.failf("表达式意外结束")
	case c == '(':
		p.advance()
		n, err := p.parseTernary()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.peek() != ')' {
			return nil, p.failf("缺少 ')'")
		}
		p.advance()
		return n, nil
	case c == '\'' || c == '"':
		return p.parseString()
	case c >= '0' && c <= '9':
		return p.parseNumber()
	case c == '[':
		return p.parseArray()
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	switch name {
	case "true":
		return &Node{T: "lit", V: true}, nil
	case "false":
		return &Node{T: "lit", V: false}, nil
	case "null":
		return &Node{T: "lit", V: nil}, nil
	}
	switch name {
	case "new", "this", "await", "function", "typeof", "delete":
		return nil, p.failf("表达式禁止 %q", name)
	}
	// 允许的具名引用:作用域绑定、$t、$event、$refs——求值时判定是否在作用域内。
	segs := []*Seg{{Key: name}}
	return &Node{T: "path", P: segs}, nil
}

func (p *parser) parseString() (*Node, error) {
	quote := p.advance()
	var b strings.Builder
	for {
		if p.pos >= len(p.src) {
			return nil, p.failf("字符串没有闭合")
		}
		c := p.advance()
		switch c {
		case quote:
			return &Node{T: "lit", V: b.String()}, nil
		case '\\':
			if p.pos >= len(p.src) {
				return nil, p.failf("转义符后没有字符")
			}
			e := p.advance()
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '\'', '"':
				b.WriteByte(e)
			default:
				return nil, p.failf("不支持的转义 \\%c", e)
			}
		default:
			b.WriteByte(c)
		}
	}
}

func (p *parser) parseNumber() (*Node, error) {
	start := p.pos
	digits := func() int {
		n := 0
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.advance()
			n++
		}
		return n
	}
	digits() // parsePrimary 已保证首字符是数字
	if p.peek() == '.' {
		p.advance()
		digits()
	}
	if p.peek() == 'e' || p.peek() == 'E' {
		p.advance()
		if p.peek() == '+' || p.peek() == '-' {
			p.advance()
		}
		if digits() == 0 {
			return nil, p.failf("非法指数")
		}
	}
	text := p.src[start:p.pos]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, p.failf("非法数字 %q", text)
	}
	return &Node{T: "lit", V: v}, nil
}

func (p *parser) parseArray() (*Node, error) {
	p.advance() // [
	var args []*Node
	p.skipWS()
	if p.peek() == ']' {
		p.advance()
		return &Node{T: "arr", Args: args}, nil
	}
	for {
		a, err := p.parseTernary()
		if err != nil {
			return nil, err
		}
		args = append(args, a)
		p.skipWS()
		if p.accept(",") {
			p.skipWS()
			if p.peek() == ']' { // 尾逗号
				p.advance()
				return &Node{T: "arr", Args: args}, nil
			}
			continue
		}
		if p.peek() == ']' {
			p.advance()
			return &Node{T: "arr", Args: args}, nil
		}
		return nil, p.failf("数组缺少 ',' 或 ']'")
	}
}
