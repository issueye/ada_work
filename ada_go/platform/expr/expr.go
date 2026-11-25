// Package expr 是受限表达式求值器(数据不允许带执行)。
//
// 语法:路径($.step.field)、字面量(字符串/数字/true/false/null)、比较
// (== != > >= < <=)、逻辑(&& || !)、括号。没有函数调用、赋值与 I/O。
// flow 的 when、bundle 的 when/disabled 都复用它。
package expr

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

type node interface {
	eval(values map[string]any) (any, error)
}

type litNode struct{ value any }

func (n litNode) eval(map[string]any) (any, error) { return n.value, nil }

type pathNode struct{ path string }

func (n pathNode) eval(values map[string]any) (any, error) { return Lookup(n.path, values) }

type notNode struct{ inner node }

func (n notNode) eval(values map[string]any) (any, error) {
	v, err := n.inner.eval(values)
	if err != nil {
		return nil, err
	}
	b, err := truthy(v)
	if err != nil {
		return nil, err
	}
	return !b, nil
}

type andNode struct{ left, right node }

func (n andNode) eval(values map[string]any) (any, error) {
	lv, err := n.left.eval(values)
	if err != nil {
		return nil, err
	}
	lb, err := truthy(lv)
	if err != nil {
		return nil, err
	}
	if !lb {
		return false, nil
	}
	rv, err := n.right.eval(values)
	if err != nil {
		return nil, err
	}
	return truthy(rv)
}

type orNode struct{ left, right node }

func (n orNode) eval(values map[string]any) (any, error) {
	lv, err := n.left.eval(values)
	if err != nil {
		return nil, err
	}
	lb, err := truthy(lv)
	if err != nil {
		return nil, err
	}
	if lb {
		return true, nil
	}
	rv, err := n.right.eval(values)
	if err != nil {
		return nil, err
	}
	return truthy(rv)
}

type cmpNode struct {
	op          string
	left, right node
}

func (n cmpNode) eval(values map[string]any) (any, error) {
	lv, err := n.left.eval(values)
	if err != nil {
		return nil, err
	}
	rv, err := n.right.eval(values)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return Equal(lv, rv), nil
	case "!=":
		return !Equal(lv, rv), nil
	}
	if lf, lok := ToFloat(lv); lok {
		if rf, rok := ToFloat(rv); rok {
			switch n.op {
			case ">":
				return lf > rf, nil
			case ">=":
				return lf >= rf, nil
			case "<":
				return lf < rf, nil
			case "<=":
				return lf <= rf, nil
			}
		}
	}
	ls, lok := lv.(string)
	rs, rok := rv.(string)
	if lok && rok {
		switch n.op {
		case ">":
			return ls > rs, nil
		case ">=":
			return ls >= rs, nil
		case "<":
			return ls < rs, nil
		case "<=":
			return ls <= rs, nil
		}
	}
	return nil, fmt.Errorf("expr: 无法比较 %T 与 %T", lv, rv)
}

func truthy(v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case nil:
		return false, nil
	}
	return false, fmt.Errorf("expr: 条件必须是布尔值,得到 %T(%v)", v, v)
}

// Eval 求值一个表达式,结果必须是布尔(或 nil,视为假)。
func Eval(s string, values map[string]any) (bool, error) {
	root, err := parse(s)
	if err != nil {
		return false, err
	}
	v, err := root.eval(values)
	if err != nil {
		return false, err
	}
	return truthy(v)
}

// Validate 只检查语法(不求值);用于配置解析期提前暴露拼写错误。
func Validate(s string) error {
	_, err := parse(s)
	return err
}

// Equal 判断两个值是否相等(数字按数值比较,其余按深比较)。
func Equal(a, b any) bool {
	af, aok := ToFloat(a)
	bf, bok := ToFloat(b)
	if aok && bok {
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

// ToFloat 把 JSON/Go 数字统一成 float64。
func ToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// Lookup 读取 "$.a.b" 路径;缺失路径返回 nil(与 null 比较时视为相等)。
func Lookup(path string, values map[string]any) (any, error) {
	segs, err := Segments(path)
	if err != nil {
		return nil, err
	}
	var cur any = values
	for _, seg := range segs {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, nil
		}
		v, ok := m[seg]
		if !ok {
			return nil, nil
		}
		cur = v
	}
	return cur, nil
}

// Segments 把 "$.a.b" 拆成 ["a","b"]。
func Segments(path string) ([]string, error) {
	if !strings.HasPrefix(path, "$.") {
		return nil, fmt.Errorf("expr: 路径必须以 $. 开头: %q", path)
	}
	raw := strings.TrimPrefix(path, "$.")
	if raw == "" {
		return nil, fmt.Errorf("expr: 路径缺少字段名: %q", path)
	}
	segs := strings.Split(raw, ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("expr: 路径里有空段: %q", path)
		}
	}
	return segs, nil
}

