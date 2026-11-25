package ui_test

import (
	"runtime"
	"testing"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/ui"
)

// TestWindowControls 守住 Windows 的 frameless 标题栏：最小化、最大化、
// 关闭三枚按钮必须由视图自己画出来，而不是依赖不可见的系统按钮。
func TestWindowControls(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows uses the frameless custom title bar")
	}
	a := baseApp(t)
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1000, 700)
	tt.Frame()
	for _, label := range []string{"最小化", "最大化", "关闭"} {
		if _, ok := tt.Find(label); !ok {
			t.Fatalf("标题栏缺少按钮 %q", label)
		}
	}
}

// TestWindowOptionsFramelessOnWindows 守住白边框修复：Windows 走 Frameless，
// 由 MyGo 用客户区盖住 Windows 10 的可见非客户区线；其它平台保留原生标题栏。
func TestWindowOptionsFramelessOnWindows(t *testing.T) {
	a := baseApp(t)
	u := ui.New(a)
	o := ui.WindowOptions(u)
	if runtime.GOOS == "windows" {
		if !o.Frameless {
			t.Fatalf("Windows 应使用 Frameless 窗口")
		}
		if o.TitleBarStyle != "" {
			t.Fatalf("Frameless 窗口不应再设 TitleBarStyle: %q", o.TitleBarStyle)
		}
	} else if o.TitleBarStyle != "hidden" {
		t.Fatalf("非 Windows 应保留隐藏标题栏: %q", o.TitleBarStyle)
	}
}
