package sched_test

import (
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/sched"
)

func mustParse(t *testing.T, spec string) *sched.Cron {
	t.Helper()
	c, err := sched.ParseCron(spec)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", spec, err)
	}
	return c
}

func nextAt(t *testing.T, spec string, after time.Time) time.Time {
	t.Helper()
	c := mustParse(t, spec)
	next, ok := c.Next(after)
	if !ok {
		t.Fatalf("%q 在 %s 之后没有下一次触发", spec, after)
	}
	return next
}

// 1. 基础语法:通配、步长、范围、列表、月份/星期缩写。
func TestCronNext(t *testing.T) {
	base := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC) // 2026-01-01 是周四

	cases := []struct {
		name  string
		spec  string
		after time.Time
		want  time.Time
	}{
		{"每分钟", "* * * * *", base, time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)},
		{"步长", "*/15 * * * *", time.Date(2026, 1, 1, 10, 7, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)},
		{"范围步长", "10-30/5 * * * *", time.Date(2026, 1, 1, 10, 11, 0, 0, time.UTC), time.Date(2026, 1, 1, 10, 15, 0, 0, time.UTC)},
		{"列表", "0 9,18 * * *", time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC)},
		{"工作日", "0 9 * * mon-fri", time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC), time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)},
		{"月份缩写", "0 0 1 jan *", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"星期缩写与 7=周日", "0 0 * * sun", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)},
		{"闰日", "0 0 29 2 *", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextAt(t, tc.spec, tc.after)
			if !got.Equal(tc.want) {
				t.Fatalf("%q: 期望 %s,实际 %s", tc.spec, tc.want, got)
			}
		})
	}
}

// 2. 日期与星期都受限时,按标准 cron 语义「任一满足」。
func TestCronDOMOrDOW(t *testing.T) {
	// 2026-01-01 是周四;13 号与周五里先到的是 1 月 2 日(周五)。
	after := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got := nextAt(t, "0 0 13 * 5", after)
	want := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("dom/dow 应取先到的那个: 期望 %s,实际 %s", want, got)
	}
}

// 3. 永不存在的日期:5 年内无解必须返回 false,而不是死循环。
func TestCronImpossibleDate(t *testing.T) {
	c := mustParse(t, "0 0 30 2 *")
	if _, ok := c.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("2 月 30 日不应有下次触发")
	}
}

// 4. 非法表达式必须报错,不做「尽量理解」。
func TestCronRejectsBadSpecs(t *testing.T) {
	cases := []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * * *,,*",
	}
	for _, spec := range cases {
		if _, err := sched.ParseCron(spec); err == nil {
			t.Fatalf("非法表达式应被拒绝: %q", spec)
		}
	}
}

// 5. Next 不返回 after 所在的那一分钟(避免同一分钟触发两次)。
func TestCronStrictlyAfter(t *testing.T) {
	after := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	got := nextAt(t, "* * * * *", after)
	if !got.After(after) {
		t.Fatalf("下一次触发必须严格晚于 after: %s", got)
	}
	if got.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("不应在同一分钟触发两次")
	}
}
