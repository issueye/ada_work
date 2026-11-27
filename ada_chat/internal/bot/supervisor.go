// Package bot 管理机器人进程:一个工作目录一个 ACP agent 进程,pty 外的
// 事件都经 ACP(stdio 上的 JSON-RPC 2.0)回到这里,再由这里写成会话消息。
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ada_chat/internal/acp"
	"ada_chat/internal/app"
)

// 一轮回答的时长策略:真正判死的是「多久没有新进展」,不是总时长。
// 固定 3 分钟会把工具多的长任务误杀 —— 用户看到的 context deadline exceeded
// 就是这样来的;重试也只会再撞一次同一堵墙。
const (
	turnIdleTimeout = 2 * time.Minute
	turnMaxTimeout  = 30 * time.Minute
)

// Supervisor 按工作目录管理进程实例:同目录的多个机器人共享一个 ACP 进程与
// 一条连接,也不会因此启动第二个进程。
type Supervisor struct {
	app  *app.App
	args []string

	mu        sync.Mutex
	instances map[string]*instance
	nextID    int
	closing   atomic.Bool
	// cancels 保存每一轮正在进行的调用；canceled 标记由用户主动终止的轮次。
	cancels  map[int]context.CancelFunc
	canceled map[int]bool
}

type instance struct {
	id int
	// key 是实例键(目录 + 环境指纹);workspace 只用于展示与 cwd。
	key       string
	workspace string
	cmd       *exec.Cmd
	log       string
	state     app.BotState
	pid       int
	err       string

	acp           *acp.Client
	acpReady      chan struct{}
	acpErr        string
	acpCreateMu   sync.Mutex
	acpSessions   map[int]string
	acpBindings   map[string]acpBinding
	acpTurns      map[int]string
	acpApprovals  map[string]chan bool
	acpPermission int

	// lastActivity 是最近一次收到进展的时间(会话更新/工具事件/审批请求),
	// idleHit 记这一轮是不是被「长时间没有新进展」掐断的。
	lastActivity atomic.Int64
	idleHit      atomic.Bool

	bots     map[int]struct{}
	stopping atomic.Bool
}

// touch 记录一次进展(会话更新、工具事件、审批请求都算)。
func (r *instance) touch() { r.lastActivity.Store(time.Now().UnixNano()) }

// idleFor 返回距离最近一次进展的时长;还没打过点时按 0 处理。
func (r *instance) idleFor() time.Duration {
	last := r.lastActivity.Load()
	if last == 0 {
		return 0
	}
	return time.Since(time.Unix(0, last))
}

// watchIdle 是「长时间没有新进展」的看门狗:超过 idle 没有事件时,先让 agent
// 停下这一轮(session/cancel),再取消本地等待。总时长仍由 maxCtx 兜底。
func (s *Supervisor) watchIdle(r *instance, turn int, idle time.Duration, maxCtx context.Context, cancel context.CancelFunc, stop <-chan struct{}) {
	tick := time.Second
	if idle > 0 && idle < tick {
		tick = idle
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-maxCtx.Done():
			return
		case <-t.C:
			if r.idleFor() <= idle {
				continue
			}
			r.idleHit.Store(true)
			s.mu.Lock()
			sid := r.acpTurns[turn]
			s.mu.Unlock()
			if sid != "" && r.acp != nil {
				cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = r.acp.Cancel(cctx, sid)
				ccancel()
			}
			cancel()
			return
		}
	}
}

// acpBinding 把 agent 侧 sessionId 关联到发起这一轮的会话。
type acpBinding struct {
	BotID int
	Turn  int
}

// acpOptionInfo 是 session/request_permission 里的一个选项。
type acpOptionInfo struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// NewSupervisor 用宿主设置构造进程管理(agent 路径缺省回落 ADA_BOT_RUNTIME)。
func NewSupervisor(a *app.App) (*Supervisor, error) {
	args, err := runtimeArgs(os.Getenv("ADA_BOT_RUNTIME_ARGS"))
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		app: a, args: args,
		instances: map[string]*instance{},
		cancels:   map[int]context.CancelFunc{},
		canceled:  map[int]bool{},
	}, nil
}

