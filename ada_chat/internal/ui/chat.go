package ui

import (
	"strconv"
	"strings"

	mui "github.com/egoist/mygo/ui"
	"github.com/issueye/ada_t/plugin"
	"github.com/lobehub/lobe-mygo/pkg/action"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/layout"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/markdown"
	"github.com/lobehub/lobe-mygo/pkg/overlay"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

// 对话区是一条固定宽度的列，和 lobe-chat 在大窗口里保持一致。
const (
	conversationWidth = 880
	chatHeaderHeight  = 54
)

// chatPanel 是放对话的卡片，位于侧栏右侧。
func (u *UI) chatPanel(c *mui.Context) {
	tk := theme.TokensOf(c)
	inset := float32(0)
	if !u.sidebarOpen {
		inset = 8
	}
	edge := tk.Border
	if tk.Dark {
		edge = tk.BorderSecondary
	}
	mui.Column(c).Grow(1).Basis(0).MinWidth(0).Margin(0, 8, 8, inset).
		Radius(tk.BorderRadiusLG).Background(tk.BgContainer).Border(1, edge).Clip().
		Children(func() {
			if u.App.Current() == nil {
				u.welcomePanel(c, tk)
				return
			}
			u.chatHeader(c, tk)
			mui.Box(c).Height(1).Shrink(0).Background(tk.BorderSecondary)
			mui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(mui.Center).Children(func() {
				u.transcript(c, tk)
			})
			u.composerArea(c, tk)
		})
}

// welcomePanel 是没有任何对话时的空工作台：这里没有预置聊天，只引导添加机器人。
func (u *UI) welcomePanel(c *mui.Context, tk *theme.Tokens) {
	mui.Column(c).Fill().Center().Gap(18).Padding(32).Children(func() {
		display.Empty(c, display.EmptyProps{
			Icon:        lucide.Bot(),
			Title:       "还没有对话",
			Description: "添加一个机器人，选择它的工作目录。消息会交给那个工作区里的进程。",
		})
		if action.Button(c, "添加机器人", action.ButtonProps{Variant: action.Primary}).Clicked() {
			u.openBotDialog()
		}
	})
}

func (u *UI) chatHeader(c *mui.Context, tk *theme.Tokens) {
	s := u.App.Current()
	if s == nil {
		return
	}
	bot := u.botForSession(s)
	mui.Row(c).Height(chatHeaderHeight).Shrink(0).Padding(8, 16).Gap(8).AlignItems(mui.Center).Justify(mui.SpaceBetween).Children(func() {
		mui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(3).Children(func() {
			mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
				mui.Text(c, s.Title).FontSize(15).FontWeight(600).TextColor(tk.Text).SingleLine().MaxWidth(560)
				if bot != nil {
					display.Tag(c, bot.Name, display.TagProps{
						Variant: display.TagBorderless, Color: display.TagGray, Size: theme.Small, Icon: botGlyphSVG(bot.Icon),
					})
				}
				if s.Pinned {
					display.Tag(c, "已置顶", display.TagProps{Variant: display.TagBorderless, Color: display.TagGold, Size: theme.Small, Icon: lucide.Pin()})
				}
			})
			mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
				if bot != nil && bot.Model != "" {
					display.Tag(c, bot.Model, display.TagProps{Variant: display.TagBorderless, Color: display.TagGray, Size: theme.Small})
				}
				if bot != nil {
					tag := display.Tag(c, compactPath(bot.Workspace), display.TagProps{
						Variant: display.TagBorderless, Color: display.TagGray, Size: theme.Small, Icon: lucide.FolderClosed(),
					})
					overlay.Tooltip(c, tag, "工作目录: "+bot.Workspace, overlay.TooltipProps{})
				}
				display.Text(c, "·", display.TextProps{Type: display.TextSecondary, FontSize: 12})
				display.Text(c, messageCount(len(s.Messages)), display.TextProps{Type: display.TextSecondary, FontSize: 12})
			})
		})
		mui.Row(c).Gap(4).AlignItems(mui.Center).Children(func() {
			u.pluginSlot(c, plugin.SlotChatHeader)
			u.conversationMenu(c, s)
		})
	})
}

// botForSession 返回这条对话绑定的机器人。
func (u *UI) botForSession(s *app.Session) *app.Bot {
	if s == nil || s.BotID == 0 {
		return nil
	}
	return u.App.BotByID(s.BotID)
}

