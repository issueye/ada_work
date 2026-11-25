package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withStatePath 把状态文件换到临时路径；返回恢复函数。
func withStatePath(t *testing.T, path string) {
	t.Helper()
	old := statePath
	statePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { statePath = old })
}

func TestSaveAndRestoreState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)
	workspace := filepath.Join(dir, "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	app := &App{
		Bots: []*Bot{{
			ID: 3, Name: "小张", Description: "测试机器人", Instruction: "先读文件",
			Model: "replay", Workspace: workspace, Icon: "bot", Color: "#3366ff",
		}},
		Sessions: []*Session{{
			ID: 4, BotID: 3, Title: "你好", Preview: "读 a.txt",
			Messages: []*Message{
				{ID: 5, Role: RoleUser, Text: "读 a.txt", At: time.Now()},
				{ID: 6, Role: RoleAssistant, At: time.Now(), Streaming: true,
					Tools: []*ToolCall{{ID: "c1", Name: "read_file", State: ToolRunning}},
					Parts: []Part{{Kind: PartTool, ToolID: "c1"}}},
			},
		}},
		ActiveID: 4, Workspace: workspace, nextID: 7,
	}
	if err := app.SaveState(); err != nil {
		t.Fatalf("save: %v", err)
	}

	b := &App{}
	b.restore()
	if len(b.Bots) != 1 || len(b.Sessions) != 1 {
		t.Fatalf("恢复结果 = bots=%d sessions=%d", len(b.Bots), len(b.Sessions))
	}
	if got := b.Bots[0]; got.Name != "小张" || got.Workspace != workspace ||
		got.State != BotStopped || got.PID != 0 {
		t.Errorf("机器人恢复不对: %+v", got)
	}
	if b.ActiveID != 4 || b.Workspace != workspace {
		t.Errorf("activeId/workspace 恢复不对: %d %q", b.ActiveID, b.Workspace)
	}
	if b.nextID != 6 {
		t.Errorf("nextID 应回填为最大 id 6, got %d", b.nextID)
	}
	if got := b.id(); got != 7 {
		t.Errorf("下一条 id = %d, want 7", got)
	}
	m := b.Sessions[0].Messages[1]
	if m.Streaming {
		t.Error("流式消息恢复后应结束")
	}
	if len(m.Tools) != 1 || m.Tools[0].State != ToolFailed {
		t.Errorf("运行中的工具应记为中断: %+v", m.Tools)
	}
	if m.Text == "" {
		t.Error("中断的助手消息应有占位文本")
	}
}

func TestRestoreIgnoresCorruptState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	a.restore()
	if len(a.Bots) != 0 || len(a.Sessions) != 0 || a.nextID != 0 {
		t.Fatalf("坏文件不应恢复出状态: %+v", a)
	}
}

func TestNewBotSessionIsPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)

	app := &App{
		Bots: []*Bot{{ID: 1, Name: "小张", Workspace: dir}},
		Sessions: []*Session{
			{
				ID: 2, BotID: 1, Title: "旧对话", Messages: []*Message{
					{ID: 3, Role: RoleUser, Text: "你好"},
				},
			},
		},
		ActiveID: 2, nextID: 3,
	}
	s, err := app.NewBotSession(1)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if s.ID != 4 || s.BotID != 1 || app.ActiveID != 4 {
		t.Fatalf("新会话不对: %+v active=%d", s, app.ActiveID)
	}
	if len(app.Sessions) != 2 || app.Sessions[0].ID != 4 {
		t.Fatalf("新会话应排在最前: %+v", app.Sessions)
	}

	b := &App{}
	b.restore()
	if len(b.Sessions) != 2 || b.Sessions[0].ID != 4 || b.ActiveID != 4 {
		t.Fatalf("重启后新会话应仍在: %+v active=%d", b.Sessions, b.ActiveID)
	}
}

func TestRestoreMarksMissingWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)

	a := &App{Bots: []*Bot{{ID: 1, Name: "小张", Workspace: filepath.Join(dir, "gone")}}, nextID: 2}
	if err := a.SaveState(); err != nil {
		t.Fatal(err)
	}
	b := &App{}
	b.restore()
	if len(b.Bots) != 1 {
		t.Fatalf("机器人应仍在: %+v", b.Bots)
	}
	if b.Bots[0].State != BotError || b.Bots[0].Err == "" {
		t.Errorf("缺失工作目录应标记为错误: state=%v err=%q", b.Bots[0].State, b.Bots[0].Err)
	}
}
