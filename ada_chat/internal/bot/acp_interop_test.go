package bot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ada_chat/internal/app"
)

// lockedApp 把 app 的异步 dispatch 串行化,给测试提供一致的读快照。
// 真实窗口里 SetDispatch 会把修改送到 UI 线程;这里用一条队列 + 互斥锁模拟,
// 且 apply 只入队不内联,避免「持锁调用 App 方法 → 内部 dispatch 再拿锁」死锁。
type lockedApp struct {
	mu sync.Mutex
	a  *app.App
	ch chan func()
}

func newLockedApp(a *app.App) *lockedApp {
	l := &lockedApp{a: a, ch: make(chan func(), 256)}
	go l.loop()
	return l
}

func (l *lockedApp) loop() {
	for fn := range l.ch {
		l.mu.Lock()
		fn()
		l.mu.Unlock()
	}
}

func (l *lockedApp) dispatch(fn func()) { l.ch <- fn }

func (l *lockedApp) run(fn func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fn()
}

func (l *lockedApp) bot(id int) app.Bot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.a.BotByID(id); b != nil {
		return *b
	}
	return app.Bot{}
}

func (l *lockedApp) lastMessage(sessionID int) (text string, streaming bool, tools int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.a.SessionByID(sessionID)
	if s == nil || len(s.Messages) == 0 {
		return "", false, 0, false
	}
	m := s.Messages[len(s.Messages)-1]
	return m.Text, m.Streaming, len(m.Tools), true
}

func (l *lockedApp) approvals() []app.Approval {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]app.Approval(nil), l.a.Approvals...)
}

