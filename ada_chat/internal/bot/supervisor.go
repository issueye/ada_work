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
	"ada_chat/internal/rpc"
)

// Supervisor owns process instances. The key is the working directory: every
// bot that points at the same workspace shares one process and one RPC peer.
// ada_chat owns the lifecycle so adding a second bot in the same workspace
// never spawns a second process.
type Supervisor struct {
	app  *app.App
	svc  *rpc.Service
	args []string

	mu        sync.Mutex
	instances map[string]*instance
	nextID    int
	closing   atomic.Bool
	// cancels 保存每一轮正在进行的 RPC 调用；canceled 标记由用户主动终止的轮次。
	cancels  map[int]context.CancelFunc
	canceled map[int]bool
	// notify 把 runtime 的 chat/error 事件接到发起该轮调用的 goroutine，避免同一失败
	// 又被 RPC 错误写一遍（事件帧先于响应帧到达）。
	notify map[int]chan struct{}
}

type instance struct {
	id int
	// key 是实例键(目录 + 环境指纹);workspace 只用于展示与 cwd。
	key       string
	workspace string
	cmd       *exec.Cmd
	ep        endpoint
	log       string
	peer      rpc.Peer
	state     app.BotState
	pid       int
	err       string

	// acp 非空时走 ACP 协议;此时 peer/ep 不参与。
	acp           *acp.Client
	acpReady      chan struct{}
	acpErr        string
	acpCreateMu   sync.Mutex
	acpSessions   map[int]string
	acpBindings   map[string]acpBinding
	acpTurns      map[int]string
	acpApprovals  map[string]chan bool
	acpPermission int

	// turns maps a chat turn/message id to the bot that asked for it, so shared
	// process events still update the right conversation.
	turns    map[int]int
	bots     map[int]struct{}
	stopping atomic.Bool
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

// NewSupervisor creates an engine that starts the runtime from the host
// settings (falling back to ADA_BOT_RUNTIME). Bot replies are not local.
func NewSupervisor(a *app.App) (*Supervisor, error) {
	svc := rpc.NewService()
	if err := rpc.RegisterNet(svc); err != nil {
		return nil, err
	}
	if err := rpc.RegisterPipe(svc); err != nil {
		return nil, err
	}
	args, err := runtimeArgs(os.Getenv("ADA_BOT_RUNTIME_ARGS"))
	if err != nil {
		return nil, err
	}
	return &Supervisor{
		app: a, svc: svc, args: args,
		instances: map[string]*instance{},
		cancels:   map[int]context.CancelFunc{},
		canceled:  map[int]bool{},
		notify:    map[int]chan struct{}{},
	}, nil
}

// runtimeArgs parses ADA_BOT_RUNTIME_ARGS as a JSON array of strings.
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

// Start ensures the bot's workspace has one process and adds the bot to it.
func (s *Supervisor) Start(b *app.Bot) error {
	if b == nil {
		return errors.New("机器人不存在")
	}
	runtimePath := strings.TrimSpace(s.app.Prefs.RuntimePath)
	if runtimePath == "" {
		runtimePath = strings.TrimSpace(os.Getenv("ADA_BOT_RUNTIME"))
	}
	if runtimePath == "" {
		return errors.New("没有配置 Agent 路径，请在设置里选择 ada-rpc-agent 或 ada")
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
		acp := r.acp
		workspace := r.workspace
		s.mu.Unlock()
		s.app.SetBotState(b.ID, state, pid, errText)
		if acp != nil && state == app.BotRunning {
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
		state: app.BotStarting,
		turns: map[int]int{}, bots: bots,
	}
	if s.app.Prefs.AgentKindValue() == app.AgentACP {
		r.acpSessions = map[int]string{}
		r.acpBindings = map[string]acpBinding{}
		r.acpTurns = map[int]string{}
		r.acpApprovals = map[string]chan bool{}
		r.acpReady = make(chan struct{})
	}
	s.instances[key] = r
	s.mu.Unlock()

	s.setState(r, app.BotStarting, 0, "")
	if s.app.Prefs.AgentKindValue() == app.AgentACP {
		return s.startACP(r, b, runtimePath)
	}
	ep, err := newEndpoint(r.id)
	if err != nil {
		s.setState(r, app.BotError, 0, err.Error())
		s.drop(r)
		return err
	}
	r.ep = ep
	cmd, logPath, err := s.spawn(b, ep, runtimePath)
	if err != nil {
		s.setState(r, app.BotError, 0, err.Error())
		s.drop(r)
		ep.cleanup()
		return err
	}
	r.cmd, r.log = cmd, logPath
	go s.connect(r)
	go s.wait(r)
	return nil
}

func (s *Supervisor) spawn(b *app.Bot, ep endpoint, runtimePath string) (*exec.Cmd, string, error) {
	f, err := os.CreateTemp("", fmt.Sprintf("ada-bot-%d-*.log", ep.id))
	if err != nil {
		return nil, "", fmt.Errorf("创建机器人日志失败: %w", err)
	}
	cmd := exec.Command(runtimePath, s.args...)
	cmd.Dir = b.Workspace
	env := s.baseEnv(b)
	env = setEnv(env, EnvAddr, ep.addr.String())
	env = setEnv(env, EnvToken, string(ep.token))
	cmd.Env = env
	cmd.Stdout = f
	cmd.Stderr = f
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", fmt.Errorf("启动机器人进程失败: %w", err)
	}
	_ = f.Close()
	return cmd, f.Name(), nil
}

// baseEnv 是两种 runtime 共享的环境:机器人身份 + 供应商 + 机器人自定义变量。
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
		err := errors.New("ACP 模式需要 Agent preset 或 ADA_BOT_RUNTIME_ARGS")
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
// ADA_BOT_ADDR/TOKEN/NAME/MODEL/INSTRUCTION/WORKSPACE 与 preset 由宿主决定,不能被覆盖。
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
	EnvAddr: true, EnvToken: true, EnvName: true, EnvModel: true,
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

// connect retries the dial until the child's listener is up, then subscribes
// and starts pumping events.
func (s *Supervisor) connect(r *instance) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if s.closing.Load() || r.stopping.Load() {
			return
		}
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		p, err := s.svc.Dial(dctx, r.ep.addr, rpc.DialOptions{
			Client:  ClientName,
			Auth:    rpc.Auth{Mode: rpc.AuthToken, Token: r.ep.token},
			Timeout: 2 * time.Second,
		})
		cancel()
		if err == nil {
			sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = p.Subscribe(sctx, "chat/*")
			scancel()
			if err == nil {
				s.mu.Lock()
				if s.closing.Load() || r.stopping.Load() || s.instances[r.key] != r {
					s.mu.Unlock()
					_ = p.Close()
					return
				}
				r.peer = p
				s.mu.Unlock()
				s.setState(r, app.BotRunning, r.cmd.Process.Pid, "")
				s.refreshInfo(r, p)
				go s.events(r, p)
				return
			}
			_ = p.Close()
		}
		if time.Now().After(deadline) {
			s.failed(r, "机器人进程没有连上: "+errText(err))
			return
		}
		select {
		case <-time.After(25 * time.Millisecond):
		case <-time.After(time.Until(deadline)):
		}
	}
}

