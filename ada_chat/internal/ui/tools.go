package ui

import (
	"strconv"
	"time"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/code"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/feedback"
	"github.com/lobehub/lobe-mygo/pkg/layout"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/markdown"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

func toolIcon(name string) *mui.SVG {
	switch name {
	case "run_shell":
		return lucide.Terminal()
	case "read_file":
		return lucide.FileText()
	case "write_file", "edit_file":
		return lucide.Pencil()
	case "search_files":
		return lucide.Search()
	case "list_dir":
		return lucide.Folder()
	}
	return lucide.Wrench()
}

// toolByID 在一轮回答里找到卡片对应的调用，供按 Part 顺序渲染使用。
func toolByID(m *app.Message, id string) *app.ToolCall {
	for _, call := range m.Tools {
		if call.ID == id {
			return call
		}
	}
	return nil
}

// toolKey 让同一轮里不同工具的折叠状态互不影响。
func toolKey(call *app.ToolCall) string {
	if call.ID != "" {
		return "tool:" + call.ID
	}
	return "tool:" + call.Name
}

// toolCard 是对话里的一次工具调用：一行标题（工具名、状态、耗时），展开后是参数和结果。
func toolCard(c *mui.Context, tk *theme.Tokens, call *app.ToolCall) {
	card := layout.Block(c, layout.BlockProps{Variant: layout.BlockOutlined}).AlignSelf(mui.Stretch).FillWidth().Clip()
	card.Children(func() {
		p := mui.CollapsibleBase(c.Key(toolKey(call)), &call.Open)
		tr := p.Trigger.Label(call.Name).Row().FillWidth().Padding(10, 12).Gap(8).AlignItems(mui.Center).
			Cursor(mui.CursorPointer).Unselectable()
		if tr.Hovered() {
			tr.Background(tk.FillTertiary)
		}
		tr.Children(func() {
			mui.Icon(c, toolIcon(call.Name)).FontSize(14).TextColor(tk.TextSecondary)
			mui.Text(c, call.Name).FontSize(13).FontWeight(600).TextColor(tk.Text)
			toolStatus(c, call)
			mui.Spacer(c)
			if took := toolTook(call); took != "" {
				mui.Text(c, took).FontSize(11).TextColor(tk.TextTertiary)
			}
			mui.Icon(c, lucide.ChevronDown()).FontSize(14).TextColor(tk.TextTertiary).Rotate(180 * p.Progress())
		})
		p.Panel(func() {
			// 展开内容限高并在内部滚动:工具结果可能是整页 HTML,不能让卡片顶穿整条对话。
			mui.Scroll(c).MaxHeight(320).MaxWidthPercent(100).Children(func() {
				// 上下留白与左右一致：标题行 10 + 面板 2，标签距上边框正好 12。
				mui.Column(c).Padding(2, 12, 12, 12).Gap(6).Children(func() {
					mui.Text(c, "参数").FontSize(11).FontWeight(600).TextColor(tk.TextTertiary)
					code.Snippet(c, call.Args, code.SnippetProps{Language: "json", Variant: code.Filled, FontSize: 12})
					if call.State == app.ToolRunning {
						return
					}
					mui.Text(c, "结果").FontSize(11).FontWeight(600).TextColor(tk.TextTertiary).Margin(4, 0, 0, 0)
					if call.State == app.ToolFailed {
						feedback.Alert(c, "工具没有跑完", feedback.AlertProps{
							Type: feedback.Error, Variant: feedback.Plain, Description: call.Result, Selectable: true,
						})
						return
					}
					markdown.Markdown(c, call.Result, markdown.MarkdownProps{Variant: markdown.Chat, FontSize: 13})
				})
			})
		})
	})
}

func toolStatus(c *mui.Context, call *app.ToolCall) {
	if call.State == app.ToolRunning {
		// Spin 自带 AlignSelf(Start);在居中行里要改回 Center，否则会和“运行中”文字错位。
		feedback.Spin(c, feedback.SpinProps{CustomSize: 11}, nil).AlignSelf(mui.Center)
		display.Tag(c, "运行中", display.TagProps{Variant: display.TagBorderless, Color: display.TagProcessing, Size: theme.Small})
		return
	}
	if call.State == app.ToolFailed {
		display.Tag(c, "失败", display.TagProps{Variant: display.TagBorderless, Color: display.TagError, Size: theme.Small})
		return
	}
	display.Tag(c, "已完成", display.TagProps{Variant: display.TagBorderless, Color: display.TagSuccess, Size: theme.Small})
}

func toolTook(call *app.ToolCall) string {
	if call.State == app.ToolRunning {
		return ""
	}
	if call.Took <= 0 {
		return ""
	}
	if call.Took < time.Second {
		return strconv.Itoa(int(call.Took/time.Millisecond)) + "ms"
	}
	return strconv.FormatFloat(call.Took.Seconds(), 'f', 1, 64) + "s"
}