// runtimeArgs 把 ADA_BOT_RUNTIME_ARGS 解析成命令行参数数组(JSON 字符串数组)。
func runtimeArgs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("ADA_BOT_RUNTIME_ARGS 必须是 JSON 字符串数组: %w", err)
	}
	return args, nil
}

var _ app.BotEngine = (*Supervisor)(nil)

// InstanceInfo is a read-only view of one workspace process, for diagnostics
// and a future process panel.
type InstanceInfo struct {
	Workspace string
	PID       int
	Bots      int
	State     app.BotState
	Err       string
}

// Instances returns the live workspace instances.
func (s *Supervisor) Instances() []InstanceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InstanceInfo, 0, len(s.instances))
	for _, r := range s.instances {
		out = append(out, InstanceInfo{
			Workspace: r.workspace, PID: r.pid, Bots: len(r.bots),
			State: r.state, Err: r.err,
		})
	}
	return out
}

func workspaceKey(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// Start 确保机器人的工作区有一个进程,并把它加为成员。
func (s *Supervisor) Start(b *app.Bot) error {
	if b == nil {
		return errors.New("机器人不存在")
	}
	runtimePath := strings.TrimSpace(s.app.Prefs.RuntimePath)
	if runtimePath == "" {
		runtimePath = strings.TrimSpace(os.Getenv("ADA_BOT_RUNTIME"))
	}
	if runtimePath == "" {
		return errors.New("没有配置 Agent 路径，请在设置里选择 ada（或其他 ACP agent）")
	}
	if abs, err := filepath.Abs(runtimePath); err == nil {
		runtimePath = abs
	}
	if info, err := os.Stat(runtimePath); err != nil || info.IsDir() {
		return fmt.Errorf("Agent 路径不存在或不是文件: %s", runtimePath)
	}
	key := app.InstanceKey(b)
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		return errors.New("窗口正在关闭")
	}
	if r, ok := s.instances[key]; ok {
		r.bots[b.ID] = struct{}{}
		state, pid, errText := r.state, r.pid, r.err
		workspace := r.workspace
		s.mu.Unlock()
		s.app.SetBotState(b.ID, state, pid, errText)
		if state == app.BotRunning {
			s.app.SetBotInfo(b.ID, s.app.Prefs.Model, workspace)
		}
		return nil
	}
	info, err := os.Stat(b.Workspace)
	if err != nil || !info.IsDir() {
		s.mu.Unlock()
		return fmt.Errorf("工作目录不存在或不是文件夹: %s", key)
	}
	s.nextID++
	members := s.app.BotIDsInWorkspace(b)
	if len(members) == 0 {
		members = []int{b.ID}
	}
	bots := make(map[int]struct{}, len(members))
	for _, id := range members {
		bots[id] = struct{}{}
	}
	r := &instance{
		id: s.nextID, workspace: b.Workspace, key: key,
		state: app.BotStarting, bots: bots,
		acpReady:     make(chan struct{}),
		acpSessions:  map[int]string{},
		acpBindings:  map[string]acpBinding{},
		acpTurns:     map[int]string{},
		acpApprovals: map[string]chan bool{},
	}
	s.instances[key] = r
	s.mu.Unlock()

	s.setState(r, app.BotStarting, 0, "")
	return s.startACP(r, b, runtimePath)
}

