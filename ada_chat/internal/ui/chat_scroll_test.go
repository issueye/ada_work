package ui_test

import (
	"strings"
	"testing"

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
