// Package controlacp 是 control 的最小 ACP 适配器。
//
// 它只做方法翻译,不实现 agent:
//   - initialize
//   - session/new
//   - session/load
//   - session/prompt
//   - session/cancel
//
// 流式 session/update 通过 BindUpdates 订阅 control/update;权限请求通过
// Approver/BindApproval 接到 approval(见 permission.go);client 反向
// fs/terminal 能力见 acp_client.go。
package controlacp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProtocolVersion 是当前实现的 ACP 协议版本。
const ProtocolVersion = 1

// Endpoint 是一个 ACP agent 端点。
type Endpoint struct {
	conn *jsonrpc2.Conn
	ctl  *control.Client

	mu        sync.Mutex
	sourceSet bool
	sessions  map[string]struct{}
	closeOnce sync.Once
	stop      chan struct{}

	// queue 串行化 session/update 与 flush 屏障:所有更新都经过同一条队列,
	// 因此屏障消息一定排在此前入队的更新之后。
	queue chan queuedUpdate

	dropped atomic.Uint64
	failed  atomic.Uint64

	capMu sync.RWMutex
	caps  acpseam.Capabilities
}

// queuedUpdate 是转发队列的元素:flush 非 nil 时表示一个屏障请求。
type queuedUpdate struct {
	update control.Update
	flush  chan struct{}
}

// New 在 conn 上注册 ACP 方法。
func New(conn *jsonrpc2.Conn, ctl *control.Client) *Endpoint {
	e := &Endpoint{
		conn: conn, ctl: ctl, stop: make(chan struct{}),
		queue:    make(chan queuedUpdate, 128),
		sessions: make(map[string]struct{}),
	}
	conn.Handle("initialize", e.initialize)
	conn.Handle("session/new", e.newSession)
	conn.Handle("session/load", e.loadSession)
	conn.Handle("session/prompt", e.prompt)
	conn.Handle("session/cancel", e.cancel)
	go e.forwardUpdates()
	return e
}

// Close 关闭底层连接并回收本连接创建的会话;可重复调用。
func (e *Endpoint) Close() error {
	if e == nil || e.conn == nil {
		return nil
	}
	// 先关闭本端登记的控制层会话,避免长进程里端点反复建立后残留 map 条目。
	// 使用连接 ctx(尚未 Close)以遵守「插件实现不使用 context.Background」。
	if e.ctl != nil {
		for _, id := range e.takeSessions() {
			_ = e.ctl.Close(e.conn.Context(), id)
		}
	}
	e.closeOnce.Do(func() { close(e.stop) })
	return e.conn.Close()
}

// trackSession 记录本端点创建的会话,供 Close 回收。
func (e *Endpoint) trackSession(id string) {
	if id == "" {
		return
	}
	e.mu.Lock()
	e.sessions[id] = struct{}{}
	e.mu.Unlock()
}

func (e *Endpoint) takeSessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.sessions))
	for id := range e.sessions {
		out = append(out, id)
	}
	e.sessions = make(map[string]struct{})
	return out
}

// Stats 返回被丢弃/转发失败的更新计数,用于诊断背压与单条超限。
func (e *Endpoint) Stats() (dropped, failed uint64) {
	if e == nil {
		return 0, 0
	}
	return e.dropped.Load(), e.failed.Load()
}

type initializeParams struct {
	ProtocolVersion    int                 `json:"protocolVersion,omitempty"`
	ClientCapabilities *clientCapabilities `json:"clientCapabilities,omitempty"`
}

// clientCapabilities 是 initialize 里 client 声明的反向能力。
type clientCapabilities struct {
	FS struct {
		ReadTextFile  bool `json:"readTextFile"`
		WriteTextFile bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal"`
}

func (e *Endpoint) initialize(_ context.Context, raw json.RawMessage) (any, error) {
	var p initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "initialize 参数非法"}
		}
	}
	// 协议版本只支持 1;明确拒绝而不是假装协商成功,让客户端尽早失败。
	if p.ProtocolVersion != 0 && p.ProtocolVersion != ProtocolVersion {
		return nil, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: fmt.Sprintf("不支持的 ACP 协议版本 %d(仅支持 %d)", p.ProtocolVersion, ProtocolVersion),
		}
	}
	e.setCapabilities(p.ClientCapabilities)
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image": false, "audio": false, "embeddedContext": false,
			},
		},
	}, nil
}

type newSessionParams struct {
	Cwd        string `json:"cwd,omitempty"`
	MCPServers []any  `json:"mcpServers,omitempty"`
}

func (e *Endpoint) newSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var p newSessionParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/new 参数非法"}
		}
	}
	if err := rejectMCPServers(p.MCPServers); err != nil {
		return nil, rpcError(err)
	}
	info, err := e.ctl.Create(ctx, control.CreateRequest{Title: "ACP", Cwd: p.Cwd})
	if err != nil {
		return nil, rpcError(err)
	}
	e.trackSession(info.ID)
	return map[string]any{"sessionId": info.ID}, nil
}

