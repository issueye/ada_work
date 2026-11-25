package ui

import (
	"strconv"
	"strings"

	"ada_chat/internal/app"
	mui "github.com/egoist/mygo/ui"
	"github.com/issueye/ada_t/plugin"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/field"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/theme"
)

var sendGlyph = mui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5"/><path d="m5 12 7-7 7 7"/></svg>`))

// composerArea 在对话底部放输入框卡片。
func (u *UI) composerArea(c *mui.Context, tk *theme.Tokens) {
	mui.Column(c).Shrink(0).Padding(12, 16, 14, 16).AlignItems(mui.Center).Children(func() {
		mui.Column(c).Width(conversationWidth).MaxWidthPercent(100).AlignItems(mui.Stretch).Children(func() {
			u.composerCard(c, tk)
		})
	})
}

func (u *UI) composerCard(c *mui.Context, tk *theme.Tokens) {
	edge := tk.Fill
	if tk.Dark {
		edge = tk.FillSecondary
	}
	mui.Column(c).Radius(20).Border(1, edge).Background(tk.BgElevated).ZIndex(1).
		Shadow(0, 1, 2, 0, mui.RGBA(0, 0, 0, 0.03)).Shadow(0, 12, 32, 0, mui.RGBA(0, 0, 0, 0.04)).
		Children(func() {
			// 无边框输入区会保留一圈透明的 1 DIP 边框；负外边距把它抵消掉，
			// 让文字落在卡片 12 DIP 的内边距上。
			u.queuedMessages(c, tk)
			mui.Box(c).MinHeight(92).Margin(-1, -1, 0, -1).Children(func() {
				placeholder := "给 " + u.assistantName() + " 发消息…"
				if u.App.Busy() {
					placeholder = "生成中…输入会排队，本轮结束后自动发送"
				}
				area := field.TextArea(c, &u.App.Draft, field.TextAreaProps{
					Variant:     field.InputBorderless,
					Placeholder: placeholder,
					MinRows:     3,
					MaxRows:     10,
				}).Label("给 " + u.assistantName() + " 发消息")
				area.Padding(0).FontSize(14).LineHeight(1.45)
				area.OnTextKey(func(k mui.TextKey) bool {
					send := k.Key == mui.KeyEnter &&
						((k.Mods == 0 && u.App.Prefs.EnterToSend) || k.Mods == mui.Cmd)
					if !send {
						return false
					}
					u.App.Send()
					return true
				})
			})
			mui.Row(c).Padding(4, 8, 8, 8).Gap(4).AlignItems(mui.Center).Justify(mui.SpaceBetween).Children(func() {
				mui.Row(c).Gap(2).AlignItems(mui.Center).Children(func() {
					attachButton(c)
					u.pluginSlot(c, plugin.SlotComposerAction)
					display.Tag(c, "本机运行", display.TagProps{
						Variant: display.TagBorderless, Color: display.TagGray, Size: theme.Small, Icon: lucide.Database(),
					})
				})
				mui.Row(c).Gap(10).AlignItems(mui.Center).Children(func() {
					display.Text(c, u.hintForSend(), display.TextProps{Type: display.TextSecondary, FontSize: 11})
					u.sendButton(c, tk)
				})
			})
		})
}

func (u *UI) hintForSend() string {
	if u.App.Busy() {
		if u.App.Prefs.EnterToSend {
			return "生成中 · Enter 排队 · Shift+Enter 换行"
		}
		return "生成中 · Cmd/Ctrl+Enter 排队 · Enter 换行"
	}
	if u.App.Prefs.EnterToSend {
		return "Enter 发送 · Shift+Enter 换行"
	}
	return "Cmd/Ctrl+Enter 发送 · Enter 换行"
}

func attachButton(c *mui.Context) {
	if action.ActionIcon(c, lucide.Paperclip(), action.ActionIconProps{Size: theme.Small, Title: "添加附件"}).Clicked() {
		feedback.ShowToast(c, feedback.ToastProps{
			Title:       "这个版本还没有接入附件",
			Description: "等上传服务接好，把文件拖进窗口即可。",
			Type:        feedback.ToastInfo,
		})
	}
}

func (u *UI) sendButton(c *mui.Context, tk *theme.Tokens) {
	// 机器人还在生成时，同一个按钮变成“停止生成”：RPC 取消这一轮，消息留在原地。
	if u.App.Busy() {
		b := mui.ButtonBase(c).Label("停止生成").Size(32, 32).Radius(16).Center().Cursor(mui.CursorPointer)
		bg := tk.Primary
		if b.Hovered() {
			bg = tk.PrimaryHover
		}
		b.Background(bg).TextColor(tk.PrimaryForeground)
		b.Children(func() { mui.Icon(c, lucide.Square()).FontSize(12) })
		if b.Clicked() && u.App.StopTurn() {
			feedback.ShowToast(c, feedback.ToastProps{Title: "已停止生成", Type: feedback.ToastInfo})
		}
		return
	}
	empty := strings.TrimSpace(u.App.Draft) == ""
	b := mui.ButtonBase(c).Label("发送").Size(32, 32).Radius(16).Center().Disabled(empty).Cursor(mui.CursorPointer)
	bg := tk.Primary
	if b.Hovered() && !empty {
		bg = tk.PrimaryHover
	}
	b.Background(bg).TextColor(tk.PrimaryForeground)
	if empty {
		b.Opacity(0.5).Cursor(mui.CursorNotAllowed)
	}
	b.Children(func() { mui.Icon(c, sendGlyph).FontSize(14) })
	if b.Clicked() {
		u.App.Send()
	}
}

// queuedMessages 是输入框上方的「待发送」列表:生成中按 Enter 的消息排在这里,
// 本轮结束时由 App 自动发出;每条可以单条移除。
func (u *UI) queuedMessages(c *mui.Context, tk *theme.Tokens) {
	s := u.App.Current()
	if s == nil || len(s.Queue) == 0 {
		return
	}
	remove := -1
	shown := len(s.Queue)
	if shown > 4 {
		shown = 4
	}
	mui.Column(c).Gap(2).Padding(8, 12, 0, 12).Children(func() {
		mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
			mui.Icon(c, lucide.ListOrdered()).FontSize(12).TextColor(tk.TextTertiary).Shrink(0)
			mui.Text(c, "待发送 "+strconv.Itoa(len(s.Queue))+" 条").FontSize(11).TextColor(tk.TextTertiary)
		})
		for i := 0; i < shown; i++ {
			i := i
			mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
				mui.Text(c, app.Clip(s.Queue[i], 96)).FontSize(12).TextColor(tk.TextSecondary).
					SingleLine().Grow(1).Basis(0).MinWidth(0)
				if action.ActionIcon(c, lucide.X(), small("移除排队消息")).Clicked() {
					remove = i
				}
			})
		}
		if len(s.Queue) > 4 {
			mui.Text(c, "还有 "+strconv.Itoa(len(s.Queue)-4)+" 条…").FontSize(11).TextColor(tk.TextTertiary)
		}
	})
	if remove >= 0 {
		u.App.DropQueued(s, remove)
	}
}