// baseEnv 是 agent 进程的环境:机器人身份 + 供应商 + 机器人自定义变量。
func (s *Supervisor) baseEnv(b *app.Bot) []string {
	env := append(os.Environ(),
		EnvName+"="+b.Name,
		EnvModel+"="+b.Model,
		EnvInstruction+"="+b.Instruction,
		EnvWorkspace+"="+b.Workspace,
	)
	if preset := strings.TrimSpace(s.app.Prefs.AgentPreset); preset != "" {
		env = setEnv(env, "ADA_GO_AGENT_PRESET", preset)
	}
	if provider := strings.TrimSpace(s.app.Prefs.Provider); provider != "" {
		env = setEnv(env, "ADA_LLM_PROVIDER", provider)
	}
	if endpoint := strings.TrimSpace(s.app.Prefs.Endpoint); endpoint != "" {
		env = setEnv(env, "ADA_LLM_ENDPOINT", endpoint)
	}
	if model := strings.TrimSpace(s.app.Prefs.Model); model != "" {
		env = setEnv(env, "ADA_LLM_MODEL", model)
	}
	if key := strings.TrimSpace(s.app.Prefs.APIKey); key != "" {
		env = setEnv(env, "ADA_LLM_API_KEY", key)
	}
	return applyBotEnv(env, b.Env)
}

// acpArgs 返回 ACP 模式的命令行参数。
//
// 默认是 `acp --preset <AgentPreset>`(ada_go 的 `ada acp`);
// ADA_BOT_RUNTIME_ARGS 非空时按它整组覆盖,便于接任意 ACP agent。
func (s *Supervisor) acpArgs() []string {
	if len(s.args) > 0 {
		return append([]string(nil), s.args...)
	}
	if preset := strings.TrimSpace(s.app.Prefs.AgentPreset); preset != "" {
		return []string{"acp", "--preset", preset}
	}
	return nil
}

// startACP 启动一个 ACP agent 进程;连接建立后由 acpConnect 完成握手。
func (s *Supervisor) startACP(r *instance, b *app.Bot, runtimePath string) error {
	args := s.acpArgs()
	if len(args) == 0 {
		err := errors.New("需要 Agent preset 或 ADA_BOT_RUNTIME_ARGS")
		s.setState(r, app.BotError, 0, err.Error())
		s.drop(r)
		return err
	}
	cmd, logPath, cli, err := s.spawnACP(b, runtimePath, args)
	if err != nil {
		s.setState(r, app.BotError, 0, err.Error())
		s.drop(r)
		return err
	}
	r.cmd, r.log, r.acp = cmd, logPath, cli
	s.bindACP(r)
	go s.acpConnect(r, b)
	go s.wait(r)
	return nil
}

func (s *Supervisor) spawnACP(b *app.Bot, runtimePath string, args []string) (*exec.Cmd, string, *acp.Client, error) {
	f, err := os.CreateTemp("", "ada-acp-*.log")
	if err != nil {
		return nil, "", nil, fmt.Errorf("创建机器人日志失败: %w", err)
	}
	cmd := exec.Command(runtimePath, args...)
	cmd.Dir = b.Workspace
	cmd.Env = s.baseEnv(b)
	cmd.Stderr = f
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, fmt.Errorf("ACP stdin 不可用: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, fmt.Errorf("ACP stdout 不可用: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", nil, fmt.Errorf("启动 ACP agent 失败: %w", err)
	}
	_ = f.Close()
	return cmd, f.Name(), acp.New(stdout, stdin), nil
}

// bindACP 把 agent 的通知/反向请求接到当前实例;必须在 Initialize 前完成。
func (s *Supervisor) bindACP(r *instance) {
	r.acp.OnNotification(func(n acp.Notification) { s.acpNotification(r, n) })
	r.acp.Handle("session/request_permission", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.acpPermission(r, ctx, params)
	})
}

func (s *Supervisor) acpConnect(r *instance, b *app.Bot) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := r.acp.Initialize(ctx)
	if err != nil {
		s.mu.Lock()
		r.acpErr = "ACP 初始化失败: " + errText(err)
		s.mu.Unlock()
		close(r.acpReady)
		if r.stopping.Load() || !s.current(r) {
			return
		}
		s.failed(r, "ACP 初始化失败: "+errText(err))
		return
	}
	close(r.acpReady)
	if r.stopping.Load() || !s.current(r) {
		return
	}
	s.setState(r, app.BotRunning, r.cmd.Process.Pid, "")
	s.mu.Lock()
	members := s.members(r)
	s.mu.Unlock()
	for _, id := range members {
		s.app.SetBotInfo(id, s.app.Prefs.Model, b.Workspace)
	}
}

