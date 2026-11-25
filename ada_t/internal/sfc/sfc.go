// Package sfc 把 .aui 单文件组件切块(§4.1)。
//
// 一个 .aui 最多四段 + 一个可选 i18n 段:<meta>/<template>/<script>/<style>/<i18n>,
// 顺序无关;template 必需。切块只找顶层块边界,不解析块内容。
package sfc

import (
	"fmt"
	"strings"
)

// Code 是结构诊断码。
type Code string

const CodeStruct Code = "EUISTRUCT"

// Error 是 .aui 结构错误(附录 A:EUISTRUCT)。
type Error struct {
	Line    int
	Col     int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("EUISTRUCT %d:%d: %s", e.Line, e.Col, e.Message)
}

// Block 是切出的一段。
type Block struct {
	Name string // meta template script style i18n
	Lang string // script 的 lang 属性(ts/js),style 的 scoped 标记
	Body string
	Line int // Body 首行在源文件里的行号(1 起)
}

// File 是一个 .aui 的切块结果。
type File struct {
	Meta     string   // <meta> 的 JSON 文本(可为空)
	Template Block    // template 块(必需)
	Script   Block    // script 块(可为空:纯声明式组件)
	Style    Block    // style 块(可为空)
	I18n     string   // <i18n> 的 JSON 文本(可为空)
	blocks   []string // 已出现的块名(重复块检测)
}

// Has 报告某块是否出现过。
func (f *File) Has(name string) bool {
	for _, b := range f.blocks {
		if b == name {
			return true
		}
	}
	return false
}

// Split 切块一个 .aui 文件。
func Split(src string) (*File, error) {
	f := &File{}
	pos := 0
	line := 1
	col := 1
	for pos < len(src) {
		// 找下一个 '<'
		for pos < len(src) && src[pos] != '<' {
			if src[pos] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			pos++
		}
		if pos >= len(src) {
			break
		}
		if strings.HasPrefix(src[pos:], "<!--") {
			// 顶层注释:跳过(块之间的说明文字不应影响切块)
			end := strings.Index(src[pos+4:], "-->")
			if end < 0 {
				return nil, &Error{Line: line, Col: col, Message: "注释没有闭合"}
			}
			stop := pos + 4 + end + 3
			for ; pos < stop; pos++ {
				if src[pos] == '\n' {
					line++
					col = 1
				} else {
					col++
				}
			}
			continue
		}
		// 读块名
		openLine, openCol := line, col
		pos++ // <
		start := pos
		for pos < len(src) && (isIdentChar(src[pos])) {
			pos++
			col++
		}
		name := src[start:pos]
		// 剩余打开标签:吃属性直到 '>'
		attrs, err := eatAttrs(src, &pos, &line, &col)
		if err != nil {
			return nil, err
		}
		closeTag := "</" + name + ">"
		idx := strings.Index(src[pos:], closeTag)
		if idx < 0 {
			return nil, &Error{Line: openLine, Col: openCol, Message: fmt.Sprintf("块 <%s> 没有闭合(%s 未找到)", name, closeTag)}
		}
		bodyStart := pos
		body := src[pos : pos+idx]
		trimmed := strings.TrimSpace(body)
		// body 首行号 = 第一个非空白字符所在行(Block.Body 是 trim 后的内容,
		// 诊断行号以它为基准,因此这里不能再用「整个 body 的新行数」)。
		bodyLine := line
		if trimmed != "" {
			lead := strings.Index(body, trimmed)
			for i := 0; i < lead; i++ {
				if body[i] == '\n' {
					bodyLine++
				}
			}
		}
		pos += idx + len(closeTag)
		// 更新 line/col:跳过 body + 关闭标签
		for i := bodyStart; i < pos; i++ {
			if src[i] == '\n' {
				line++
				col = 1
			} else {
				col++
			}
		}
		switch name {
		case "meta", "template", "script", "style", "i18n":
		default:
			return nil, &Error{Line: openLine, Col: openCol, Message: fmt.Sprintf("未知块 <%s>:只允许 meta/template/script/style/i18n", name)}
		}
		if name == "meta" {
			if f.Meta != "" {
				return nil, &Error{Line: openLine, Col: openCol, Message: "重复的 <meta> 块"}
			}
			f.Meta = trimmed
			f.blocks = append(f.blocks, "meta")
			continue
		}
		if name == "i18n" {
			if f.I18n != "" {
				return nil, &Error{Line: openLine, Col: openCol, Message: "重复的 <i18n> 块"}
			}
			f.I18n = trimmed
			f.blocks = append(f.blocks, "i18n")
			continue
		}
		b := Block{Name: name, Lang: attrValue(attrs, "lang"), Body: trimmed, Line: bodyLine}
		if name == "style" && hasAttr(attrs, "scoped") {
			b.Lang = "scoped"
		}
		if name == "template" {
			if f.Has("template") {
				return nil, &Error{Line: openLine, Col: openCol, Message: "重复的 <template> 块"}
			}
			f.Template = b
			f.blocks = append(f.blocks, "template")
			continue
		}
		if name == "script" {
			if f.Has("script") {
				return nil, &Error{Line: openLine, Col: openCol, Message: "重复的 <script> 块"}
			}
			f.Script = b
			f.blocks = append(f.blocks, "script")
			continue
		}
		if name == "style" {
			if f.Has("style") {
				return nil, &Error{Line: openLine, Col: openCol, Message: "重复的 <style> 块"}
			}
			f.Style = b
			f.blocks = append(f.blocks, "style")
			continue
		}
	}
	if !f.Has("template") {
		return nil, &Error{Line: 1, Col: 1, Message: "缺少必需的 <template> 块"}
	}
	return f, nil
}

