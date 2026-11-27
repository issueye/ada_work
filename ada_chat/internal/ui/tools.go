package ui

import (
	"strconv"
	"time"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/code"
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
//
// 工具是「过程」:默认只留一行低对比度的标题,不套外框、不用卡片底色;展开后的参数/结果
// 也走无边框样式,避免每次调用都在对话里砸出一个方块。
func toolCard(c *mui.Context, tk *theme.Tokens, call *app.ToolCall) {
	p := mui.CollapsibleBase(c.Key(toolKey(call)), &call.Open)
	tr := p.Trigger.Label(call.Name).Row().FillWidth().Padding(2, 10).Gap(6).AlignItems(mui.Center).
		Cursor(mui.CursorPointer).Unselectable()
	if tr.Hovered() {
		tr.Background(tk.FillTertiary).Radius(tk.BorderRadiusSM)
	}
	tr.Children(func() {
		mui.Icon(c, toolIcon(call.Name)).FontSize(13).TextColor(tk.TextTertiary)
		mui.Text(c, call.Name).FontSize(12).FontWeight(500).TextColor(tk.TextSecondary)
		toolStatus(c, tk, call)
		mui.Spacer(c)
		if took := toolTook(call); took != "" {
			mui.Text(c, took).FontSize(11).TextColor(tk.TextTertiary)
		}
		mui.Icon(c, lucide.ChevronDown()).FontSize(13).TextColor(tk.TextTertiary).Rotate(180 * p.Progress())
	})
	p.Panel(func() {
		// 展开后的参数/结果是卡片:(收起时只有一行标题,展开才有框)。
		card := layout.Block(c, layout.BlockProps{Variant: layout.BlockOutlined}).FillWidth()
		card.Background(tk.FillQuaternary)
		card.Children(func() {
			// 内容限高并在内部滚动:工具结果可能是整页 HTML,不能让卡片顶穿整条对话。
			mui.Scroll(c).MaxHeight(320).MaxWidthPercent(100).Children(func() {
				mui.Column(c).Padding(8, 10, 10, 10).Gap(6).Children(func() {
					mui.Text(c, "参数").FontSize(11).FontWeight(600).TextColor(tk.TextTertiary)
					code.Snippet(c, call.Args, code.SnippetProps{Language: "json", Variant: code.Borderless, FontSize: 12})
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
					markdown.Markdown(c, call.Result, markdown.MarkdownProps{Variant: markdown.Chat, FontSize: 12})
				})
			})
		})
	})
}

// toolStatus 是工具状态的低对比度标签:运行中带 spinner,失败才用错误色。
func toolStatus(c *mui.Context, tk *theme.Tokens, call *app.ToolCall) {
	switch call.State {
	case app.ToolRunning:
		// Spin 自带 AlignSelf(Start);在居中行里要改回 Center,否则会和文字错位。
		feedback.Spin(c, feedback.SpinProps{CustomSize: 11}, nil).AlignSelf(mui.Center)
		mui.Text(c, "运行中").FontSize(11).TextColor(tk.TextSecondary)
	case app.ToolFailed:
		mui.Text(c, "失败").FontSize(11).TextColor(tk.Error)
	default:
		mui.Text(c, "已完成").FontSize(11).TextColor(tk.TextTertiary)
	}
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
