package version_test

import (
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/version"
)

// 1. 解析与比较:预发布段的排序符合 semver。
func TestParseAndCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "1.0.0-beta.1", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.10", -1},
		{"v1.2.3", "1.2.3", 0},
	}
	for _, tc := range cases {
		a, err := version.Parse(tc.a)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", tc.a, err)
		}
		b, err := version.Parse(tc.b)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", tc.b, err)
		}
		if got := version.Compare(a, b); got != tc.want {
			t.Fatalf("Compare(%q,%q)=%d,期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, bad := range []string{"", "1.2", "1.2.3.4", "x.y.z", "1.2.-3", "1.2.3-"} {
		if _, err := version.Parse(bad); err == nil {
			t.Fatalf("%q 应解析失败", bad)
		}
	}
}

// 2. 约束:各运算符与非法形式。
func TestConstraints(t *testing.T) {
	host, _ := version.Parse("2.1.0")
	cases := []struct {
		constraint string
		want       bool
	}{
		{">=2.0.0", true},
		{">2.1.0", false},
		{"<=2.1.0", true},
		{"<2.1.0", false},
		{"=2.1.0", true},
		{"==2.1.0", true},
		{"!=2.1.0", false},
		{"2.0.0", false},
	}
	for _, tc := range cases {
		c, err := version.ParseConstraint(tc.constraint)
		if err != nil {
			t.Fatalf("解析约束 %q 失败: %v", tc.constraint, err)
		}
		if got := c.Check(host); got != tc.want {
			t.Fatalf("%q 对 %s = %v,期望 %v", tc.constraint, host, got, tc.want)
		}
	}
	for _, bad := range []string{"", ">=x", "^2.0.0", "~2.0.0", "2.0.0 || 3.0.0"} {
		if _, err := version.ParseConstraint(bad); err == nil {
			t.Fatalf("约束 %q 应被拒绝", bad)
		}
	}
}

// 3. Satisfy:满足返回 nil,不满足返回可读错误。
func TestSatisfy(t *testing.T) {
	if err := version.Satisfy(">=2.0.0", "2.1.0"); err != nil {
		t.Fatalf("应满足: %v", err)
	}
	if err := version.Satisfy(">=2.0.0", "1.9.0"); err == nil || !strings.Contains(err.Error(), "不满足") {
		t.Fatalf("不满足应报错,实际 %v", err)
	}
}