// ---------- 词法 ----------

type tokKind int

const (
	tokEOF tokKind = iota
	tokPath
	tokString
	tokNumber
	tokBool
	tokNull
	tokOp
	tokLParen
	tokRParen
)

type token struct {
	kind    tokKind
	text    string
	num     float64
	boolVal bool
}

func lex(s string) ([]token, error) {
	var toks []token
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			toks = append(toks, token{kind: tokLParen, text: "("})
			i++
		case c == ')':
			toks = append(toks, token{kind: tokRParen, text: ")"})
			i++
		case c == '$':
			start := i
			i++
			if i >= len(s) || s[i] != '.' {
				return nil, fmt.Errorf("expr: 路径必须以 $. 开头(位置 %d)", start)
			}
			i++
			for i < len(s) {
				if isIdentByte(s[i]) || s[i] == '.' {
					i++
					continue
				}
				break
			}
			text := s[start:i]
			if _, err := Segments(text); err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokPath, text: text})
		case c == '\'' || c == '"':
			quote := c
			i++
			var b strings.Builder
			for i < len(s) && s[i] != quote {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					switch s[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(s[i])
					}
					i++
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			if i >= len(s) {
				return nil, fmt.Errorf("expr: 字符串没有闭合: %q", s)
			}
			i++
			toks = append(toks, token{kind: tokString, text: b.String()})
		case (c >= '0' && c <= '9') || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			start := i
			i++
			for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
				i++
			}
			text := s[start:i]
			n, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("expr: 非法数字 %q", text)
			}
			toks = append(toks, token{kind: tokNumber, text: text, num: n})
		case isIdentStart(c):
			start := i
			for i < len(s) && isIdentByte(s[i]) {
				i++
			}
			word := s[start:i]
			switch word {
			case "true":
				toks = append(toks, token{kind: tokBool, text: word, boolVal: true})
			case "false":
				toks = append(toks, token{kind: tokBool, text: word, boolVal: false})
			case "null":
				toks = append(toks, token{kind: tokNull, text: word})
			default:
				return nil, fmt.Errorf("expr: 不支持标识符 %q(只允许 true/false/null 与 $. 路径)", word)
			}
		default:
			two := ""
			if i+1 < len(s) {
				two = s[i : i+2]
			}
			switch two {
			case "==", "!=", ">=", "<=", "&&", "||":
				toks = append(toks, token{kind: tokOp, text: two})
				i += 2
				continue
			}
			switch c {
			case '>', '<', '!':
				toks = append(toks, token{kind: tokOp, text: string(c)})
				i++
			default:
				return nil, fmt.Errorf("expr: 无法识别的字符 %q(位置 %d)", string(c), i)
			}
		}
	}
	toks = append(toks, token{kind: tokEOF, text: "EOF"})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentByte(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

// ---------- 语法 ----------

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }

func parse(s string) (node, error) {
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	root, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("expr: 表达式有多余内容: %q", p.peek().text)
	}
	return root, nil
}

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && p.peek().text == "||" {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orNode{left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokOp && p.peek().text == "&&" {
		p.next()
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = andNode{left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (node, error) {
	if p.peek().kind == tokOp && p.peek().text == "!" {
		p.next()
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return notNode{inner: inner}, nil
	}
	return p.parseComparison()
}

var cmpOps = map[string]bool{"==": true, "!=": true, ">": true, ">=": true, "<": true, "<=": true}

func (p *parser) parseComparison() (node, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	if t.kind == tokOp && cmpOps[t.text] {
		p.next()
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		return cmpNode{op: t.text, left: left, right: right}, nil
	}
	return left, nil
}

func (p *parser) parseTerm() (node, error) {
	t := p.next()
	switch t.kind {
	case tokLParen:
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, fmt.Errorf("expr: 缺少右括号")
		}
		p.next()
		return inner, nil
	case tokPath:
		return pathNode{path: t.text}, nil
	case tokString:
		return litNode{value: t.text}, nil
	case tokNumber:
		return litNode{value: t.num}, nil
	case tokBool:
		return litNode{value: t.boolVal}, nil
	case tokNull:
		return litNode{value: nil}, nil
	}
	return nil, fmt.Errorf("expr: 表达式里出现意外的 %q", t.text)
}
