package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// TestStreamingMessageShowsLoading 守住对话尾部:正文还在流入时,消息里有
// 一个持续的 loading(spinner + 生成中),结束后动画必须停。
func TestStreamingMessageShowsLoading(t *testing.T) {
	now := time.Now()
	a := &app.App{ActiveID: 1}
	a.Sessions = []*app.Session{{
		ID: 1, Title: "你是谁", Updated: now,
		Messages: []*app.Message{{
			ID: 1, Role: app.RoleAssistant, At: now, Streaming: true,
			Parts: []app.Part{{Kind: app.PartText, Text: "我正在整理最新动态,"}},
		}},
	}}

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 700, 760)
	tt.Frame()
	if !tt.FramePending() {
		t.Fatalf("正文流入时应有持续请求帧的 loading")
	}
	first := copyRGBA(tt.Image())
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if equalRGBA(first, copyRGBA(tt.Image())) {
		t.Fatalf("loading 没有动:两帧像素相同")
	}
	shot(t, tt, "chat-streaming.png")

	// 一轮结束:不再持续请求帧。
	a.Sessions[0].Messages[0].Streaming = false
	for i := 0; i < 8 && tt.FramePending(); i++ {
		tt.Advance(200 * time.Millisecond)
		tt.Frame()
	}
	if tt.FramePending() {
		t.Fatalf("一轮结束后不应再持续请求帧")
	}
	shot(t, tt, "chat-done.png")
}

// TestReasoningTailKeepsSpinner 思考块在流入时靠思考卡片自己的 spinner;
// 动画仍在动,但正文尾部不会再多一个。
func TestReasoningTailKeepsSpinner(t *testing.T) {
	now := time.Now()
	a := &app.App{ActiveID: 1}
	a.Sessions = []*app.Session{{
		ID: 1, Title: "你是谁", Updated: now,
		Messages: []*app.Message{{
			ID: 1, Role: app.RoleAssistant, At: now, Streaming: true,
			Parts: []app.Part{{Kind: app.PartReasoning, Text: "先想一下…", Open: true}},
		}},
	}}
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 700, 760)
	tt.Frame()
	if !tt.FramePending() {
		t.Fatalf("思考流式进行时应有动画")
	}
	// 展开的思考正文是卡片:等折叠/逐字动画走完再截图人工核对。
	for i := 0; i < 12; i++ {
		tt.Advance(200 * time.Millisecond)
		tt.Frame()
	}
	shot(t, tt, "chat-reasoning.png")
}