// assistantName 是当前对话里回答者的名字。
func (u *UI) assistantName() string {
	if b := u.botForSession(u.App.Current()); b != nil {
		return b.Name
	}
	return "机器人"
}

func messageCount(n int) string {
	switch {
	case n == 0:
		return "暂无消息"
	case n == 1:
		return "1 条消息"
	}
	return strconv.Itoa(n) + " 条消息"
}

func (u *UI) conversationText(s *app.Session) string {
	var b strings.Builder
	for i, m := range s.Messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if m.Role == app.RoleUser {
			b.WriteString("**" + u.App.UserName() + "：** ")
		} else {
			name := "机器人"
			if bot := u.botForSession(s); bot != nil {
				name = bot.Name
			}
			b.WriteString("**" + name + "：** ")
		}
		b.WriteString(m.Text)
	}
	return b.String()
}

func (u *UI) conversationMenu(c *mui.Context, s *app.Session) {
	pinLabel, pinIcon := "置顶对话", lucide.Pin()
	if s.Pinned {
		pinLabel, pinIcon = "取消置顶", lucide.PinOff()
	}
	items := make([]overlay.MenuItem, 0, 6)
	if u.App.Busy() {
		items = append(items,
			overlay.MenuItem{Key: "stop", Label: "停止生成", Icon: lucide.Square()},
			overlay.MenuItem{Divider: true},
		)
	}
	items = append(items,
		overlay.MenuItem{Key: "pin", Label: pinLabel, Icon: pinIcon},
		overlay.MenuItem{Key: "rename", Label: "重命名", Icon: lucide.Pencil()},
		overlay.MenuItem{Key: "copy", Label: "复制对话", Icon: lucide.Copy()},
		overlay.MenuItem{Divider: true},
		overlay.MenuItem{Key: "delete", Label: "删除对话", Icon: lucide.Trash2(), Danger: true},
	)
	chosen := overlay.DropdownMenu(c, &u.chatMenu, overlay.DropdownMenuProps{Items: items}, func(t mui.Element) {
		action.StyleActionIcon(c, t, lucide.Ellipsis(), action.ActionIconProps{Size: theme.Small, Title: "对话操作", Active: u.chatMenu})
	})
	switch chosen {
	case "stop":
		if u.App.StopTurn() {
			feedback.ShowToast(c, feedback.ToastProps{Title: "已停止生成", Type: feedback.ToastInfo})
		}
	case "pin":
		s.Pinned = !s.Pinned
	case "rename":
		u.renameDraft = s.Title
		u.renameUp = true
	case "copy":
		c.WriteClipboard(u.conversationText(s))
		feedback.ShowToast(c, feedback.ToastProps{Title: "已复制对话", Type: feedback.ToastSuccess})
	case "delete":
		u.deleteID = s.ID
		u.confirmDel = true
	}
}

func (u *UI) transcript(c *mui.Context, tk *theme.Tokens) {
	s := u.App.Current()
	if s == nil {
		return
	}
	u.resetListIfSwitched()
	msgs := s.Messages
	u.chatList.Key = func(i int) any { return msgs[i].ID }
	u.chatList.Label = func(i int) string {
		name := u.assistantName()
		if msgs[i].Role == app.RoleUser {
			name = u.App.UserName()
		}
		label := name + ": " + app.Clip(msgs[i].Text, 80)
		if n := len(msgs[i].Tools); n > 0 {
			label = name + "（" + strconv.Itoa(n) + " 个工具）：" + app.Clip(msgs[i].Text, 80)
		}
		return label
	}
	u.chatList.FollowEnd = true
	u.chatList.FollowAnimation = &mui.Smooth

	list := mui.List(c, &u.chatList, len(msgs), func(i int) {
		u.messageRow(c, tk, msgs[i])
	}).Grow(1).Basis(0).Width(conversationWidth).MaxWidthPercent(100).
		Padding(16, 24, 8, 24).Gap(4)
	if len(msgs) == 0 {
		list.Justify(mui.Center).Children(func() { u.emptyChat(c, tk) })
	} else {
		list.Justify(mui.End)
	}
	// 列表行可能要求边构建边改动列表：先记下来，等列表建完再执行。
	if fn := u.pending; fn != nil {
		u.pending = nil
		fn()
	}
}

