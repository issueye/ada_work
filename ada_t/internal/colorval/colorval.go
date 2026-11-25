// Package colorval 校验颜色字面量,供编译期诊断与原生重放后端共用。
//
// mygo 的 ui.Hex 对非法输入会 panic,因此非法值必须在编译期拒绝、
// 并在重放前再兜底一次(动态 a-bind 无法编译期校验)。
package colorval

// ValidHex 报告 s 是否是受支持的 #hex 颜色:
// #rgb、#rgba、#rrggbb、#rrggbbaa(与 mygo ui.Hex 的接受面一致)。
func ValidHex(s string) bool {
	if len(s) < 4 || s[0] != '#' {
		return false
	}
	h := s[1:]
	switch len(h) {
	case 3, 4, 6, 8:
	default:
		return false
	}
	for i := 0; i < len(h); i++ {
		if !isHexDigit(h[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}