// TestACPInterop 用真实 ada acp 进程跑一轮,验证 supervisor 的 ACP 分支。
// 未配置环境变量时跳过:
//
//	ADA_ACP_INTEROP_AGENT  ada(.exe) 路径
//	ADA_ACP_INTEROP_PRESET examples/acp/acp.preset.json 的绝对路径
func TestACPInterop(t *testing.T) {
	agent := strings.TrimSpace(os.Getenv("ADA_ACP_INTEROP_AGENT"))
	preset := strings.TrimSpace(os.Getenv("ADA_ACP_INTEROP_PRESET"))
	if agent == "" || preset == "" {
		t.Skip("未配置 ADA_ACP_INTEROP_AGENT / ADA_ACP_INTEROP_PRESET")
	}
	if _, err := os.Stat(agent); err != nil {
		t.Fatalf("ada 不存在: %v", err)
	}
	if _, err := os.Stat(preset); err != nil {
		t.Fatalf("preset 不存在: %v", err)
	}
	workspace := t.TempDir()
	// 示例 preset 里的 replay 路径相对进程 cwd;ACP 进程的 cwd 必须是机器人工
	// 作目录,所以测试用绝对路径生成一份临时 preset。
	base := filepath.Dir(preset)
	tmpPreset := filepath.Join(t.TempDir(), "preset.json")
	body, err := json.Marshal(map[string]any{
		"name":    "acp-interop",
		"plugins": []string{filepath.Join(base, "plugins")},
		"patch": map[string]any{
			"llm-replay": map[string]any{"path": filepath.Join(base, "replay.jsonl")},
			"agent-loop": map[string]any{"model": "replay"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPreset, body, 0o600); err != nil {
		t.Fatal(err)
	}
	restoreState := app.SetStatePath(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(restoreState)
	state := &app.App{Prefs: app.Prefs{
		AgentKind: app.AgentACP, RuntimePath: agent, AgentPreset: tmpPreset,
	}}
	l := newLockedApp(state)
	state.SetDispatch(l.dispatch)
	sup, err := NewSupervisor(state)
	if err != nil {
		t.Fatal(err)
	}
	state.SetEngine(sup)
	t.Cleanup(func() { _ = sup.Close() })

	var bot *app.Bot
	l.run(func() {
		var addErr error
		bot, addErr = state.AddBot(app.Bot{Name: "acp", Workspace: workspace})
		err = addErr
	})
	if err != nil {
		t.Fatalf("添加机器人失败: %v", err)
	}
	botID := bot.ID
	deadline := time.Now().Add(20 * time.Second)
	for {
		cur := l.bot(botID)
		if cur.ID != 0 && cur.State == app.BotError {
			t.Fatalf("ACP 进程启动失败: %s", cur.Err)
		}
		if cur.ID != 0 && cur.State == app.BotRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 ACP 进程就绪超时")
		}
		time.Sleep(25 * time.Millisecond)
	}

	var sessionID int
	l.run(func() {
		sess, serr := state.NewBotSession(botID)
		if serr != nil {
			err = serr
			return
		}
		sessionID = sess.ID
		state.SelectSession(sess.ID)
		state.Draft = "hello"
		state.Send()
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline = time.Now().Add(20 * time.Second)
	for {
		text, streaming, tools, ok := l.lastMessage(sessionID)
		if ok && !streaming {
			if strings.Contains(text, "ACP agent") {
				if tools == 0 {
					t.Fatal("ACP 工具事件没有映射成工具卡片")
				}
				return
			}
			t.Fatalf("ACP 回答不符: %q", text)
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 ACP 回答超时")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestACPInteropPermission 用 approval-gate 让 tool_find 触发 ACP 权限请求,
// 宿主侧自动允许一次,验证反向 session/request_permission 链路。
func TestACPInteropPermission(t *testing.T) {
	agent := strings.TrimSpace(os.Getenv("ADA_ACP_INTEROP_AGENT"))
	preset := strings.TrimSpace(os.Getenv("ADA_ACP_INTEROP_PRESET"))
	if agent == "" || preset == "" {
		t.Skip("未配置 ADA_ACP_INTEROP_AGENT / ADA_ACP_INTEROP_PRESET")
	}
	workspace := t.TempDir()
	base := filepath.Dir(preset)
	tmpPreset := filepath.Join(t.TempDir(), "preset.json")
	body, err := json.Marshal(map[string]any{
		"name": "acp-interop-permission",
		"plugins": []string{
			filepath.Join(base, "plugins"),
			filepath.Join(base, "..", "agent", "plugins", "approval-gate"),
		},
		"patch": map[string]any{
			"llm-replay":    map[string]any{"path": filepath.Join(base, "replay.jsonl")},
			"agent-loop":    map[string]any{"model": "replay"},
			"approval-gate": map[string]any{"tools": []string{"tool_find"}, "backend": "acp-client", "timeoutMs": 20000},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPreset, body, 0o600); err != nil {
		t.Fatal(err)
	}
	restoreState := app.SetStatePath(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(restoreState)
	state := &app.App{Prefs: app.Prefs{
		AgentKind: app.AgentACP, RuntimePath: agent, AgentPreset: tmpPreset,
	}}
	l := newLockedApp(state)
	state.SetDispatch(l.dispatch)
	sup, err := NewSupervisor(state)
	if err != nil {
		t.Fatal(err)
	}
	state.SetEngine(sup)
	t.Cleanup(func() { _ = sup.Close() })

	var bot *app.Bot
	l.run(func() {
		var addErr error
		bot, addErr = state.AddBot(app.Bot{Name: "acp-perm", Workspace: workspace})
		err = addErr
	})
	if err != nil {
		t.Fatalf("添加机器人失败: %v", err)
	}
	botID := bot.ID
	deadline := time.Now().Add(20 * time.Second)
	for {
		cur := l.bot(botID)
		if cur.ID != 0 && cur.State == app.BotError {
			t.Fatalf("ACP 进程启动失败: %s", cur.Err)
		}
		if cur.ID != 0 && cur.State == app.BotRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待 ACP 进程就绪超时")
		}
		time.Sleep(25 * time.Millisecond)
	}
	var sessionID int
	l.run(func() {
		sess, serr := state.NewBotSession(botID)
		if serr != nil {
			err = serr
			return
		}
		sessionID = sess.ID
		state.SelectSession(sess.ID)
		state.Draft = "hello"
		state.Send()
	})
	if err != nil {
		t.Fatal(err)
	}

	approved := false
	deadline = time.Now().Add(20 * time.Second)
	for {
		if !approved {
			aps := l.approvals()
			if len(aps) > 0 {
				if err := state.DecideApproval(aps[0].ID, true, "测试允许"); err != nil {
					t.Fatalf("审批失败: %v", err)
				}
				approved = true
			}
		}
		text, streaming, _, ok := l.lastMessage(sessionID)
		if ok && !streaming {
			if !approved {
				t.Fatal("回答结束但从未收到 ACP 审批请求")
			}
			if !strings.Contains(text, "ACP agent") {
				t.Fatalf("ACP 回答不符: %q", text)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 ACP 审批/回答超时, approved=%v approvals=%d", approved, len(l.approvals()))
		}
		time.Sleep(25 * time.Millisecond)
	}
}
