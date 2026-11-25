package ui

import (
	"strconv"
	"strings"
	"time"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/field"
	"github.com/lobehub/lobe-mygo/pkg/layout"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/motion"
	"github.com/lobehub/lobe-mygo/pkg/overlay"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

// 侧栏留出足够宽度，让较长的对话标题也不至于挤到对话区。
const (
	sidebarMin     = 240
	sidebarMax     = 360
	sidebarDefault = 276
)

type sessionGroup struct {
	label string
	items []*app.Session
}

// sidebar 是对话来源列表：添加机器人、搜索框、机器人分组和对话，新的在前。
func (u *UI) sidebar(c *mui.Context) {
	layout.DraggablePanel(c, &u.sidebarWidth, &u.sidebarOpen, layout.DraggablePanelProps{
		Placement: layout.Left,
		MinSize:   sidebarMin,
		MaxSize:   sidebarMax,
		NoToggle:  true,
		NoBorder:  true,
	}, func() {
		tk := theme.TokensOf(c)
		mui.Column(c).Fill().TextColor(tk.TextSecondary).Children(func() {
			u.sidebarHeader(c)
			u.sidebarSearch(c)
			mui.Scroll(c).Grow(1).Basis(0).Children(func() {
				mui.Column(c).PaddingX(4).PaddingY(4).Gap(14).Children(func() {
					u.botSection(c)
					u.sessionSections(c)
					u.pluginSidebarSection(c)
				})
			})
			u.sidebarFooter(c)
		})
	})
}

// sidebarHeader 顶部的整行按钮：侧栏里不再有工作区下拉，改用一条“添加机器人”入口。
func (u *UI) sidebarHeader(c *mui.Context) {
	mui.Column(c).Shrink(0).Padding(8, 10, 2, 10).Children(func() { u.addBotButton(c) })
}

func (u *UI) addBotButton(c *mui.Context) {
	tk := theme.TokensOf(c)
	b := mui.ButtonBase(c).Label("添加机器人").Height(36).AlignSelf(mui.Stretch).
		Justify(mui.Center).Gap(8).Radius(tk.BorderRadiusSM).Cursor(mui.CursorPointer).TextColor(tk.Text)
	bg := tk.FillSecondary
	if b.Hovered() {
		bg = tk.Fill
	}
	b.Background(bg)
	b.Children(func() {
		mui.Icon(c, lucide.Plus()).FontSize(15).TextColor(tk.Primary)
		mui.Text(c, "添加机器人").FontSize(13).FontWeight(500).SingleLine()
	})
	if b.Clicked() {
		u.openBotDialog()
	}
}

func (u *UI) sidebarSearch(c *mui.Context) {
	mui.Box(c).Shrink(0).Padding(0, 10, 6, 10).Children(func() {
		field.SearchBar(c, &u.query, field.SearchBarProps{
			Placeholder: "搜索对话",
			ShortKey:    "mod+k",
			Size:        theme.Small,
			Variant:     field.InputFilled,
		}).Label("搜索对话")
	})
}

func (u *UI) sessionSections(c *mui.Context) {
	tk := theme.TokensOf(c)
	now := c.Now()
	active := ""
	if s := u.App.Current(); s != nil {
		active = keyOf(s.ID)
	}
	for _, g := range groupSessions(u.App.Sessions, u.query, now) {
		if len(g.items) == 0 {
			continue
		}
		mui.Row(c).Padding(10, 8, 2, 8).Gap(6).AlignItems(mui.Center).Children(func() {
			display.Text(c, strings.ToUpper(g.label), display.TextProps{Type: display.TextSecondary, FontSize: 11, Strong: true})
			mui.Spacer(c)
			display.Text(c, strconv.Itoa(len(g.items)), display.TextProps{Type: display.TextSecondary, FontSize: 11})
		})
		mui.Column(c).Gap(2).AlignSelf(mui.Stretch).Children(func() {
			for _, s := range g.items {
				if u.sessionRow(c, tk, s, keyOf(s.ID) == active) {
					u.App.SelectSession(s.ID)
				}
			}
		})
	}
}

var listRowTransition = mui.ElementTransition{Duration: 150 * time.Millisecond, Ease: motion.EaseOut, Colors: true}

// sessionRow 画一行会话:头像 + 标题/预览 + 尾部状态。
//
// 尾部状态按优先级:正在生成 → loading 动画;有未读 → 「新」;其余 → 相对时间。
// display.List 的 Extra 只能是字符串,放不下动画,所以这一行由本包自绘:
// 布局、字号与 display.List 的 Compact 行保持一致(32 高、12 间距、8 内边距)。
func (u *UI) sessionRow(c *mui.Context, tk *theme.Tokens, s *app.Session, active bool) bool {
	row := mui.ButtonBase(c).Label(s.Title).Row().Gap(12).AlignItems(mui.Center).
		Justify(mui.Start).MinHeight(32).PaddingY(6).PaddingX(8).Radius(tk.BorderRadius).
		FontSize(14).AlignSelf(mui.Stretch).Transition(listRowTransition)
	fg := tk.TextSecondary
	switch {
	case active:
		fg = tk.Text
		row.Background(tk.FillSecondary).Cursor(mui.CursorPointer)
	case row.Hovered():
		fg = tk.Text
		row.Background(tk.FillTertiary).Cursor(mui.CursorPointer)
	default:
		row.Cursor(mui.CursorPointer)
	}
	row.TextColor(fg)
	row.Children(func() {
		mui.Box(c).Shrink(0).Children(func() {
			if b := u.botForSession(s); b != nil {
				botAvatar(c, b.Icon, b.Color, 22)
				return
			}
			display.Avatar(c, s.Title, display.AvatarProps{Size: 22})
		})
		mui.Column(c).Gap(1).Grow(1).Basis(0).MinWidth(0).Children(func() {
			label := mui.Text(c, s.Title).SingleLine()
			if active {
				label.FontWeight(500)
			}
			if s.Preview != "" {
				mui.Text(c, s.Preview).SingleLine().FontSize(12).LineHeight(1.4).TextColor(tk.TextDescription)
			}
		})
		switch {
		case s.Running():
			// 这一行的「正在运行」就是动画本身:spinner 每帧请求帧,直到这一轮结束。
			feedback.Spin(c, feedback.SpinProps{CustomSize: 14}, nil).AlignSelf(mui.Center)
		case s.Unread > 0:
			display.Text(c, "新", display.TextProps{Type: display.TextSecondary, FontSize: 12})
		default:
			display.Text(c, app.RelativeTime(c.Now(), s.Updated), display.TextProps{Type: display.TextSecondary, FontSize: 12})
		}
	})
	// 右键菜单与会话行、机器人行共用 overlay.ContextMenu:
	// 系统菜单(mui.Menu)由宿主绘制,字体、行高、分隔线与阴影都与窗口内的
	// 机器人菜单不同,两条侧栏行摆在一起就会像两个产品。
	archiveLabel, archiveKey, archiveIcon := "归档", "archive", lucide.Archive()
	if s.Archived {
		archiveLabel, archiveKey, archiveIcon = "取消归档", "unarchive", lucide.ArchiveRestore()
	}
	switch overlay.ContextMenu(c, row, overlay.DropdownMenuProps{Items: []overlay.MenuItem{
		{Key: "copy", Label: "复制会话 ID", Icon: lucide.Copy()},
		{Key: archiveKey, Label: archiveLabel, Icon: archiveIcon},
		{Divider: true},
		{Key: "remove", Label: "移除", Icon: lucide.Trash2(), Danger: true},
	}}) {
	case "copy":
		c.WriteClipboard(strconv.Itoa(s.ID))
		feedback.ShowToast(c, feedback.ToastProps{Title: "已复制会话 ID", Description: "s-" + strconv.Itoa(s.ID), Type: feedback.ToastSuccess})
	case "archive":
		u.App.ArchiveSession(s.ID, true)
	case "unarchive":
		u.App.ArchiveSession(s.ID, false)
	case "remove":
		u.deleteID = s.ID
		u.confirmDel = true
	}
	return row.Clicked()
}

func keyOf(id int) string { return "s-" + strconv.Itoa(id) }

func groupSessions(all []*app.Session, query string, now time.Time) []sessionGroup {
	q := strings.ToLower(strings.TrimSpace(query))
	if q != "" {
		var hits []*app.Session
		for _, s := range all {
			if strings.Contains(strings.ToLower(s.Title), q) || strings.Contains(strings.ToLower(s.Preview), q) {
				hits = append(hits, s)
			}
		}
		return []sessionGroup{{label: "搜索结果", items: hits}}
	}
	var pinned, today, earlier, archived []*app.Session
	for _, s := range all {
		switch {
		case s.Archived:
			archived = append(archived, s)
		case s.Pinned:
			pinned = append(pinned, s)
		case now.Sub(s.Updated) < 24*time.Hour:
			today = append(today, s)
		default:
			earlier = append(earlier, s)
		}
	}
	return []sessionGroup{
		{label: "置顶", items: pinned},
		{label: "今天", items: today},
		{label: "更早", items: earlier},
		{label: "归档", items: archived},
	}
}

func (u *UI) sidebarFooter(c *mui.Context) {
	tk := theme.TokensOf(c)
	layout.Divider(c, layout.DividerProps{})
	mui.Row(c).Shrink(0).Padding(8).Gap(4).AlignItems(mui.Center).Children(func() {
		// 左下角用户区本身就是入口:点它弹出 Popover,里面分头进 Agent / 供应商 / 常规设置。
		user := mui.ButtonBase(c).Label(u.App.UserName()).Row().Gap(8).AlignItems(mui.Center).
			Grow(1).Basis(0).MinWidth(0).Padding(4, 4, 4, 6).Radius(tk.BorderRadiusSM).
			Cursor(mui.CursorPointer).AlignSelf(mui.Stretch)
		if u.userMenu || user.Hovered() {
			user.Background(tk.FillTertiary)
		}
		user.Children(func() {
			display.Avatar(c, u.App.UserName(), display.AvatarProps{Size: 26})
			mui.Column(c).Grow(1).Basis(0).MinWidth(0).Children(func() {
				mui.Text(c, u.App.UserName()).FontSize(13).FontWeight(500).TextColor(tk.Text).SingleLine()
			})
			// 展开时箭头朝下,收起时朝上:同一帧里跟着状态变。
			icon := lucide.ChevronUp()
			if u.userMenu {
				icon = lucide.ChevronDown()
			}
			mui.Icon(c, icon).FontSize(14).TextColor(tk.TextTertiary).Shrink(0)
		})
		if user.Clicked() {
			u.userMenu = !u.userMenu
		}
		mui.Popover(c, user, &u.userMenu, func() {
			mui.Column(c).Gap(2).Width(300).Padding(4).Children(func() {
				u.menuRow(c, tk, lucide.Bot(), "Agent", "路径", u.openAgentSettings)
				u.menuRow(c, tk, lucide.Cpu(), "供应商", "模型", u.openProviderSettings)
				u.menuRow(c, tk, lucide.SlidersHorizontal(), "常规设置", "名称", u.openSettings)
				icon, hint := lucide.Moon(), "浅色"
				if theme.TokensOf(c).Dark {
					icon, hint = lucide.Sun(), "深色"
				}
				u.menuRow(c, tk, icon, "明暗切换", hint, func() { u.toggleTheme(c) })
			})
		})
	})
}

// menuRow 是 Popover 里的一行入口:图标 + 标题/说明 + 点击回调。
// 行本身是带可访问名字的 ButtonBase,键盘与读屏行为与其它按钮一致。
// menuRow 是 Popover 里的一行入口:左边图标 + 标题,右边一句极短提示。
// 单行、窄高(28),比上下两行的菜单更紧凑;行本身是带可访问名字的 ButtonBase。
func (u *UI) menuRow(c *mui.Context, tk *theme.Tokens, icon *mui.SVG, title, hint string, onClick func()) {
	row := mui.ButtonBase(c).Label(title).Row().Gap(8).AlignItems(mui.Center).
		PaddingY(4).PaddingX(8).MinHeight(28).Radius(tk.BorderRadiusSM).
		Cursor(mui.CursorPointer).AlignSelf(mui.Stretch)
	if row.Hovered() {
		row.Background(tk.FillTertiary)
	}
	row.Children(func() {
		mui.Icon(c, icon).FontSize(15).TextColor(tk.TextSecondary).Shrink(0)
		mui.Text(c, title).FontSize(13).TextColor(tk.Text).SingleLine()
		mui.Spacer(c)
		if hint != "" {
			mui.Text(c, hint).FontSize(11).TextColor(tk.TextTertiary).SingleLine()
		}
	})
	if row.Clicked() {
		onClick()
	}
}

// toggleTheme 在浅色/深色之间切换并落盘;跟随系统时按当前实际外观决定切到哪一边。
func (u *UI) toggleTheme(c *mui.Context) {
	dark := theme.TokensOf(c).Dark
	switch u.App.Prefs.Theme {
	case "dark":
		u.App.Prefs.Theme = "light"
	case "light":
		u.App.Prefs.Theme = "dark"
	default:
		if dark {
			u.App.Prefs.Theme = "light"
		} else {
			u.App.Prefs.Theme = "dark"
		}
	}
	_ = u.App.SavePrefs()
}
