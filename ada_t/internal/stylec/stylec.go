// Package stylec 把 .aui 的 <style scoped> 编译为「class → widget 属性令牌」。
//
// native GUI 没有 CSS 引擎:样式编译期变成控件属性(设计令牌的 native 映射),
// 渲染时由 surface.js 按 class 合并进 widget props(静态属性优先)。
// 规则面刻意最小(§4.1):只允许 .class 规则、白名单属性;
// @import/@font-face/全局选择器/远程 url() 一律 EUISTYLE 拒绝。
package stylec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/issueye/ada_t/internal/colorval"
)

// Code 是样式诊断码。
type Code string

const CodeStyle Code = "EUISTYLE"

// Error 是样式诊断。
type Error struct {
	Line    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("EUISTYLE %d: %s", e.Line, e.Message)
}

// 允许的属性 → widget 属性名(渲染端消费)。
var propWhitelist = map[string]string{
	"font-size":  "font-size",
	"bold":       "bold",
	"color":      "color",
	"background": "background",
	"gap":        "gap",
	"padding":    "padding",
	"width":      "width",
	"height":     "height",
	"radius":     "radius",
	"grow":       "grow",
}

// Compile 解析样式文本,返回 class → (prop → 值) 令牌表。
// scoped 标记是约定的:所有规则天然只作用于本组件(令牌按组件存储)。
func Compile(src string) (map[string]map[string]any, error) {
	src = stripComments(src) // 注释替换为空白,保留换行(诊断行号不变)
	tokens := map[string]map[string]any{}
	line := 1
	pos := 0
	for pos < len(src) {
		// 跳过空白,记录行号
		for pos < len(src) && isSpace(src[pos]) {
			if src[pos] == '\n' {
				line++
			}
			pos++
		}
		if pos >= len(src) {
			break
		}
		startLine := line
		// 选择器:到 '{'
		selStart := pos
		for pos < len(src) && src[pos] != '{' && src[pos] != ';' {
			if src[pos] == '\n' {
				line++
			}
			pos++
		}
		if pos >= len(src) || src[pos] == ';' {
			return nil, &Error{Line: startLine, Message: "样式规则缺少 '{'"}
		}
		sel := strings.TrimSpace(src[selStart:pos])
		pos++ // {
		// 检查选择器形态
		if sel == "" {
			return nil, &Error{Line: startLine, Message: "空选择器"}
		}
		if strings.HasPrefix(sel, "@") {
			return nil, &Error{Line: startLine, Message: fmt.Sprintf("@规则 %q 被拒绝:native 令牌不支持 @import/@media/@keyframes", sel)}
		}
		if !strings.HasPrefix(sel, ".") || strings.ContainsAny(sel, "+>~*[") || strings.Contains(sel, ",") {
			return nil, &Error{Line: startLine, Message: fmt.Sprintf("选择器 %q 被拒绝:只允许单个 .class(样式天然 scoped)", sel)}
		}
		class := sel[1:]
		if !validClass(class) {
			return nil, &Error{Line: startLine, Message: fmt.Sprintf("非法类名 %q", class)}
		}
		// 声明块:到 '}'
		rules := map[string]any{}
		for {
			for pos < len(src) && isSpace(src[pos]) {
				if src[pos] == '\n' {
					line++
				}
				pos++
			}
			if pos >= len(src) {
				return nil, &Error{Line: startLine, Message: "规则块没有闭合"}
			}
			if src[pos] == '}' {
				pos++
				break
			}
			// 属性: 到 ':'(pLine 是属性名所在行,供诊断定位)
			pLine := line
			pStart := pos
			for pos < len(src) && src[pos] != ':' && src[pos] != '}' && src[pos] != ';' {
				if src[pos] == '\n' {
					line++
				}
				pos++
			}
			if pos >= len(src) || src[pos] == '}' || src[pos] == ';' {
				return nil, &Error{Line: pLine, Message: "声明缺少 ':'"}
			}
			prop := strings.TrimSpace(src[pStart:pos])
			pos++ // :
			vStart := pos
			for pos < len(src) && src[pos] != ';' && src[pos] != '}' {
				if src[pos] == '\n' {
					line++
				}
				pos++
			}
			value := strings.TrimSpace(src[vStart:pos])
			if pos < len(src) && src[pos] == ';' {
				pos++
			}
			if prop == "" {
				return nil, &Error{Line: pLine, Message: "空属性名"}
			}
			target, ok := propWhitelist[prop]
			if !ok {
				return nil, &Error{Line: pLine, Message: fmt.Sprintf("属性 %q 不在白名单:%v", prop, whitelistNames())}
			}
			if strings.Contains(value, "url(") {
				return nil, &Error{Line: pLine, Message: "url() 被拒绝(native 令牌不支持)"}
			}
			if (target == "color" || target == "background") && strings.HasPrefix(value, "#") && !colorval.ValidHex(value) {
				return nil, &Error{Line: pLine, Message: fmt.Sprintf("%q 不是合法颜色(支持 #rgb/#rgba/#rrggbb/#rrggbbaa)", value)}
			}
			rules[target] = coerceValue(value)
		}
		if len(rules) == 0 {
			return nil, &Error{Line: startLine, Message: fmt.Sprintf(".%s 规则块为空", class)}
		}
		if _, exists := tokens[class]; exists {
			return nil, &Error{Line: startLine, Message: fmt.Sprintf("重复的 .%s 规则", class)}
		}
		tokens[class] = rules
	}
	return tokens, nil
}

func whitelistNames() []string {
	names := make([]string, 0, len(propWhitelist))
	for k := range propWhitelist {
		names = append(names, k)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

func validClass(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && (r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			continue
		}
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// coerceValue 把令牌值转成 number/bool/string。
func coerceValue(v string) any {
	switch v {
	case "true":
		return true
	case "false":
		return false
	}
	if isNumeric(v) {
		// ParseFloat 严格校验:1.2.3 这类输入保持字符串,不再被 Sscanf 静默截断。
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return v
}

// stripComments 把 /* ... */ 替换为等长空白(保留换行),让注释不参与
// 选择器/声明解析,同时不改变诊断行号。
func stripComments(s string) string {
	if !strings.Contains(s, "/*") {
		return s
	}
	b := []byte(s)
	for i := 0; i < len(b); {
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '*' {
			j := i + 2
			for j+1 < len(b) && !(b[j] == '*' && b[j+1] == '/') {
				j++
			}
			if j+1 < len(b) {
				j += 2 // 吃掉 */
			}
			for k := i; k < j && k < len(b); k++ {
				if b[k] != '\n' {
					b[k] = ' '
				}
			}
			i = j
			continue
		}
		i++
	}
	return string(b)
}

func isNumeric(v string) bool {
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
