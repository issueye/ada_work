package templatec

// HTML 实体解码:静态文本与静态属性在编译期解码,组件作者可以写
// `&lt;` `&amp;` `&#123;` 等;表达式与指令值不做解码(它们是代码)。

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

var namedEntities = map[string]string{
	"amp":    "&",
	"lt":     "<",
	"gt":     ">",
	"quot":   "\"",
	"apos":   "'",
	"nbsp":   "\u00a0",
	"copy":   "©",
	"reg":    "®",
	"hellip": "…",
	"mdash":  "—",
	"ndash":  "–",
}

// decodeEntities 单次解码字符串里的 HTML 实体;未知/非法实体保持原样。
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		semi := strings.IndexByte(s[i:], ';')
		if semi < 0 || semi > 32 { // 不是实体(或实体名前缀过长)
			b.WriteByte('&')
			i++
			continue
		}
		if dec, ok := decodeEntity(s[i+1 : i+semi]); ok {
			b.WriteString(dec)
			i += semi + 1
			continue
		}
		b.WriteByte('&')
		i++
	}
	return b.String()
}

func decodeEntity(ent string) (string, bool) {
	if strings.HasPrefix(ent, "#") {
		num := ent[1:]
		base := 10
		if strings.HasPrefix(num, "x") || strings.HasPrefix(num, "X") {
			base = 16
			num = num[1:]
		}
		if num == "" {
			return "", false
		}
		v, err := strconv.ParseInt(num, base, 32)
		if err != nil || v <= 0 || v > utf8.MaxRune {
			return "", false
		}
		if v >= 0xD800 && v <= 0xDFFF { // UTF-16 代理区不是合法码点
			return "", false
		}
		return string(rune(v)), true
	}
	if v, ok := namedEntities[ent]; ok {
		return v, true
	}
	return "", false
}