// refreshInfo reads bot.info and copies the actual model/workspace to every
// bot that shares this process.
func (s *Supervisor) refreshInfo(r *instance, p rpc.Peer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var info struct {
		Model     string `json:"model"`
		Workspace string `json:"workspace"`
	}
	if err := p.Call(ctx, "bot.info", nil, &info); err != nil {
		return
	}
	s.mu.Lock()
	members := s.members(r)
	s.mu.Unlock()
	for _, id := range members {
		s.app.SetBotInfo(id, info.Model, info.Workspace)
	}
}

func (s *Supervisor) events(r *instance, p rpc.Peer) {
	for ev := range p.Events() {
		switch ev.Topic {
		case "chat/delta":
			var d struct {
				Turn int    `json:"turn"`
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					s.app.BotDelta(id, d.Turn, d.Text)
				}
			}
		case "chat/reasoning":
			var d struct {
				Turn int    `json:"turn"`
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 && d.Text != "" {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					s.app.BotReasoning(id, d.Turn, d.Text)
				}
			}
		case "chat/tool":
			var d struct {
				Turn       int    `json:"turn"`
				ID         string `json:"id"`
				Name       string `json:"name"`
				Args       string `json:"args"`
				Status     string `json:"status"`
				Text       string `json:"text"`
				DurationMS int64  `json:"durationMs"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 && d.ID != "" {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					s.app.BotTool(id, d.Turn, app.ToolEvent{
						ID: d.ID, Name: d.Name, Args: d.Args, Status: d.Status, Text: d.Text,
						Duration: time.Duration(d.DurationMS) * time.Millisecond,
					})
				}
			}
		case "chat/plan":
			var d struct {
				Turn  int    `json:"turn"`
				Title string `json:"title"`
				Steps []struct {
					Content  string `json:"content"`
					Status   string `json:"status"`
					Priority string `json:"priority"`
				} `json:"steps"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					steps := make([]app.TodoStep, 0, len(d.Steps))
					for _, step := range d.Steps {
						steps = append(steps, app.TodoStep{
							Content: step.Content, Status: step.Status, Priority: step.Priority,
						})
					}
					s.app.BotTodo(id, d.Turn, app.Todo{Title: d.Title, Steps: steps})
				}
			}
		case "chat/done":
			var d struct {
				Turn int `json:"turn"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					s.app.BotTurnDone(id, d.Turn, "")
				}
			}
		case "chat/approval":
			var d struct {
				Turn      int    `json:"turn"`
				ID        string `json:"id"`
				Source    string `json:"source"`
				Op        string `json:"op"`
				Target    string `json:"target"`
				Reason    string `json:"reason"`
				TimeoutMS int64  `json:"timeoutMs"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 && d.ID != "" {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					s.app.BotApproval(id, d.Turn, app.Approval{
						ID: d.ID, Source: d.Source, Op: d.Op, Target: d.Target, Reason: d.Reason,
						Timeout: time.Duration(d.TimeoutMS) * time.Millisecond,
					})
				}
			}
		case "chat/error":
			var d struct {
				Turn   int    `json:"turn"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Turn > 0 {
				if id := s.turnOwner(r, d.Turn); id > 0 {
					if d.Reason == "" {
						d.Reason = "agent 运行失败"
					}
					s.app.BotTurnFailed(id, d.Turn, d.Reason)
					s.signalNotified(d.Turn)
				}
			}
		}
	}
	if r.stopping.Load() || s.closing.Load() || !s.current(r) {
		return
	}
	s.lost(r, "机器人连接已断开")
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
		if p := r.peer; p != nil {
			_ = p.Close()
		}
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		r.ep.cleanup()
	}
	s.setState(r, app.BotError, 0, strings.TrimSpace(reason))
}

func (s *Supervisor) failed(r *instance, reason string) {
	already := r.stopping.Swap(true)
	if !already {
		s.removeInstance(r)
		if p := r.peer; p != nil {
			_ = p.Close()
		}
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		r.ep.cleanup()
	}
	s.setState(r, app.BotError, 0, strings.TrimSpace(reason))
}

// Send asks the workspace's process to answer one turn; events stream back to
// the bot that owns the turn. sessionID 标识宿主侧的对话，runtime 按它隔离 agent session。
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
	if r.acp != nil {
		return s.sendACP(r, b, turn, text, sessionID)
	}
	s.mu.Lock()
	r.turns[turn] = b.ID
	s.notify[turn] = make(chan struct{}, 1)
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	s.mu.Lock()
	s.cancels[turn] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.cancels, turn)
			delete(s.canceled, turn)
			delete(s.notify, turn)
			s.mu.Unlock()
		}()
		p, reason := s.waitPeer(r, 10*time.Second)
		if p == nil {
			s.forgetTurn(r, turn)
			if s.takeCanceled(turn) {
				s.app.BotTurnDone(b.ID, turn, "")
				return
			}
			s.app.BotTurnFailed(b.ID, turn, reason)
			return
		}
		var res struct {
			Turn int    `json:"turn"`
			Text string `json:"text"`
		}
		params := map[string]any{
			"turn": turn, "text": text,
			"botId": b.ID, "botName": b.Name, "instruction": b.Instruction,
			"sessionId": strconv.Itoa(sessionID),
		}
		err := p.Call(ctx, "chat.send", params, &res)
		manual := s.takeCanceled(turn)
		s.forgetTurn(r, turn)
		if manual {
			s.app.BotTurnDone(b.ID, turn, "")
			return
		}
		if err != nil {
			// runtime 在返回错误前会发 chat/error，事件已把可读原因写进对话；
			// 只有没等到事件（进程断开等）才回落到 RPC 错误 + 机器人日志末行。
			if s.wasNotified(turn) {
				return
			}
			reason := errText(err)
			if tail := logTail(r.log); tail != "" {
				reason += "；" + tail
			}
			s.app.BotTurnFailed(b.ID, turn, reason)
			return
		}
		s.app.BotTurnDone(b.ID, turn, res.Text)
	}()
	return nil
}

// sendACP 在 ACP 连接上跑一轮:创建/复用 agent session,转发流式更新,最后结束消息。
func (s *Supervisor) sendACP(r *instance, b *app.Bot, turn int, text string, sessionID int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	s.mu.Lock()
	s.cancels[turn] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			delete(s.cancels, turn)
			delete(s.canceled, turn)
			s.mu.Unlock()
		}()
		if err := s.waitACP(ctx, r); err != nil {
			s.app.BotTurnFailed(b.ID, turn, "ACP 尚未就绪: "+errText(err))
			return
		}
		session, err := s.acpSessionFor(ctx, r, b, sessionID)
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

		stop, perr := r.acp.Prompt(ctx, session, text)

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
			s.app.BotTurnFailed(b.ID, turn, "ACP 请求失败: "+errText(perr))
			return
		}
		s.app.BotTurnDone(b.ID, turn, "")
	}()
	return nil
}

// Cancel 终止某轮正在进行的调用；RPC 模式取消 context，ACP 模式先发
// session/cancel 通知再取消 context。取消标记让 Send 把这次失败当作“用户停止”，
// 而不是写一条错误到对话里。
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
		if r.acp == nil {
			continue
		}
		if sid := r.acpTurns[turn]; sid != "" {
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

// signalNotified 把 chat/error 已经处理过的轮次告诉 Send；事件帧先于响应帧到达。
func (s *Supervisor) signalNotified(turn int) {
	s.mu.Lock()
	ch := s.notify[turn]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// wasNotified 等一小会儿确认本轮是否已由 chat/error 写过失败原因。
func (s *Supervisor) wasNotified(turn int) bool {
	s.mu.Lock()
	ch := s.notify[turn]
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	case <-time.After(250 * time.Millisecond):
		return false
	}
}

// Approve 回答机器人进程的一次升级审批；请求只能经它所属的工作区进程送达。
func (s *Supervisor) Approve(botID int, id string, allow bool, reason string) error {
	s.mu.Lock()
	for _, r := range s.instances {
		if r.acp == nil {
			continue
		}
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

	bot := s.app.BotByID(botID)
	if bot == nil {
		return errors.New("机器人不存在")
	}
	key := app.InstanceKey(bot)
	s.mu.Lock()
	r := s.instances[key]
	s.mu.Unlock()
	if r == nil {
		return errors.New("这个工作区的机器人进程还没有运行")
	}
	p, why := s.waitPeer(r, 2*time.Second)
	if p == nil {
		return errors.New(why)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Call(ctx, "chat.approve", map[string]any{
		"id": id, "allow": allow, "reason": reason,
	}, nil); err != nil {
		return fmt.Errorf("提交审批结果失败: %w", err)
	}
	return nil
}

func (s *Supervisor) waitPeer(r *instance, timeout time.Duration) (rpc.Peer, string) {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		p := r.peer
		stopped := r.stopping.Load()
		exists := s.instances[r.key] == r
		s.mu.Unlock()
		if p != nil {
			return p, ""
		}
		if stopped || !exists {
			return nil, "机器人进程没有连上"
		}
		if time.Now().After(deadline) {
			return nil, "机器人启动超时"
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	if r.peer != nil {
		_ = r.peer.Close()
	}
	if r.acp != nil {
		_ = r.acp.Close()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	r.ep.cleanup()
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
		if r.peer != nil {
			_ = r.peer.Close()
		}
		if r.acp != nil {
			_ = r.acp.Close()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		r.ep.cleanup()
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

func (s *Supervisor) turnOwner(r *instance, turn int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.turns[turn]
}

func (s *Supervisor) forgetTurn(r *instance, turn int) {
	s.mu.Lock()
	delete(r.turns, turn)
	s.mu.Unlock()
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
