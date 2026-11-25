package ui

import (
	"runtime"

	"github.com/egoist/mygo"

	mui "github.com/egoist/mygo/ui"
)

// WindowOptions 是打开窗口时的参数：尺寸、自绘标题栏和画这一页的视图。
func WindowOptions(v *UI) mygo.WindowOptions {
	o := mygo.WindowOptions{
		Title:           "Ada Chat",
		Width:           1180,
		Height:          780,
		MinWidth:        920,
		MinHeight:       560,
		StateKey:        "ada-chat-main",
		BackgroundColor: "light-dark(#f8f8f8, #0b0b0c)",
		Content:         mui.View(v.Root),
	}
	if runtime.GOOS == "windows" {
		// A frameless window lets MyGo cover Windows 10's visible white
		// non-client border, which a hidden title bar keeps. The title bar
		// draws its own minimize/maximize/close buttons.
		o.Frameless = true
		o.CornerRadius = 8
	} else {
		o.TitleBarStyle = mygo.TitleBarHidden
		o.TitleBarHeight = TitleBarHeight
	}
	return o
}