// rejectMCPServers 在未实现 MCP 时显式拒绝,而不是静默忽略客户端配置。
func rejectMCPServers(servers []any) error {
	if len(servers) == 0 {
		return nil
	}
	return control.Unimplemented("session 暂不支持 mcpServers,请先在 preset 中配置工具")
}

func (e *Endpoint) loadSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID  string `json:"sessionId"`
		Cwd        string `json:"cwd,omitempty"`
		MCPServers []any  `json:"mcpServers,omitempty"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/load 需要 sessionId"}
	}
	if err := rejectMCPServers(p.MCPServers); err != nil {
		return nil, rpcError(err)
	}
	if _, err := e.ctl.Load(ctx, p.SessionID, p.Cwd); err != nil {
		return nil, rpcError(err)
	}
	e.trackSession(p.SessionID)
	// 先回放历史,再返回 null(ACP session/load 的结果就是 null)。
	history, err := e.ctl.History(ctx, p.SessionID)
	if err != nil && !seamerr.Is(err, seamerr.ErrUnimplemented) {
		return nil, rpcError(err)
	}
	for _, u := range history {
		params := replayUpdateParams(u)
		if params == nil {
			continue
		}
		if err := e.conn.Notify(ctx, "session/update", params); err != nil {
			return nil, rpcError(err)
		}
	}
	return nil, nil
}

type promptPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Title    string `json:"title,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type promptParams struct {
	SessionID string       `json:"sessionId"`
	Prompt    []promptPart `json:"prompt"`
}

// renderPrompt 把 ACP prompt 内容块转成一条用户文本。
//
// 只接受 text 与 resource_link:text 直接拼接,resource_link 渲染成路径提示,
// 由 agent 用工具读取;image/audio/embeddedContext 未实现,按 ACP 声明的能力
// 拒绝而不是静默丢弃。unknown 类型同样拒绝,避免客户端以为内容已送达。
func renderPrompt(parts []promptPart) (string, error) {
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "text":
			b.WriteString(part.Text)
		case "resource_link":
			label := part.Title
			if label == "" {
				label = part.Name
			}
			if label == "" {
				label = part.URI
			}
			if part.URI == "" && label == "" {
				return "", fmt.Errorf("resource_link 缺少 uri")
			}
			b.WriteString("\n[file] ")
			b.WriteString(label)
			if part.URI != "" && part.URI != label {
				b.WriteString(" <" + part.URI + ">")
			}
			if part.MimeType != "" {
				b.WriteString(" (" + part.MimeType + ")")
			}
			b.WriteString("\n")
		case "image", "audio":
			return "", fmt.Errorf("未实现 %s prompt 内容", part.Type)
		case "resource":
			return "", fmt.Errorf("未实现 embeddedContext prompt 内容")
		default:
			return "", fmt.Errorf("未知 prompt 类型: %s", part.Type)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func (e *Endpoint) prompt(ctx context.Context, raw json.RawMessage) (any, error) {
	var p promptParams
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/prompt 参数非法"}
	}
	text, err := renderPrompt(p.Prompt)
	if err != nil {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: err.Error()}
	}
	if text == "" {
		return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "session/prompt 需要 text"}
	}
	res, err := e.ctl.Prompt(ctx, control.PromptRequest{SessionID: p.SessionID, Text: text})
	// prompt 返回前先把本轮已入队的 session/update 写完,避免响应先于
	// 最后一批增量/工具更新到达客户端。失败路径同样要 flush(例如工具错误)。
	e.flushUpdates(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	reason := res.StopReason
	if reason == "" {
		reason = "end_turn"
	}
	return map[string]any{"stopReason": reason}, nil
}

func (e *Endpoint) cancel(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.SessionID == "" {
		return nil, nil
	}
	_ = e.ctl.Cancel(ctx, p.SessionID)
	return nil, nil
}

// SetUpdates 注入 control/update 源;只能设置一次,重复调用忽略。
//
// 宿主应把 core/agent-control 发出的 control/update 事件转发到这个 channel。
func (e *Endpoint) SetUpdates(ch <-chan control.Update) {
	if e == nil || ch == nil || !e.claimSource() {
		return
	}
	// 外部 channel 只负责生产;入队仍走 e.queue,保证与 flush 屏障同一顺序。
	go func() {
		for {
			select {
			case <-e.stop:
				return
			case u, ok := <-ch:
				if !ok {
					return
				}
				e.enqueue(u)
			}
		}
	}()
}