func (u *UI) emptyChat(c *mui.Context, tk *theme.Tokens) {
	bot := u.botForSession(u.App.Current())
	title, desc := "开始一次对话", "消息会交给这个工作目录里的机器人进程。"
	if bot != nil {
		title = "和 " + bot.Name + " 对话"
		desc = "它运行在 " + bot.Workspace + "，第一句话会发到那个进程。"
	}
	mui.Column(c).Gap(20).Padding(24, 0, 8, 0).AlignItems(mui.Center).Children(func() {
		display.Empty(c, display.EmptyProps{
			Icon:        lucide.MessageSquare(),
			Title:       title,
			Description: desc,
		})
	})
}

func (u *UI) messageRow(c *mui.Context, tk *theme.Tokens, m *app.Message) {
	if m.Role == app.RoleUser {
		u.userMessage(c, tk, m)
		return
	}
	u.assistantMessage(c, tk, m)
}

func (u *UI) userMessage(c *mui.Context, tk *theme.Tokens, m *app.Message) {
	mui.Row(c).Justify(mui.End).PaddingY(10).Children(func() {
		mui.Column(c).MaxWidthPercent(80).Gap(3).AlignItems(mui.End).Children(func() {
			b := layout.Block(c, layout.BlockProps{Variant: layout.BlockFilled}).Padding(10, 14).Radius(tk.BorderRadiusLG)
			b.Children(func() {
				mui.Text(c, m.Text).FontSize(14).LineHeight(1.5).TextColor(tk.Text)
			})
			display.Text(c, app.RelativeTime(c.Now(), m.At)+" · "+u.App.UserName(), display.TextProps{
				Type: display.TextSecondary, FontSize: 11,
			})
		})
	})
}

func (u *UI) assistantMessage(c *mui.Context, tk *theme.Tokens, m *app.Message) {
	bot := u.botForSession(u.App.Current())
	name := "机器人"
	if bot != nil {
		name = bot.Name
	}
	mui.Row(c).Gap(12).AlignItems(mui.Start).PaddingY(10).Children(func() {
		mui.Box(c).PaddingY(4).Shrink(0).Children(func() {
			if bot != nil {
				botAvatar(c, bot.Icon, bot.Color, 30)
				return
			}
			mui.Icon(c, lucide.Bot()).FontSize(24).TextColor(tk.TextTertiary)
		})
		mui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(6).Children(func() {
			mui.Row(c).Gap(8).AlignItems(mui.Center).Children(func() {
				display.Text(c, name, display.TextProps{FontSize: 13, Strong: true})
				display.Text(c, app.RelativeTime(c.Now(), m.At), display.TextProps{Type: display.TextSecondary, FontSize: 11})
				if m.Streaming {
					display.Text(c, "思考中", display.TextProps{Type: display.TextSecondary, FontSize: 11, Shiny: true})
				}
			})
			if len(m.Parts) > 0 {
				// 按真实顺序渲染：模型可能先写一句话，再调用工具，工具之后再继续写。
				mui.Column(c).Gap(6).Children(func() {
					for i, part := range m.Parts {
						switch part.Kind {
						case app.PartTool:
							if call := toolByID(m, part.ToolID); call != nil {
								toolCard(c, tk, call)
							}
						case app.PartText:
							if strings.TrimSpace(part.Text) == "" {
								continue
							}
							// 只有最后一段文本还在流入时做打字动画；被工具打断的那段已经定型。
							animated := m.Streaming && i == len(m.Parts)-1
							markdown.Markdown(c, part.Text, markdown.MarkdownProps{
								Variant:  markdown.Chat,
								Animated: animated,
							})
						case app.PartReasoning:
							if strings.TrimSpace(part.Text) == "" {
								continue
							}
							u.reasoningCard(c, tk, m, i, &m.Parts[i], m.Streaming && i == len(m.Parts)-1)
						}
					}
					// 正文、工具都画完后,如果这一轮还在跑,补一个尾部的 loading:
					// 模型在写下一段、或工具在长时间执行时,界面必须有「还在动」的信号。
					// 最后一段是仍在流入的思考块时跳过——思考卡片自己已经有 spinner。
					if m.Streaming && !tailReasoning(m) {
						mui.Row(c).Gap(6).AlignItems(mui.Center).Children(func() {
							feedback.Spin(c, feedback.SpinProps{CustomSize: 12}, nil).AlignSelf(mui.Center)
							display.Text(c, "生成中", display.TextProps{Type: display.TextSecondary, FontSize: 11})
						})
					}
				})
			} else {
				if len(m.Tools) > 0 {
					mui.Column(c).Gap(8).Children(func() {
						for _, call := range m.Tools {
							toolCard(c, tk, call)
						}
					})
				}
				switch {
				case m.Text == "" && m.Streaming && len(m.Tools) == 0:
					feedback.Spin(c, feedback.SpinProps{Size: theme.Small}, nil)
				case m.Text != "":
					markdown.Markdown(c, m.Text, markdown.MarkdownProps{
						Variant:  markdown.Chat,
						Animated: m.Streaming,
					})
				}
			}
			if !m.Streaming {
				u.messageActions(c, m)
			}
		})
	})
}

