package ui_test

import (
	"strings"
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// TestToolCardScrollsInternally 守住工具卡片的展开高度:内容再长也限制在
// 320 DIP 内,超出部分在卡片内部滚动(截图人工核对,测试保证能渲染出来)。
func TestToolCardScrollsInternally(t *testing.T) {
	a := baseApp(t)
	call := &app.ToolCall{
		ID: "call-1", Name: "http_request", Open: true, State: app.ToolDone,
		Args:   `{"url":"https://github.com/trending/go"}`,
		Result: strings.Repeat("很长的结果行，需要在卡片内部滚动。\n", 400),
	}
	a.Sessions[0].Messages = []*app.Message{{
		ID: 1, Role: app.RoleAssistant, Streaming: false,
		Parts: []app.Part{
			{Kind: app.PartReasoning, Text: "先想一想", Open: false},
			{Kind: app.PartTool, ToolID: call.ID},
		},
		Tools: []*app.ToolCall{call},
	}}

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1000, 820)
	tt.Frame()
	if _, ok := tt.Find("http_request"); !ok {
		t.Fatalf("工具卡片应渲染出来")
	}
	// 展开内容的第一行仍应可见(滚动容器里)。
	if _, ok := tt.Find("参数"); !ok {
		t.Fatalf("工具卡片展开后应显示参数区")
	}
	shot(t, tt, "tool-card-scroll.png")
}

// TestScrollToEndButton 守住「回到底部」浮动按钮:列表在末尾时隐藏,滚离末尾后出现,
// 点击后回到末尾并隐藏。
func TestScrollToEndButton(t *testing.T) {
	a := baseApp(t)
	now := time.Now()
	var msgs []*app.Message
	for i := 0; i < 14; i++ {
		msgs = append(msgs, &app.Message{
			ID: i + 1, Role: app.RoleAssistant, At: now,
			Parts: []app.Part{{Kind: app.PartText, Text: strings.Repeat("这是一段把对话撑高的正文。\n\n", 4)}},
		})
	}
	a.Sessions[0].Messages = msgs

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1000, 820)
	tt.Frame()
	if _, ok := tt.Find("回到底部"); ok {
		t.Fatalf("列表停在末尾时不应显示回到底部按钮")
	}

	// 向上滚一段(负 dy 看更早的消息),按钮应出现。
	tt.Scroll(500, 300, 0, -600)
	tt.Frame()
	rect, ok := tt.Find("回到底部")
	if !ok {
		t.Fatalf("滚离末尾后应出现回到底部按钮")
	}
	shot(t, tt, "chat-scroll-button.png")

	// 点它:回到末尾,按钮消失。
	tt.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tt.Frame()
	tt.Advance(300 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("回到底部"); ok {
		t.Fatalf("点击回到底部后按钮应隐藏")
	}
	shot(t, tt, "chat-scroll-bottom.png")
}

// TestTurnDoneCollapsesDetails 一轮结束后的视觉结果:工具参数/结果收起,
// 思考折成标题行,只有报告(正文)保持展开。
func TestTurnDoneCollapsesDetails(t *testing.T) {
	a := baseApp(t)
	call := &app.ToolCall{
		ID: "c1", Name: "run_shell", Open: true, State: app.ToolDone,
		Args: `{"program":"git"}`, Result: "ok",
	}
	now := time.Now()
	m := &app.Message{
		ID: 1, Role: app.RoleAssistant, At: now, Streaming: true, Text: "报告：已经完成",
		Parts: []app.Part{
			{Kind: app.PartReasoning, Text: "先想一想", Open: true},
			{Kind: app.PartTool, ToolID: call.ID},
			{Kind: app.PartText, Text: "报告：已经完成"},
		},
		Tools: []*app.ToolCall{call},
	}
	a.Sessions[0].BotID = 1
	a.Sessions[0].Messages = []*app.Message{m}
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 900, 760)
	tt.Frame()
	if _, ok := tt.Find("参数"); !ok {
		t.Fatalf("运行中展开的工具卡片应显示参数")
	}
	a.BotTurnDone(1, m.ID, "报告：已经完成")
	tt.Frame()
	// 折叠有动画:等它收完再看。
	for i := 0; i < 10 && tt.FramePending(); i++ {
		tt.Advance(120 * time.Millisecond)
		tt.Frame()
	}
	if _, ok := tt.Find("参数"); ok {
		t.Fatalf("一轮结束后工具参数应折叠")
	}
	if _, ok := tt.Find("报告：已经完成"); !ok {
		t.Fatalf("报告正文应保持可见")
	}
	shot(t, tt, "chat-turn-done-collapsed.png")
}
