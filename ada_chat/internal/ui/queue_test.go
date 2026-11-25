package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// TestComposerShowsQueue 守住输入框上方的「待发送」列表与单条移除。
func TestComposerShowsQueue(t *testing.T) {
	a := baseApp(t)
	a.Sessions[0].Messages = []*app.Message{{ID: 1, Role: app.RoleAssistant, Streaming: true}}
	a.Sessions[0].Queue = []string{"下一条消息", "再下一条"}

	u := ui.New(a)
	tt := mui.NewTester(u.Root, 1000, 820)
	tt.Frame()

	if _, ok := tt.Find("待发送 2 条"); !ok {
		t.Fatalf("输入框上方应显示待发送数量")
	}
	if _, ok := tt.Find("下一条消息"); !ok {
		t.Fatalf("应显示排队消息内容")
	}
	shot(t, tt, "composer-queue.png")

	if err := tt.Click("移除排队消息"); err != nil {
		t.Fatalf("移除排队消息失败: %v", err)
	}
	tt.Advance(100 * time.Millisecond)
	tt.Frame()
	if len(a.Sessions[0].Queue) != 1 || a.Sessions[0].Queue[0] != "再下一条" {
		t.Fatalf("移除后队列应只剩一条: %+v", a.Sessions[0].Queue)
	}
	if _, ok := tt.Find("待发送 1 条"); !ok {
		t.Fatalf("移除后数量应更新")
	}
}