// BindUpdates 订阅 kernel 的 control/update 事件并转发成 ACP session/update。
// Go 与 TS 控制实现都通过同一个事件名,因此 ACP 不关心控制层是哪种语言实现。
func (e *Endpoint) BindUpdates(ctx *kernel.Context) kernel.Dispose {
	if e == nil || ctx == nil || !e.claimSource() {
		return func(context.Context) error { return nil }
	}
	dispose := ctx.On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				e.enqueue(u)
			}
		}
		return nil, nil
	})
	return func(c context.Context) error { return dispose(c) }
}

// claimSource 保证同一个端点只有一个更新来源;重复设置会被忽略。
func (e *Endpoint) claimSource() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sourceSet {
		return false
	}
	e.sourceSet = true
	return true
}

// enqueue 非阻塞入队:慢客户端/积压时丢更新并计数,不阻塞控制层。
func (e *Endpoint) enqueue(u control.Update) {
	select {
	case e.queue <- queuedUpdate{update: u}:
	default:
		e.dropped.Add(1)
	}
}

// flushUpdates 等待此前入队的更新全部写出,用于保证 prompt 响应顺序。
func (e *Endpoint) flushUpdates(ctx context.Context) {
	if e == nil {
		return
	}
	done := make(chan struct{})
	select {
	case e.queue <- queuedUpdate{flush: done}:
	case <-ctx.Done():
		return
	case <-e.stop:
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	case <-e.stop:
	}
}

func (e *Endpoint) forwardUpdates() {
	ctx := e.conn.Context()
	for {
		select {
		case <-e.stop:
			e.drainFlushes()
			return
		case q := <-e.queue:
			if q.flush != nil {
				close(q.flush)
				continue
			}
			params := sessionUpdateParams(q.update)
			if params == nil {
				continue
			}
			if err := e.conn.Notify(ctx, "session/update", params); err != nil {
				// 单条失败(典型是超过 maxLine)不应终止整条事件流;
				// 连接已关闭时才退出,其余错误继续处理后续更新。
				e.failed.Add(1)
				select {
				case <-e.conn.Done():
					e.drainFlushes()
					return
				default:
				}
			}
		}
	}
}

// drainFlushes 在转发器退出时释放仍在等待的屏障,避免 prompt 卡到超时。
func (e *Endpoint) drainFlushes() {
	for {
		select {
		case q := <-e.queue:
			if q.flush != nil {
				close(q.flush)
			}
		default:
			return
		}
	}
}

// controlPlaneKinds 没有 ACP 等价物;实时转发直接跳过,避免非法 sessionUpdate。
var controlPlaneKinds = map[string]bool{
	"turn/start": true, "turn/end": true,
	"step/start": true, "step/end": true,
	"user/message": true, "user_message": true,
	"compact/summary": true, "error": true,
}

func sessionUpdateParams(u control.Update) map[string]any {
	if controlPlaneKinds[u.Kind] {
		return nil
	}
	update := map[string]any{"sessionUpdate": u.Kind}
	switch u.Kind {
	case "assistant/message", "assistant_message", "assistant/delta", "assistant_delta",
		"assistant/reasoning", "assistant_reasoning":
		text, recognized := messageText(u.Data)
		if !recognized {
			// 形状不认识时不把内部 JSON 当文本透出;严格客户端会把它显示给用户。
			return nil
		}
		if text == "" {
			// 纯工具调用的 assistant 消息没有文本,不产生空 chunk。
			return nil
		}
		update["sessionUpdate"] = "agent_message_chunk"
		if u.Kind == "assistant/reasoning" || u.Kind == "assistant_reasoning" {
			update["sessionUpdate"] = "agent_thought_chunk"
		}
		update["content"] = map[string]any{"type": "text", "text": text}
	case "tool/call", "tool_call":
		mapped := toolCallUpdate(u.Data)
		if mapped == nil {
			return nil
		}
		update = mapped
	case "tool/result", "tool_result":
		mapped := toolResultUpdate(u.Data)
		if mapped == nil {
			return nil
		}
		update = mapped
	case planseam.EventUpdated:
		p, derr := planseam.Decode(u.Data)
		if derr != nil || len(p.Steps) == 0 {
			return nil
		}
		entries := make([]map[string]any, 0, len(p.Steps))
		for _, s := range p.Steps {
			entries = append(entries, map[string]any{
				"content": s.Content, "status": string(s.Status), "priority": string(s.Priority),
			})
		}
		update = map[string]any{"sessionUpdate": "plan", "entries": entries}
	default:
		// 插件自有事件与未知事件没有 ACP 等价物;不向客户端发明非法
		// sessionUpdate,也不透传原始 JSON。
		return nil
	}
	return map[string]any{"sessionId": u.SessionID, "update": update}
}

