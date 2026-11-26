// Package acp implements the client half of the Agent Client Protocol (ACP):
// newline-delimited JSON-RPC 2.0 over the agent process's stdio.
//
// It intentionally knows nothing about ada_chat's app model; the bot supervisor
// maps session/update notifications and session/request_permission requests to
// UI state. Only the methods this host needs are implemented.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// ProtocolVersion 是本客户端实现的 ACP 协议版本。
const ProtocolVersion = 1

// maxLine 是单行 JSON-RPC 消息上限,与 ada_go 的 jsonrpc2 默认值一致。
const maxLine = 8 << 20

// RPCError 是 JSON-RPC 错误对象。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("acp: %d %s", e.Code, e.Message)
}

// Notification 是 agent 发来的无 id 消息。
type Notification struct {
	Method string
	Params json.RawMessage
}

// Handler 处理 agent 发来的反向请求(例如 session/request_permission)。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m message) hasID() bool {
	return len(m.ID) > 0 && string(m.ID) != "null"
}

// Client 是一条 ACP stdio 连接。
type Client struct {
	r  io.Reader
	w  io.WriteCloser
	br *bufio.Reader

	writeMu  sync.Mutex
	mu       sync.Mutex
	nextID   atomic.Uint64
	pending  map[string]chan message
	handlers map[string]Handler
	onNotify func(Notification)
	err      error

	done      chan struct{}
	closeOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
}

// New 创建客户端并启动读循环;r 通常是子进程 stdout,w 通常是 stdin。
func New(r io.Reader, w io.WriteCloser) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		r: r, w: w, br: bufio.NewReader(r),
		pending:  map[string]chan message{},
		handlers: map[string]Handler{},
		done:     make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}
	go c.readLoop()
	return c
}

// OnNotification 设置通知观察器;只允许设置一次,通知按到达顺序同步分发。
func (c *Client) OnNotification(fn func(Notification)) {
	c.mu.Lock()
	c.onNotify = fn
	c.mu.Unlock()
}

// Handle 注册 agent → client 的反向请求处理器。
func (c *Client) Handle(method string, h Handler) {
	c.mu.Lock()
	c.handlers[method] = h
	c.mu.Unlock()
}

// Done 在连接关闭后关闭。
func (c *Client) Done() <-chan struct{} { return c.done }

// Close 关闭连接;可重复调用。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.done)
		_ = c.w.Close()
		if rc, ok := c.r.(io.Closer); ok {
			_ = rc.Close()
		}
	})
	return nil
}

// Initialize 完成 ACP 握手。
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		// 本宿主暂不实现 ACP 的文件/终端反向能力;声明 false,让 agent 走本地工具。
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}
	var res struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if res.ProtocolVersion != 0 && res.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("acp: 协议版本不匹配: agent=%d client=%d", res.ProtocolVersion, ProtocolVersion)
	}
	return nil
}

// NewSession 在 agent 上创建一个会话,返回 agent 侧的 sessionId。
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "session/new", map[string]any{
		"cwd": cwd, "mcpServers": []any{},
	}, &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		return "", errors.New("acp: session/new 没有返回 sessionId")
	}
	return res.SessionID, nil
}

// Prompt 发送一轮用户输入,返回 stopReason(通常 end_turn/cancelled)。
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := c.Call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	}, &res)
	return res.StopReason, err
}

// Cancel 发送 session/cancel 通知。
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.Notify(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
}

// Call 发送请求并等待响应。
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("acp: 参数编码失败: %w", err)
	}
	id := fmt.Sprintf("c-%d", c.nextID.Add(1))
	ch := make(chan message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	msg := message{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%q", id)), Method: method, Params: raw}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("acp: 结果解码失败: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return c.closedErr()
	}
}

// Notify 发送一条无 id 通知。
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("acp: 参数编码失败: %w", err)
	}
	return c.write(message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Client) write(msg message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: 消息编码失败: %w", err)
	}
	if len(b)+1 > maxLine {
		return fmt.Errorf("acp: 消息超过上限 %d", maxLine)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		line, err := readLine(c.br)
		if err != nil {
			c.fail(err)
			return
		}
		if len(line) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			c.fail(fmt.Errorf("acp: 解析消息失败: %w", err))
			return
		}
		if msg.JSONRPC != "2.0" {
			c.fail(errors.New("acp: 消息不是 JSON-RPC 2.0"))
			return
		}
		switch {
		case msg.Method != "" && msg.hasID():
			go c.dispatch(msg)
		case msg.Method != "":
			c.mu.Lock()
			fn := c.onNotify
			c.mu.Unlock()
			if fn != nil {
				func() {
					defer func() { _ = recover() }()
					fn(Notification{Method: msg.Method, Params: msg.Params})
				}()
			}
		default:
			c.deliver(msg)
		}
	}
}

func (c *Client) deliver(msg message) {
	key := idKey(msg.ID)
	c.mu.Lock()
	ch := c.pending[key]
	delete(c.pending, key)
	c.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

// idKey 归一化 JSON-RPC id:客户端发字符串 id,服务端必须原样回显;
// 去掉引号后与 pending 表里的键一致。
func idKey(raw json.RawMessage) string {
	return strings.Trim(string(raw), `"`)
}

func (c *Client) dispatch(msg message) {
	c.mu.Lock()
	h := c.handlers[msg.Method]
	c.mu.Unlock()
	var (
		out any
		err error
	)
	if h == nil {
		err = &RPCError{Code: -32601, Message: "方法不存在: " + msg.Method}
	} else {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					err = &RPCError{Code: -32603, Message: fmt.Sprintf("处理器 panic: %v", rec)}
				}
			}()
			out, err = h(c.ctx, msg.Params)
		}()
	}
	resp := message{JSONRPC: "2.0", ID: msg.ID}
	if err != nil {
		var rerr *RPCError
		if errors.As(err, &rerr) {
			resp.Error = rerr
		} else {
			resp.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
	} else {
		raw, merr := json.Marshal(out)
		if merr != nil {
			resp.Error = &RPCError{Code: -32603, Message: "结果编码失败"}
		} else {
			if raw == nil {
				raw = json.RawMessage("null")
			}
			resp.Result = raw
		}
	}
	if werr := c.write(resp); werr != nil {
		c.fail(werr)
	}
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	_ = c.Close()
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return io.EOF
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxLine {
			return nil, fmt.Errorf("acp: 消息超过上限 %d", maxLine)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}