func isIdentChar(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// eatAttrs 吃到 '>' 为止,收集 k="v" / k='v' / k=v / k 属性。
func eatAttrs(src string, pos *int, line *int, col *int) (map[string]string, error) {
	attrs := map[string]string{}
	for {
		// 跳空白
		for *pos < len(src) && (src[*pos] == ' ' || src[*pos] == '\t' || src[*pos] == '\r' || src[*pos] == '\n') {
			if src[*pos] == '\n' {
				*line++
				*col = 1
			} else {
				*col++
			}
			*pos++
		}
		if *pos >= len(src) {
			return nil, &Error{Line: *line, Col: *col, Message: "标签没有闭合"}
		}
		if src[*pos] == '>' {
			*pos++
			*col++
			return attrs, nil
		}
		// 属性名
		start := *pos
		for *pos < len(src) && (isIdentChar(src[*pos]) || src[*pos] == ':') {
			*pos++
			*col++
		}
		key := src[start:*pos]
		if key == "" {
			return nil, &Error{Line: *line, Col: *col, Message: "标签内出现非法字符"}
		}
		// = 值
		value := ""
		for *pos < len(src) && (src[*pos] == ' ' || src[*pos] == '\t') {
			*pos++
			*col++
		}
		if *pos < len(src) && src[*pos] == '=' {
			*pos++
			*col++
			for *pos < len(src) && (src[*pos] == ' ' || src[*pos] == '\t') {
				*pos++
				*col++
			}
			if *pos < len(src) && (src[*pos] == '"' || src[*pos] == '\'') {
				q := src[*pos]
				*pos++
				*col++
				vs := *pos
				for *pos < len(src) && src[*pos] != q {
					if src[*pos] == '\n' {
						*line++
						*col = 1
					} else {
						*col++
					}
					*pos++
				}
				if *pos >= len(src) {
					return nil, &Error{Line: *line, Col: *col, Message: "属性值没有闭合"}
				}
				value = src[vs:*pos]
				*pos++
				*col++
			} else {
				vs := *pos
				for *pos < len(src) && src[*pos] != '>' && src[*pos] != ' ' && src[*pos] != '\t' && src[*pos] != '\n' {
					*pos++
					*col++
				}
				value = src[vs:*pos]
			}
		}
		attrs[key] = value
	}
}

func attrValue(attrs map[string]string, key string) string {
	if v, ok := attrs[key]; ok {
		return v
	}
	return ""
}

func hasAttr(attrs map[string]string, key string) bool {
	_, ok := attrs[key]
	return ok
}