// replayUpdateParams 用于 session/load 回放:把 user/message 还原成
// user_message_chunk;实时转发不回显用户消息。
func replayUpdateParams(u control.Update) map[string]any {
	switch u.Kind {
	case "user/message", "user_message":
		text, recognized := messageText(u.Data)
		if !recognized || text == "" {
			return nil
		}
		return map[string]any{"sessionId": u.SessionID, "update": map[string]any{
			"sessionUpdate": "user_message_chunk",
			"content":       map[string]any{"type": "text", "text": text},
		}}
	}
	return sessionUpdateParams(u)
}

// toolCallUpdate 把 tool/call 映射成 ACP tool_call;CallInfo 的 id 是关联键,
// 缺失时不做映射(避免产生无法收尾的 UI 条目)。
func toolCallUpdate(raw json.RawMessage) map[string]any {
	var info struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &info); err != nil || info.ID == "" {
		return nil
	}
	title := info.Name
	if title == "" {
		title = info.ID
	}
	out := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    info.ID,
		"title":         title,
		"kind":          toolKind(info.Name),
		"status":        "in_progress",
	}
	if len(info.Args) > 0 {
		out["rawInput"] = info.Args
	}
	return out
}

// toolResultUpdate 把 tool/result 映射成 ACP tool_call_update:只把文本内容
// 交给 ACP,原始 details 放进 rawOutput,避免私有字段混进 content。
func toolResultUpdate(raw json.RawMessage) map[string]any {
	var payload struct {
		Call struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"call"`
		Result struct {
			Content []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"content"`
			Status  string          `json:"status"`
			Details json.RawMessage `json:"details"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Call.ID == "" {
		return nil
	}
	out := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    payload.Call.ID,
		"status":        toolStatus(payload.Result.Status),
	}
	var b strings.Builder
	for _, block := range payload.Result.Content {
		if block.Kind == "" || block.Kind == "text" {
			b.WriteString(block.Text)
		}
	}
	if b.Len() > 0 {
		out["content"] = []map[string]any{{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": b.String()},
		}}
	}
	if len(payload.Result.Details) > 0 {
		out["rawOutput"] = payload.Result.Details
	}
	return out
}

// toolKind 把工具名映射成 ACP 的 ToolKind;只做粗分类,未知一律 other。
func toolKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "search"), strings.Contains(n, "grep"), strings.Contains(n, "find"):
		return "search"
	case strings.Contains(n, "read"), strings.Contains(n, "list"), strings.Contains(n, "view"):
		return "read"
	case strings.Contains(n, "delete"), strings.Contains(n, "remove"):
		return "delete"
	case strings.Contains(n, "move"), strings.Contains(n, "rename"):
		return "move"
	case strings.Contains(n, "write"), strings.Contains(n, "edit"),
		strings.Contains(n, "patch"), strings.Contains(n, "replace"):
		return "edit"
	case strings.Contains(n, "shell"), strings.Contains(n, "exec"),
		strings.Contains(n, "command"), strings.Contains(n, "run"):
		return "execute"
	case strings.Contains(n, "http"), strings.Contains(n, "fetch"), strings.Contains(n, "web"):
		return "fetch"
	}
	return "other"
}

// toolStatus 把 tools.Status 映射成 ACP 的 ToolCallStatus。
// 未知状态按失败处理:宁可让 UI 显示失败,也不假装成功。
func toolStatus(s string) string {
	switch s {
	case "ok", "":
		return "completed"
	}
	return "failed"
}

// textBlock 是 assistant/user 消息里的一小块内容。
type textBlock struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// messageText 从 assistant/user 事件里取文本,兼容 {"message":{...}}、
// 直接 llm.Message 与 {"text":...} 三种形状。第二个返回值表示「认得形状」:
// 空消息返回 ("", true),未知 JSON 返回 false(调用方回退原文)。
func messageText(raw json.RawMessage) (string, bool) {
	var envelope struct {
		Message json.RawMessage `json:"message"`
		Content []textBlock     `json:"content"`
		Text    *string         `json:"text"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", false
	}
	blocks := envelope.Content
	recognized := envelope.Message != nil || envelope.Content != nil || envelope.Text != nil
	if envelope.Message != nil {
		var msg struct {
			Content []textBlock `json:"content"`
		}
		if err := json.Unmarshal(envelope.Message, &msg); err == nil {
			blocks = msg.Content
		}
	}
	var b strings.Builder
	for _, c := range blocks {
		if c.Kind == "" || c.Kind == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() > 0 {
		return b.String(), true
	}
	if envelope.Text != nil {
		return *envelope.Text, true
	}
	return "", recognized
}

func rpcError(err error) *jsonrpc2.Error {
	if err == nil {
		return nil
	}
	code := jsonrpc2.CodeInternalError
	if seamerr.Is(err, seamerr.ErrInvalid) {
		code = jsonrpc2.CodeInvalidParams
	}
	return &jsonrpc2.Error{Code: code, Message: err.Error()}
}
