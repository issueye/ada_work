package ui

import (
	"runtime"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/theme"
)

func small(title string) action.ActionIconProps {
	return action.ActionIconProps{Size: theme.Small, Title: title}
}

// titleBar 在系统窗口按钮下方画出窗口自己的标题栏。
//
// Windows 用过 Frameless 窗口：它不保留 Windows 10 那条聚焦时可见的白色
// 非客户区边框，也因此没有系统标题栏按钮，三枚按钮由这里自己画。
// macOS/Linux 仍用 TitleBarHidden，系统按钮保留。
func (u *UI) titleBar(c *mui.Context) {
	bar := c.TitleBar()
	height := float32(TitleBarHeight)
	if bar.Height > 0 {
		height = bar.Height
	}
	inset := float32(10)
	if bar.Left > 0 {
		inset = bar.Left + 8
	}
	right := float32(10)
	custom := runtime.GOOS == "windows"
	if custom {
		right = 0
	} else if bar.Right > 0 {
		right = bar.Right + 8
	}

	root := mui.Row(c).Height(height).Shrink(0).AlignItems(mui.Center).
		Padding(0, right, 0, inset)
	root.Children(func() {
		drag := mui.Row(c).Grow(1).FillHeight().AlignItems(mui.Center).DragWindow()
		drag.Children(func() {
			icon := lucide.PanelLeftClose()
			title := "收起侧栏"
			if !u.sidebarOpen {
				icon, title = lucide.PanelLeftOpen(), "展开侧栏"
			}
			if action.ActionIcon(c, icon, small(title)).Clicked() {
				u.sidebarOpen = !u.sidebarOpen
			}

			mui.Row(c).Margin(0, 0, 0, 10).Gap(8).AlignItems(mui.Center).Children(func() {
				mui.Box(c).Size(24, 24).Children(func() {
					display.AgentAvatar(c, display.AgentAvatarProps{Seed: "ada", Name: "Ada", Size: 24})
				})
				mui.Text(c, "Ada Chat").FontSize(13).FontWeight(600).SingleLine()
				display.Tag(c, "本地", display.TagProps{Variant: display.TagBorderless, Color: display.TagGray, Size: theme.Small})
			})

			mui.Spacer(c)

			if action.ActionIcon(c, lucide.SlidersHorizontal(), small("设置")).Clicked() {
				u.openSettings()
			}
		})
		if custom {
			u.windowControls(c, height)
		}
	})
}

// windowControls draws the minimize, maximize/restore and close buttons of a
// frameless Windows title bar. Their colors come from the current theme, so
// they stay visible in both light and dark mode.
func (u *UI) windowControls(c *mui.Context, height float32) {
	tk := theme.TokensOf(c)
	control := func(label string, icon *mui.SVG, close bool, fn func()) {
		b := mui.ButtonBase(c).Width(46).Height(height).Radius(0).
			Label(label).FocusRing(false).Cursor(mui.CursorPointer)
		hovered, pressed := b.Hovered(), b.Pressed()
		if hovered || pressed {
			if close {
				b.Background(mui.Hex("#C42B1C"))
			} else {
				b.Background(tk.FillTertiary)
			}
		}
		if pressed && !close {
			b.Background(tk.FillSecondary)
		}
		fg := tk.Text
		if hovered {
			fg = tk.Text
			if close {
				fg = mui.Hex("#FFFFFF")
			}
		}
		b.Children(func() {
			mui.Icon(c, icon).FontSize(12).TextColor(fg)
		})
		if b.Clicked() {
			fn()
		}
	}
	controls := mui.Row(c).Height(height).AlignItems(mui.Center)
	controls.Children(func() {
		if u.win != nil {
			control("最小化", lucide.Minus(), false, func() { u.win.Minimize() })
			if u.win.IsMaximized() {
				control("还原", lucide.Copy(), false, func() { u.win.Unmaximize() })
			} else {
				control("最大化", lucide.Square(), false, func() { u.win.Maximize() })
			}
			control("关闭", lucide.X(), true, func() { u.win.Close() })
			return
		}
		control("最小化", lucide.Minus(), false, func() {})
		control("最大化", lucide.Square(), false, func() {})
		control("关闭", lucide.X(), true, func() {})
	})
}

// openSettings 打开常规设置:先把已保存的偏好放回表单,并关掉其它弹窗/菜单。
func (u *UI) openSettings() {
	u.settings.ResetTo(u.App.Prefs)
	u.showKey = false
	u.userMenu = false
	u.agentUp, u.providerUp = false, false
	u.settingsUp = true
}