// reasoningCard 显示模型这一段思考内容：默认展开，用户也可以收起；思考流式进行时带 spinner。
func (u *UI) reasoningCard(c *mui.Context, tk *theme.Tokens, m *app.Message, index int, part *app.Part, streaming bool) {
	p := mui.CollapsibleBase(c.Key("reasoning:"+strconv.Itoa(m.ID)+":"+strconv.Itoa(index)), &part.Open)
	card := layout.Block(c, layout.BlockProps{Variant: layout.BlockOutlined}).AlignSelf(mui.Stretch).FillWidth().Clip()
	card.Background(tk.FillQuaternary)
	card.Children(func() {
		tr := p.Trigger.Label("思考过程").Row().FillWidth().Padding(8, 12).Gap(8).AlignItems(mui.Center).
			Cursor(mui.CursorPointer).Unselectable()
		if tr.Hovered() {
			tr.Background(tk.FillTertiary)
		}
		tr.Children(func() {
			mui.Icon(c, lucide.Brain()).FontSize(14).TextColor(tk.TextTertiary)
			mui.Text(c, "思考过程").FontSize(12).FontWeight(500).TextColor(tk.TextSecondary)
			if streaming {
				// Spin 自带 AlignSelf(Start);在居中行里要改回 Center，否则会比文字高一截。
				feedback.Spin(c, feedback.SpinProps{CustomSize: 10}, nil).AlignSelf(mui.Center)
			}
			mui.Spacer(c)
			mui.Icon(c, lucide.ChevronDown()).FontSize(14).TextColor(tk.TextTertiary).Rotate(180 * p.Progress())
		})
		p.Panel(func() {
			// 上留白与左留白一致(12)：思考正文不贴标题行。
			mui.Column(c).Padding(12, 12, 10, 12).Children(func() {
				markdown.Markdown(c, part.Text, markdown.MarkdownProps{
					Variant:  markdown.Chat,
					FontSize: 13,
					Animated: streaming,
				})
			})
		})
	})
}

func (u *UI) messageActions(c *mui.Context, m *app.Message) {
	s := u.App.Current()
	if s == nil {
		return
	}
	chosen := action.ActionIconGroup(c, action.ActionIconGroupProps{
		Variant: action.GroupBorderless,
		Size:    theme.Small,
		Items: []action.ActionGroupItem{
			{Key: "copy", Icon: lucide.Copy(), Label: "复制回复"},
			{Key: "regenerate", Icon: lucide.RefreshCw(), Label: "重新生成"},
		},
		Menu: []overlay.MenuItem{
			{Key: "delete", Label: "删除消息", Icon: lucide.Trash2(), Danger: true},
		},
	})
	switch chosen {
	case "copy":
		c.WriteClipboard(m.Text)
		feedback.ShowToast(c, feedback.ToastProps{Title: "已复制到剪贴板", Type: feedback.ToastSuccess})
	case "regenerate", "delete":
		// 列表正在构建：等它建完再改。
		u.pending = func() {
			if chosen == "regenerate" {
				u.App.Regenerate(s)
				return
			}
			u.App.DeleteMessage(s, m.ID)
			feedback.ShowToast(c, feedback.ToastProps{Title: "消息已删除", Type: feedback.ToastInfo})
		}
	}
}

// tailReasoning 报告这一轮最后一个可见 part 是不是仍在流入的思考块。
// 那种情况下思考卡片自己带着 spinner,正文尾部不必再放一个。
func tailReasoning(m *app.Message) bool {
	if m == nil || len(m.Parts) == 0 {
		return false
	}
	last := m.Parts[len(m.Parts)-1]
	return last.Kind == app.PartReasoning && strings.TrimSpace(last.Text) != ""
}
