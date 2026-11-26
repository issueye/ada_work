package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBotTodoUpdatesSessionAndPersists(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := &App{
		Bots: []*Bot{{ID: 1, Name: "bot", Workspace: t.TempDir()}},
		Sessions: []*Session{{
			ID: 2, BotID: 1, Title: "todo",
			Messages: []*Message{{ID: 3, Role: RoleAssistant, At: time.Now(), Streaming: true}},
		}},
	}
	a.BotTodo(1, 3, Todo{
		Title: "  发布  ",
		Steps: []TodoStep{
			{Content: "  写代码 ", Status: "completed", Priority: "high"},
			{Content: "跑测试", Status: "in_progress"},
			{Content: "  "},
			{Content: "上线", Status: "unknown", Priority: "urgent"},
		},
	})
	s := a.SessionByID(2)
	if s == nil || s.Todo == nil {
		t.Fatal("计划没有写到会话")
	}
	if s.Todo.Title != "发布" || len(s.Todo.Steps) != 3 {
		t.Fatalf("计划清洗不符: %+v", s.Todo)
	}
	if s.Todo.Steps[0].Content != "写代码" || s.Todo.Steps[0].Status != "completed" {
		t.Fatalf("第一步不符: %+v", s.Todo.Steps[0])
	}
	if s.Todo.Steps[1].Status != "in_progress" || s.Todo.Steps[1].Priority != "medium" {
		t.Fatalf("第二步默认值不符: %+v", s.Todo.Steps[1])
	}
	if s.Todo.Steps[2].Status != "pending" || s.Todo.Steps[2].Priority != "medium" {
		t.Fatalf("非法状态应回落: %+v", s.Todo.Steps[2])
	}
	if done, total := s.Todo.Counts(); done != 1 || total != 3 {
		t.Fatalf("进度不符: %d/%d", done, total)
	}

	// 重新从磁盘加载后任务仍在。
	a2 := &App{}
	a2.restore()
	if a2.SessionByID(2) == nil || a2.SessionByID(2).Todo == nil {
		t.Fatalf("重启后计划丢失: %+v", a2.Sessions)
	}
	if done, total := a2.SessionByID(2).Todo.Counts(); done != 1 || total != 3 {
		t.Fatalf("重启后进度不符: %d/%d", done, total)
	}

	// 空计划表示清除面板。
	a.BotTodo(1, 3, Todo{})
	if a.SessionByID(2).Todo != nil {
		t.Fatal("空计划应清除会话任务")
	}
}

func TestNormalizeTodoCapsSteps(t *testing.T) {
	todo := Todo{Steps: make([]TodoStep, 0, 80)}
	for i := 0; i < 80; i++ {
		todo.Steps = append(todo.Steps, TodoStep{Content: "step"})
	}
	normalizeTodo(&todo)
	if len(todo.Steps) != 50 {
		t.Fatalf("最多保留 50 步,实际 %d", len(todo.Steps))
	}
}
