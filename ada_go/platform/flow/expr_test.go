package flow_test

import (
	"testing"

	"github.com/issueye/ada_go_v2/platform/flow"
)

// 1. 路径、比较、逻辑与字面量。
func TestEvalExpressions(t *testing.T) {
	state := map[string]any{
		"scan": map[string]any{"verdict": "block", "score": float64(7)},
		"n":    float64(5),
		"on":   true,
	}
	cases := []struct {
		expr string
		want bool
	}{
		{`$.scan.verdict == "block"`, true},
		{`$.scan.verdict != "block"`, false},
		{`$.n > 3`, true},
		{`$.n <= 5 && $.on`, true},
		{`!($.n > 3)`, false},
		{`$.missing == null`, true},
		{`"a" < "b"`, true},
		{`$.scan.score >= 7 || false`, true},
		{`(($.n > 10) || ($.n < 6)) && $.on`, true},
	}
	for _, tc := range cases {
		got, err := flow.Eval(tc.expr, state)
		if err != nil {
			t.Fatalf("%q 求值失败: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("%q = %v,期望 %v", tc.expr, got, tc.want)
		}
	}
}

// 2. 非法表达式在编译期被发现,不做「尽量理解」。
func TestEvalRejectsBadExpressions(t *testing.T) {
	cases := []string{
		`$.n >`, `$.n === 1`, `(1`, `$.n > 3 extra`, `bad ident`,
		`$.n > "x"`, // 类型不可比,运行期报错
	}
	for _, expr := range cases {
		state := map[string]any{"n": float64(1)}
		if _, err := flow.Eval(expr, state); err == nil {
			t.Fatalf("非法表达式应报错: %q", expr)
		}
	}
	// 非布尔的条件也必须报错,而不是当成真/假。
	if _, err := flow.Eval(`$.n`, map[string]any{"n": float64(1)}); err == nil {
		t.Fatalf("非布尔条件应报错")
	}
	// 缺失路径在比较里是 null,不报错。
	if ok, err := flow.Eval(`$.nope == null`, map[string]any{}); err != nil || !ok {
		t.Fatalf("缺失路径应等于 null: ok=%v err=%v", ok, err)
	}
}
