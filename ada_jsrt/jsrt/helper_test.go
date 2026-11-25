package jsrt_test

import "testing"

// asInt 归一化 JS 数字:moejs 的 ToGo 对整数值返回 int64,对小数返回 float64。
func asInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	t.Fatalf("期望数字,实际 %T(%v)", v, v)
	return 0
}
