package app

import (
	"strings"
	"time"
)

// Todo 是会话级任务列表,对应 agent 的计划(ada_go 的 plan 事件或 ACP 的 plan 更新)。
//
// 它不是内存里的临时视图:随 Session 写进 state.json,重启后仍能看到上一轮的计划;
// 内容由 agent 产生,宿主只负责展示,不把计划当成新的系统提示。
type Todo struct {
	Title   string     `json:"title,omitempty"`
	Steps   []TodoStep `json:"steps,omitempty"`
	Updated time.Time  `json:"updated,omitempty"`
}

// TodoStep 是任务列表中的一步;状态值与 ada_go/ACP 对齐。
type TodoStep struct {
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"`   // pending | in_progress | completed
	Priority string `json:"priority,omitempty"` // high | medium | low
}

// Counts 返回已完成数与总数,供界面显示 2/5 这类进度。
func (t *Todo) Counts() (done, total int) {
	if t == nil {
		return 0, 0
	}
	for _, step := range t.Steps {
		if step.Status == "completed" {
			done++
		}
	}
	return done, len(t.Steps)
}

// BotTodo 更新某一轮所属会话的任务列表。
//
// 事件只带 turn,不带 session id:turn 在 App 里已经记录了机器人与会话,
// 这与工具卡片/流式文本使用同一套路由。
func (a *App) BotTodo(botID, turn int, todo Todo) {
	a.apply(func() {
		_, s := a.botTurn(botID, turn)
		if s == nil {
			return
		}
		normalizeTodo(&todo)
		if len(todo.Steps) == 0 {
			s.Todo = nil
		} else {
			todo.Updated = a.now()
			s.Todo = &todo
		}
		a.persist()
	})
}

// normalizeTodo 清洗 agent 输入:去空行、补默认值,并限制最多 50 步,
// 避免一个异常 agent 把整个对话区撑爆。
func normalizeTodo(t *Todo) {
	if t == nil {
		return
	}
	t.Title = strings.TrimSpace(t.Title)
	if len([]rune(t.Title)) > 100 {
		t.Title = string([]rune(t.Title)[:100])
	}
	out := make([]TodoStep, 0, len(t.Steps))
	for _, step := range t.Steps {
		content := strings.TrimSpace(step.Content)
		if content == "" {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(step.Status))
		switch status {
		case "pending", "in_progress", "completed":
		default:
			status = "pending"
		}
		priority := strings.ToLower(strings.TrimSpace(step.Priority))
		switch priority {
		case "high", "medium", "low":
		default:
			priority = "medium"
		}
		if len([]rune(content)) > 200 {
			content = string([]rune(content)[:200])
		}
		out = append(out, TodoStep{Content: content, Status: status, Priority: priority})
		if len(out) >= 50 {
			break
		}
	}
	t.Steps = out
}
