package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/ui"
)

// TestThemeToggle 守住左下角用户 Popover 的明暗切换:一次切到深色,再切回浅色。
func TestThemeToggle(t *testing.T) {
	a := baseApp(t)
	a.Prefs.Theme = "light"

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 760)
	tt.Frame()

	if err := tt.Click("老王"); err != nil {
		t.Fatalf("打开用户菜单失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("明暗切换"); !ok {
		t.Fatalf("Popover 里应有明暗切换入口")
	}

	if err := tt.Click("明暗切换"); err != nil {
		t.Fatalf("切到深色失败: %v", err)
	}
	tt.Frame()
	if a.Prefs.Theme != "dark" {
		t.Fatalf("第一次切换应写入 dark: %q", a.Prefs.Theme)
	}
	shot(t, tt, "theme-dark.png")

	if err := tt.Click("明暗切换"); err != nil {
		t.Fatalf("切回浅色失败: %v", err)
	}
	tt.Frame()
	if a.Prefs.Theme != "light" {
		t.Fatalf("第二次切换应写回 light: %q", a.Prefs.Theme)
	}
	shot(t, tt, "theme-light.png")
}
