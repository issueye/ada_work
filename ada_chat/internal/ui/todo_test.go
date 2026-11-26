package ui_test

import (
	"testing"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// TestTodoPanel 守住任务面板:标题、步骤、进度都来自会话的 Todo。
func TestTodoPanel(t *testing.T) {
	a := baseApp(t)
	a.Sessions[0].Todo = &app.Todo{
		Title: "发布",
		Steps: []app.TodoStep{
			{Content: "写代码", Status: "completed", Priority: "high"},
			{Content: "跑测试", Status: "in_progress"},
			{Content: "上线", Status: "pending"},
		},
	}
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 760)
	tt.Frame()
	for _, want := range []string{"任务", "发布", "写代码", "跑测试", "上线", "1/3"} {
		if _, ok := tt.Find(want); !ok {
			t.Fatalf("任务面板缺少 %q", want)
		}
	}
	shot(t, tt, "todo-panel.png")
}