// waitACP 等待 initialize 完成;失败时把初始化错误带回本轮。
func (s *Supervisor) waitACP(ctx context.Context, r *instance) error {
	select {
	case <-r.acpReady:
		s.mu.Lock()
		errText := r.acpErr
		s.mu.Unlock()
		if errText != "" {
			return errors.New(errText)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.acp.Done():
		return errors.New("ACP 连接已关闭")
	}
}

// acpNotification 把 ACP session/update 映射到聊天消息、思考块与工具卡片。
func (s *Supervisor) acpNotification(r *instance, n acp.Notification) {
	r.touch()
	if n.Method != "session/update" {
		return
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(n.Params, &p) != nil || p.SessionID == "" || len(p.Update) == 0 {
		return
	}
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
		ToolCallID    string          `json:"toolCallId"`
		Title         string          `json:"title"`
		Kind          string          `json:"kind"`
		Status        string          `json:"status"`
		RawInput      json.RawMessage `json:"rawInput"`
		RawOutput     json.RawMessage `json:"rawOutput"`
		Entries       []struct {
			Content  string `json:"content"`
			Status   string `json:"status"`
			Priority string `json:"priority"`
		} `json:"entries"`
	}
	if json.Unmarshal(p.Update, &u) != nil {
		return
	}
	s.mu.Lock()
	bind, ok := r.acpBindings[p.SessionID]
	s.mu.Unlock()
	if !ok {
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk":
		if text := acpContentText(u.Content); text != "" {
			s.app.BotDelta(bind.BotID, bind.Turn, text)
		}
	case "agent_thought_chunk":
		if text := acpContentText(u.Content); text != "" {
			s.app.BotReasoning(bind.BotID, bind.Turn, text)
		}
	case "tool_call":
		if u.ToolCallID == "" {
			return
		}
		s.app.BotTool(bind.BotID, bind.Turn, app.ToolEvent{
			ID: u.ToolCallID, Name: u.Title, Args: acpRawString(u.RawInput),
			Status: acpToolStatus(u.Status),
		})
	case "tool_call_update":
		if u.ToolCallID == "" {
			return
		}
		text := acpContentText(u.Content)
		if text == "" {
			text = acpRawString(u.RawOutput)
		}
		status := acpToolStatus(u.Status)
		if u.Status == "" && text == "" {
			return
		}
		s.app.BotTool(bind.BotID, bind.Turn, app.ToolEvent{
			ID: u.ToolCallID, Status: status, Text: text,
		})
	case "plan":
		steps := make([]app.TodoStep, 0, len(u.Entries))
		for _, entry := range u.Entries {
			steps = append(steps, app.TodoStep{
				Content: entry.Content, Status: entry.Status, Priority: entry.Priority,
			})
		}
		s.app.BotTodo(bind.BotID, bind.Turn, app.Todo{Title: u.Title, Steps: steps})
	}
}

// acpSessionFor 返回 ada_chat 会话对应的 agent sessionId;首次发送时创建。
func (s *Supervisor) acpSessionFor(ctx context.Context, r *instance, b *app.Bot, sessionID int) (string, error) {
	s.mu.Lock()
	id := r.acpSessions[sessionID]
	s.mu.Unlock()
	if id != "" {
		return id, nil
	}
	r.acpCreateMu.Lock()
	defer r.acpCreateMu.Unlock()
	s.mu.Lock()
	id = r.acpSessions[sessionID]
	s.mu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := r.acp.NewSession(ctx, b.Workspace)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	r.acpSessions[sessionID] = id
	s.mu.Unlock()
	return id, nil
}

// acpPermission 把 agent 的反向权限请求转成宿主审批面板,并等待用户决定。
func (s *Supervisor) acpPermission(r *instance, ctx context.Context, params json.RawMessage) (any, error) {
	r.touch() // 权限请求本身也是一次进展
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			ToolCallID string `json:"toolCallId"`
			Title      string `json:"title"`
			Kind       string `json:"kind"`
		} `json:"toolCall"`
		Options []acpOptionInfo `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		return acpCancelled(), nil
	}
	s.mu.Lock()
	bind, ok := r.acpBindings[p.SessionID]
	s.mu.Unlock()
	if !ok {
		return acpCancelled(), nil
	}
	allowOption := acpOption(p.Options, true)
	if allowOption == "" {
		return acpCancelled(), nil
	}
	rejectOption := acpOption(p.Options, false)
	s.mu.Lock()
	r.acpPermission++
	seq := r.acpPermission
	s.mu.Unlock()
	id := fmt.Sprintf("acp:%d:%d", bind.BotID, seq)
	if p.ToolCall.ToolCallID != "" {
		id = "acp:" + p.ToolCall.ToolCallID + ":" + strconv.Itoa(seq)
	}
	ch := make(chan bool, 1)
	s.mu.Lock()
	r.acpApprovals[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(r.acpApprovals, id)
		s.mu.Unlock()
	}()
	title := p.ToolCall.Title
	if title == "" {
		title = "ACP 工具调用"
	}
	s.app.BotApproval(bind.BotID, bind.Turn, app.Approval{
		ID: id, Source: "ACP", Op: p.ToolCall.Kind, Target: title,
		Reason: "ACP agent 请求执行工具，需要你确认", Timeout: 30 * time.Second,
	})
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case allow := <-ch:
		if allow {
			return acpSelected(allowOption), nil
		}
		if rejectOption == "" {
			return acpCancelled(), nil
		}
		return acpSelected(rejectOption), nil
	case <-timer.C:
		return acpCancelled(), nil
	case <-ctx.Done():
		return acpCancelled(), nil
	}
}

// acpOption 从 agent 给出的选项里挑放行/拒绝项;只看 kind 与 id,不猜文案。
func acpOption(options []acpOptionInfo, allow bool) string {
	for _, o := range options {
		key := strings.ToLower(o.Kind + " " + o.OptionID)
		if allow && strings.Contains(key, "allow") {
			return o.OptionID
		}
		if !allow && (strings.Contains(key, "reject") || strings.Contains(key, "deny")) {
			return o.OptionID
		}
	}
	return ""
}

func acpSelected(optionID string) map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}
}

func acpCancelled() map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

// acpToolStatus 把 ACP tool status 映射成 ada_chat 的 ToolEvent 状态。
func acpToolStatus(status string) string {
	switch status {
	case "completed":
		return "ok"
	case "failed":
		return "error"
	default:
		return "running"
	}
}

// acpContentText 兼容 ACP 的 content 形态:
// 单个 text block、content block 数组,或嵌套在 content 字段里的 text。
func acpContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if obj.Text != "" {
			return obj.Text
		}
		if len(obj.Content) > 0 {
			return acpContentText(obj.Content)
		}
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var b strings.Builder
		for _, item := range arr {
			b.WriteString(acpContentText(item))
		}
		return b.String()
	}
	return ""
}

// acpRawString 把 rawInput/rawOutput 变成工具卡片可显示的文字。
func acpRawString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// applyBotEnv 把机器人自己的环境变量写进子进程,跳过宿主保留的变量:
// ADA_BOT_NAME/MODEL/INSTRUCTION/WORKSPACE 与 preset 由宿主决定,不能被覆盖。
func applyBotEnv(env []string, extra map[string]string) []string {
	for name, value := range extra {
		if reservedBotEnv[name] {
			continue
		}
		env = setEnv(env, name, value)
	}
	return env
}

// reservedBotEnv 是宿主自己写入、机器人不能覆盖的变量。
var reservedBotEnv = map[string]bool{
	EnvName: true, EnvModel: true,
	EnvInstruction: true, EnvWorkspace: true, "ADA_GO_AGENT_PRESET": true,
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+value)
}

// wait reaps the child and records an unexpected exit.
func (s *Supervisor) wait(r *instance) {
	err := r.cmd.Wait()
	if r.stopping.Load() || s.closing.Load() {
		return
	}
	reason := "机器人进程已退出"
	if err != nil {
		reason += ": " + logTail(r.log)
	}
	s.lost(r, reason)
}

func (s *Supervisor) lost(r *instance, reason string) {
	already := r.stopping.Swap(true)
	if !already {
		s.removeInstance(r)
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	}
	s.setState(r, app.BotError, 0, strings.TrimSpace(reason))
}

func (s *Supervisor) failed(r *instance, reason string) {
	already := r.stopping.Swap(true)
	if !already {
		s.removeInstance(r)
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	}
	s.setState(r, app.BotError, 0, strings.TrimSpace(reason))
}

// Send 让工作区进程回答一轮;流式更新经 session/update 回到发起这一轮的会话。
// sessionID 标识宿主侧的对话,runtime 按它隔离 agent session。
func (s *Supervisor) Send(b *app.Bot, turn int, text string, sessionID int) error {
	key := app.InstanceKey(b)
	s.mu.Lock()
	r := s.instances[key]
	s.mu.Unlock()
	if r == nil {
		// 进程实例不在（上一轮启动失败或状态失配）时，Send 自己补一次启动，
		// 不让第一条消息落在“机器人进程还没有运行”上。
		if err := s.Start(b); err != nil {
			return err
		}
		s.mu.Lock()
		r = s.instances[key]
		s.mu.Unlock()
	}
	if r == nil {
		return errors.New("这个工作区的机器人进程还没有运行")
	}
	return s.sendACP(r, b, turn, text, sessionID)
}

// sendACP 在 ACP 连接上跑一轮:创建/复用 agent session,转发流式更新,最后结束消息。
func (s *Supervisor) sendACP(r *instance, b *app.Bot, turn int, text string, sessionID int) error {
	ctx, cancel := context.WithCancel(context.Background())
	maxCtx, maxCancel := context.WithTimeout(ctx, turnMaxTimeout)
	s.mu.Lock()
	s.cancels[turn] = cancel
	s.mu.Unlock()
	r.idleHit.Store(false)
	r.touch()
	stopWatch := make(chan struct{})

	go func() {
		defer func() {
			close(stopWatch)
			maxCancel()
			cancel()
			s.mu.Lock()
			delete(s.cancels, turn)
			delete(s.canceled, turn)
			s.mu.Unlock()
		}()
		go s.watchIdle(r, turn, turnIdleTimeout, maxCtx, cancel, stopWatch)
		if err := s.waitACP(maxCtx, r); err != nil {
			s.app.BotTurnFailed(b.ID, turn, "ACP 尚未就绪: "+errText(err))
			return
		}
		session, err := s.acpSessionFor(maxCtx, r, b, sessionID)
		if err != nil {
			if s.takeCanceled(turn) {
				s.app.BotTurnDone(b.ID, turn, "")
				return
			}
			s.app.BotTurnFailed(b.ID, turn, "ACP 创建会话失败: "+errText(err))
			return
		}
		s.mu.Lock()
		if r.stopping.Load() || s.instances[r.key] != r {
			s.mu.Unlock()
			s.app.BotTurnFailed(b.ID, turn, "机器人进程已经停止")
			return
		}
		r.acpBindings[session] = acpBinding{BotID: b.ID, Turn: turn}
		r.acpTurns[turn] = session
		s.mu.Unlock()

		stop, perr := r.acp.Prompt(maxCtx, session, text)

		s.mu.Lock()
		if bind, ok := r.acpBindings[session]; ok && bind.Turn == turn {
			delete(r.acpBindings, session)
		}
		delete(r.acpTurns, turn)
		s.mu.Unlock()

		manual := s.takeCanceled(turn)
		if manual || stop == "cancelled" {
			s.app.BotTurnDone(b.ID, turn, "")
			return
		}
		if perr != nil {
			if r.idleHit.Load() {
				s.app.BotTurnFailed(b.ID, turn, "长时间没有新进展("+turnIdleTimeout.String()+"),已中止这一轮")
				return
			}
			s.app.BotTurnFailed(b.ID, turn, "ACP 请求失败: "+errText(perr))
			return
		}
		s.app.BotTurnDone(b.ID, turn, "")
	}()
	return nil
}

// Cancel 终止某轮正在进行的调用:先发 session/cancel 通知,再取消本地等待。
// 取消标记让 Send 把这次失败当作“用户停止”,而不是写一条错误到对话里。
func (s *Supervisor) Cancel(botID, turn int) error {
	_ = botID
	s.mu.Lock()
	cancel := s.cancels[turn]
	if cancel != nil {
		s.canceled[turn] = true
	}
	var client *acp.Client
	var session string
	for _, r := range s.instances {
		if sid := r.acpTurns[turn]; sid != "" && r.acp != nil {
			client, session = r.acp, sid
			break
		}
	}
	s.mu.Unlock()
	if client != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Cancel(cctx, session)
		ccancel()
	}
	if cancel == nil {
		return errors.New("这一轮已经结束")
	}
	cancel()
	return nil
}

func (s *Supervisor) takeCanceled(turn int) bool {
	s.mu.Lock()
	v := s.canceled[turn]
	delete(s.canceled, turn)
	s.mu.Unlock()
	return v
}

// Approve 回答一次 ACP 权限请求;请求只能经发起它的工作区实例送达。
func (s *Supervisor) Approve(botID int, id string, allow bool, reason string) error {
	_ = reason // ACP 的 outcome 只带 optionId,没有自由文本理由
	if s.app.BotByID(botID) == nil {
		return errors.New("机器人不存在")
	}
	s.mu.Lock()
	for _, r := range s.instances {
		if _, ok := r.bots[botID]; !ok {
			continue
		}
		ch := r.acpApprovals[id]
		delete(r.acpApprovals, id)
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- allow:
			default:
			}
		}
		return nil
	}
	s.mu.Unlock()
	// 找不到等待中的请求:多半已超时或该轮已结束,与 DecideApproval 的宽容语义一致。
	return nil
}

// Stop detaches one bot. The shared process is killed only when its last bot
// leaves.
func (s *Supervisor) Stop(id int) error {
	s.mu.Lock()
	var r *instance
	for _, inst := range s.instances {
		if _, ok := inst.bots[id]; ok {
			r = inst
			break
		}
	}
	if r == nil {
		s.mu.Unlock()
		return nil
	}
	delete(r.bots, id)
	last := len(r.bots) == 0
	if last {
		delete(s.instances, r.key)
		r.stopping.Store(true)
	}
	s.mu.Unlock()
	if !last {
		return nil
	}
	if r.acp != nil {
		_ = r.acp.Close()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	return nil
}

// Close stops every workspace process instance.
func (s *Supervisor) Close() error {
	s.closing.Store(true)
	s.mu.Lock()
	runs := make([]*instance, 0, len(s.instances))
	for key, r := range s.instances {
		r.stopping.Store(true)
		runs = append(runs, r)
		delete(s.instances, key)
	}
	s.mu.Unlock()
	for _, r := range runs {
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	}
	return nil
}

func (s *Supervisor) setState(r *instance, state app.BotState, pid int, errText string) {
	s.mu.Lock()
	r.state, r.pid, r.err = state, pid, errText
	members := s.members(r)
	s.mu.Unlock()
	for _, id := range members {
		s.app.SetBotState(id, state, pid, errText)
	}
}

func (s *Supervisor) members(r *instance) []int {
	out := make([]int, 0, len(r.bots))
	for id := range r.bots {
		out = append(out, id)
	}
	return out
}

func (s *Supervisor) current(r *instance) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instances[r.key] == r
}

func (s *Supervisor) removeInstance(r *instance) {
	s.mu.Lock()
	if s.instances[r.key] == r {
		delete(s.instances, r.key)
	}
	s.mu.Unlock()
}

func (s *Supervisor) drop(r *instance) {
	s.removeInstance(r)
	r.stopping.Store(true)
}

func errText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}

// logTail returns the last line of the bot's log, for a failure reason.
func logTail(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return filepath.Base(path)
	}
	if len(last) > 240 {
		last = last[len(last)-240:]
	}
	return last
}
