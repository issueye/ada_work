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

func (l *lockedApp) todo(sessionID int) *app.Todo {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.a.SessionByID(sessionID)
	if s == nil || s.Todo == nil {
		return nil
	}
	cp := *s.Todo
	cp.Steps = append([]app.TodoStep(nil), s.Todo.Steps...)
	return &cp
}

// TestACPInterop 用真实 ada acp 进程跑一轮,验证 supervisor 的 ACP 分支。
// 未配置环境变量时跳过:
//
//	ADA_ACP_INTEROP_AGENT  ada(.exe) 路径
//	ADA_ACP_INTEROP_PRESET examples/acp/acp.preset.json 的绝对路径
//
// interopPreset 用给定 preset 克隆出一份临时 preset:插件目录指向统一目录
// (preset 所在模块根的 plugins/),回放脚本按绝对路径写死,再叠 extras。
//
// preset 位于 examples/<示例>/ 下时,模块根是它的上两级;发布包里则是 presets/ 的上一级。
func interopPreset(t *testing.T, presetPath string, extraEnable []string, extraPatch map[string]any) string {
	t.Helper()
	base := filepath.Dir(presetPath)
	root := filepath.Clean(filepath.Join(base, "..", ".."))
	raw, err := os.ReadFile(presetPath)
	if err != nil {
		t.Fatalf("读取 preset 失败: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 preset 失败: %v", err)
	}
	doc["plugins"] = []string{filepath.Join(root, "plugins")}
	if len(extraEnable) > 0 {
		enable, _ := doc["enable"].([]any)
		for _, id := range extraEnable {
			enable = append(enable, id)
		}
		doc["enable"] = enable
	}
	patch, _ := doc["patch"].(map[string]any)
	if patch == nil {
		patch = map[string]any{}
	}
	patch["llm-replay"] = map[string]any{"path": filepath.Join(base, "replay.jsonl")}
	patch["agent-loop"] = map[string]any{"model": "replay"}
	for k, v := range extraPatch {
		patch[k] = v
	}
	doc["patch"] = patch
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "preset.json")
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return tmp
}
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
	// 统一插件目录后,示例 preset 自己就带相对模块根的 plugins 路径;这里仍克隆一份临时
	// preset,把插件目录与回放路径写成绝对路径——ACP 进程的 cwd 是机器人工作目录,
	// 临时 preset 又不在模块里,相对路径会失效。
	tmpPreset := interopPreset(t, preset, nil, nil)
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
				todo := l.todo(sessionID)
				if todo == nil || len(todo.Steps) != 2 {
					t.Fatalf("ACP plan 事件没有映射成任务列表: %+v", todo)
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
	// 在统一插件目录上追加 approval-gate:让 tool_find 走 ACP 权限请求。
	tmpPreset := interopPreset(t, preset, []string{"approval-gate"}, map[string]any{
		"approval-gate": map[string]any{"tools": []string{"tool_find"}, "backend": "acp-client", "timeoutMs": 20000},
	})
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
