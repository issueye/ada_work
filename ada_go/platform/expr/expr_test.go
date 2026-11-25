package expr_test

import (
	"testing"

	"github.com/issueye/ada_go_v2/platform/expr"
)

// 1. 求值:路径/比较/逻辑/缺失路径。
func TestEval(t *testing.T) {
	values := map[string]any{"a": map[string]any{"on": true}, "n": float64(3)}
	cases := []struct {
		src  string
		want bool
	}{
		{`$.a.on`, true},
		{`$.n > 2 && $.a.on`, true},
		{`!($.n < 2)`, true},
		{`$.missing == null`, true},
		{`"x" != "y"`, true},
	}
	for _, tc := range cases {
		got, err := expr.Eval(tc.src, values)
		if err != nil {
			t.Fatalf("%q 求值失败: %v", tc.src, err)
		}
		if got != tc.want {
			t.Fatalf("%q = %v,期望 %v", tc.src, got, tc.want)
		}
	}
}

// 2. 语法校验与路径查找。
func TestValidateAndLookup(t *testing.T) {
	if err := expr.Validate(`$.a.b == 1`); err != nil {
		t.Fatalf("合法表达式被拒绝: %v", err)
	}
	for _, bad := range []string{`$.a >`, `bad ident`, `(1`, `$.a === 1`} {
		if err := expr.Validate(bad); err == nil {
			t.Fatalf("非法表达式应被拒绝: %q", bad)
		}
	}
	v, err := expr.Lookup("$.a.b", map[string]any{"a": map[string]any{"b": float64(1)}})
	if err != nil || v != float64(1) {
		t.Fatalf("Lookup 失败: %v %v", v, err)
	}
	if v, err := expr.Lookup("$.nope", map[string]any{}); err != nil || v != nil {
		t.Fatalf("缺失路径应返回 nil: %v %v", v, err)
	}
}
