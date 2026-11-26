package ui

import (
	"strconv"

	mui "github.com/egoist/mygo/ui"
	"github.com/lobehub/lobe-mygo/pkg/display"
	"github.com/lobehub/lobe-mygo/pkg/layout"
	"github.com/lobehub/lobe-mygo/pkg/lucide"
	"github.com/lobehub/lobe-mygo/pkg/theme"

	"ada_chat/internal/app"
)

// todoPanel 显示 agent 当前的任务计划。它固定在对话头部下方,滚动消息时也一直可见;
// 默认展开,用户折叠后按会话记住状态。
func (u *UI) todoPanel(c *mui.Context, tk *theme.Tokens) {
	s := u.App.Current()
	if s == nil || s.Todo == nil || len(s.Todo.Steps) == 0 {
		return
	}
	todo := s.Todo
	done, total := todo.Counts()
	mui.Column(c).Padding(8, 16, 0, 16).Children(func() {
		card := layout.Block(c, layout.BlockProps{Variant: layout.BlockOutlined}).AlignSelf(mui.Stretch).FillWidth().Clip()
		card.Background(tk.FillQuaternary)
		card.Children(func() {
			p := mui.CollapsibleBase(c.Key("todo:"+strconv.Itoa(s.ID)), u.todoOpenState(s.ID))
			tr := p.Trigger.Label("任务").Row().FillWidth().Padding(8, 12).Gap(8).AlignItems(mui.Center).
				Cursor(mui.CursorPointer).Unselectable()
			if tr.Hovered() {
				tr.Background(tk.FillTertiary)
			}
			tr.Children(func() {
				mui.Icon(c, lucide.ListTodo()).FontSize(14).TextColor(tk.TextSecondary)
				mui.Text(c, "任务").FontSize(12).FontWeight(600).TextColor(tk.TextSecondary)
				if todo.Title != "" {
					mui.Text(c, todo.Title).FontSize(12).TextColor(tk.TextTertiary).
						SingleLine().MaxWidth(420)
				}
				mui.Spacer(c)
				color := display.TagGray
				switch {
				case total > 0 && done == total:
					color = display.TagSuccess
				case hasInProgress(todo):
					color = display.TagProcessing
				}
				display.Tag(c, strconv.Itoa(done)+"/"+strconv.Itoa(total), display.TagProps{
					Variant: display.TagBorderless, Color: color, Size: theme.Small,
				})
				mui.Icon(c, lucide.ChevronDown()).FontSize(14).TextColor(tk.TextTertiary).Rotate(180 * p.Progress())
			})
			p.Panel(func() {
				mui.Scroll(c).MaxHeight(240).MaxWidthPercent(100).Children(func() {
					mui.Column(c).Padding(2, 12, 10, 12).Gap(7).Children(func() {
						for _, step := range todo.Steps {
							todoRow(c, tk, step)
						}
					})
				})
			})
		})
	})
}

func (u *UI) todoOpenState(sessionID int) *bool {
	if u.todoOpen == nil {
		u.todoOpen = map[int]*bool{}
	}
	if p, ok := u.todoOpen[sessionID]; ok {
		return p
	}
	open := true
	u.todoOpen[sessionID] = &open
	return &open
}

func hasInProgress(todo *app.Todo) bool {
	for _, step := range todo.Steps {
		if step.Status == "in_progress" {
			return true
		}
	}
	return false
}

func todoRow(c *mui.Context, tk *theme.Tokens, step app.TodoStep) {
	icon := lucide.Circle()
	color := tk.TextTertiary
	contentColor := tk.TextSecondary
	switch step.Status {
	case "completed":
		icon = lucide.CheckCircle2()
		color = tk.Success
		contentColor = tk.TextTertiary
	case "in_progress":
		icon = lucide.CircleDot()
		color = tk.Primary
		contentColor = tk.Text
	}
	mui.Row(c).Gap(8).AlignItems(mui.Start).Children(func() {
		mui.Icon(c, icon).FontSize(14).TextColor(color).Margin(1, 0, 0, 0)
		mui.Text(c, step.Content).FontSize(12).LineHeight(1.45).TextColor(contentColor).
			Grow(1).Basis(0).MinWidth(0)
		if step.Priority == "high" {
			display.Tag(c, "高", display.TagProps{
				Variant: display.TagBorderless, Color: display.TagGold, Size: theme.Small,
			})
		}
	})
}
