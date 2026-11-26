package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// stateSchema 是 state.json 的版本；字段语义变化时升版本，旧文件按「没有状态」处理。
const stateSchema = 1

// statePath 定位机器人/对话的状态文件；测试里替换成临时路径。
var statePath = defaultStatePath

func defaultStatePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ada_chat", "state.json"), nil
}

// savedState 是落盘的机器人、对话与 id 计数。
//
// 进程状态（State/PID/Err）与审批队列是瞬时数据，不进文件；重启后统一按
// BotStopped 恢复，等用户发消息或点机器人时再启动进程。
type savedState struct {
	Version   int        `json:"version"`
	NextID    int        `json:"nextId"`
	ActiveID  int        `json:"activeId"`
	Workspace string     `json:"workspace,omitempty"`
	Bots      []*Bot     `json:"bots,omitempty"`
	Sessions  []*Session `json:"sessions,omitempty"`
}

// SaveState 把机器人、对话与 id 计数原子写入状态文件。
func (a *App) SaveState() error {
	path, err := statePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(savedState{
		Version: stateSchema, NextID: a.nextID, ActiveID: a.ActiveID,
		Workspace: a.Workspace, Bots: a.Bots, Sessions: a.Sessions,
	}, "", "  ")
	if err != nil {
		return err
	}
	// 覆盖前留一份上一版:测试/误操作把状态写空时,还能把机器人捞回来。
	if old, rerr := os.ReadFile(path); rerr == nil && len(old) > 0 {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SetStatePath 覆盖状态文件位置,返回恢复函数。只给宿主集成测试/工具用;生产代码不要调用。
func SetStatePath(path string) (restore func()) {
	old := statePath
	statePath = func() (string, error) { return path, nil }
	return func() { statePath = old }
}

// persist 是保存的尽力而为版本：保存失败不该打断聊天。
func (a *App) persist() { _ = a.SaveState() }

// restore 读取上次落盘的机器人、对话与 id 计数；文件缺失或损坏时保持空工作台。
func (a *App) restore() {
	path, err := statePath()
	if err != nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st savedState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateSchema {
		return
	}
	a.Bots, a.Sessions = st.Bots, st.Sessions
	a.ActiveID, a.Workspace = st.ActiveID, st.Workspace
	a.normalizeRestored()
}

// normalizeRestored 把落盘状态里的瞬时字段改成「重启后」的样子，并重建 id 计数。
func (a *App) normalizeRestored() {
	bots := make([]*Bot, 0, len(a.Bots))
	maxID := 0
	for _, b := range a.Bots {
		if b == nil {
			continue
		}
		b.State, b.PID, b.Err = BotStopped, 0, ""
		if _, err := os.Stat(b.Workspace); err != nil {
			b.State = BotError
			b.Err = "工作目录不存在: " + b.Workspace
		}
		if b.ID > maxID {
			maxID = b.ID
		}
		bots = append(bots, b)
	}
	a.Bots = bots

	sessions := make([]*Session, 0, len(a.Sessions))
	for _, s := range a.Sessions {
		if s == nil || !a.hasBot(s.BotID) {
			continue
		}
		if s.ID > maxID {
			maxID = s.ID
		}
		msgs := make([]*Message, 0, len(s.Messages))
		for _, m := range s.Messages {
			if m == nil {
				continue
			}
			if m.ID > maxID {
				maxID = m.ID
			}
			normalizeMessage(m)
			msgs = append(msgs, m)
		}
		s.Messages = msgs
		if s.Todo != nil {
			normalizeTodo(s.Todo)
			if len(s.Todo.Steps) == 0 {
				s.Todo = nil
			}
		}
		sessions = append(sessions, s)
	}
	a.Sessions = sessions
	a.nextID = maxID
	if a.nextID < 1 {
		a.nextID = 1
	}

	// 审批请求不落盘；同步副本也清空。
	a.Approvals = nil
	a.approvalMu.Lock()
	a.approvalOwner = map[string]Approval{}
	a.approvalMu.Unlock()

	if len(a.Sessions) == 0 {
		a.ActiveID = 0
		return
	}
	for _, s := range a.Sessions {
		if s.ID == a.ActiveID {
			return
		}
	}
	a.ActiveID = a.Sessions[0].ID
}

func (a *App) hasBot(id int) bool {
	for _, b := range a.Bots {
		if b.ID == id {
			return true
		}
	}
	return false
}

// normalizeMessage 把重启前还在生成的消息收尾：停止流式，跑着的工具记为中断。
func normalizeMessage(m *Message) {
	m.Streaming = false
	for _, c := range m.Tools {
		if c == nil || c.State != ToolRunning {
			continue
		}
		c.State = ToolFailed
		if strings.TrimSpace(c.Result) == "" {
			c.Result = "进程重启，工具调用中断"
		}
	}
	if m.Role == RoleAssistant && strings.TrimSpace(m.Text) == "" {
		m.Text = "（回答未完成）"
		if len(m.Parts) > 0 {
			m.Parts = append(m.Parts, Part{Kind: PartText, Text: m.Text})
		}
	}
}
