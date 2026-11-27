// Package jsonrpc2 实现最小的双向 JSON-RPC 2.0 连接。
//
// 它只负责消息与 id/取消/错误,不认识 agent、session 或 ACP 方法;
// 这些属于 control-acp 适配层。连接可以是 net.Conn、stdio 或内存管道,
// 因此同一份代码可被 ACP、CLI attach 或测试复用。
package jsonrpc2

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Version 是协议版本。
const Version = "2.0"

// 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Error 是 JSON-RPC 错误对象。
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc2: %d %s", e.Code, e.Message)
}

// Message 是请求、响应或通知的统一表示。
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsRequest 表示带 id 的方法调用。
func (m Message) IsRequest() bool {
	return m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null"
}

// IsNotification 表示没有 id 的方法调用。
func (m Message) IsNotification() bool {
	return m.Method != "" && (len(m.ID) == 0 || string(m.ID) == "null")
}

// IsResponse 表示结果或错误响应。
func (m Message) IsResponse() bool {
	return m.Method == "" && (m.Result != nil || m.Error != nil)
}

func marshal(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Handler 处理一个入站请求/通知;params 是原始 JSON。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// options 是连接参数。
type options struct {
	maxLine        int
	notificationFn func(Message)
}

// Option 配置连接。
type Option func(*options)

// WithMaxLine 设置单行消息上限。
func WithMaxLine(n int) Option { return func(o *options) { o.maxLine = n } }

// WithNotificationHandler 设置入站通知观察器。
//
// 观察器只做观察:通知仍会照常分发给已注册的 Handler,不会被观察器吞掉。
func WithNotificationHandler(fn func(Message)) Option {
	return func(o *options) { o.notificationFn = fn }
}

const defaultMaxLine = 8 << 20

// Conn 是双向 JSON-RPC 连接。
type Conn struct {
	rw  io.ReadWriteCloser
	br  *bufio.Reader
	opt options

	writeMu  sync.Mutex
	mu       sync.Mutex
	handlers map[string]Handler
	pending  map[string]chan Message

	// wg 追踪进行中的 dispatch;读侧结束时先排空,再关闭连接,避免丢响应。
	wg sync.WaitGroup

	done      chan struct{}
	closeOnce sync.Once
	cancel    context.CancelFunc
	ctx       context.Context
	errMu     sync.Mutex
	err       error
	seq       atomic.Uint64
}

// New 创建连接并启动读循环;ctx 取消会关闭连接。
//
// 入站方法的 handler 使用连接作用域的 ctx:连接关闭(对端断开、读写失败、
// 调用方取消)会一并取消 handler,长任务(如 ACP prompt)不会变成孤儿。
func New(ctx context.Context, rw io.ReadWriteCloser, opts ...Option) *Conn {
	o := options{maxLine: defaultMaxLine}
	for _, fn := range opts {
		fn(&o)
	}
	connCtx, cancel := context.WithCancel(ctx)
	c := &Conn{
		rw: rw, br: bufio.NewReader(rw), opt: o, ctx: connCtx, cancel: cancel,
		handlers: make(map[string]Handler), pending: make(map[string]chan Message),
		done: make(chan struct{}),
	}
	go c.readLoop()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	return c
}

// Handle 注册方法;返回的函数用于注销。
func (c *Conn) Handle(method string, h Handler) func() {
	c.mu.Lock()
	c.handlers[method] = h
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.handlers, method)
		c.mu.Unlock()
	}
}

// Call 发起请求并等待响应。
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc2: 参数编码失败: %w", err)
	}
	id := json.RawMessage(fmt.Sprintf("%q", fmt.Sprintf("c-%d", c.seq.Add(1))))
	ch := make(chan Message, 1)
	key := string(id)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	msg := Message{JSONRPC: Version, ID: id, Method: method, Params: raw}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("jsonrpc2: 结果解码失败: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.ctx.Done():
		return c.errOrClosed()
	case <-c.done:
		return c.errOrClosed()
	}
}

// Notify 发送无 id 通知。
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc2: 参数编码失败: %w", err)
	}
	return c.write(Message{JSONRPC: Version, Method: method, Params: raw})
}

// Close 关闭连接;可重复调用。
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		close(c.done)
		_ = c.rw.Close()
	})
	return nil
}

// Done 在连接关闭(对端断开、读写失败或显式 Close)后关闭,
// 供宿主等待一次会话结束。
func (c *Conn) Done() <-chan struct{} { return c.done }

// Context 返回连接作用域 ctx:连接关闭时取消。
//
// 适配器需要长时间存活的后台 goroutine(例如 ACP 更新转发)时复用它,
// 而不是退化成 context.Background。
func (c *Conn) Context() context.Context { return c.ctx }

func (c *Conn) errOrClosed() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err != nil {
		return c.err
	}
	return io.EOF
}

func (c *Conn) fail(err error) {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
	c.Close()
}

// drainOnReadEnd 读侧结束(EOF/读错误)时:先取消连接 ctx 解除进行中 Call 的阻塞,
// 等 in-flight dispatch 写完响应,再关闭连接;避免对端关闭输入后抢在响应写出前关闭输出、丢掉响应。
func (c *Conn) drainOnReadEnd(err error) {
	c.cancel()
	c.wg.Wait()
	c.fail(err)
}

func (c *Conn) write(msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jsonrpc2: 编码消息失败: %w", err)
	}
	if c.opt.maxLine > 0 && len(b)+1 > c.opt.maxLine {
		return fmt.Errorf("jsonrpc2: 消息 %d 超过上限 %d", len(b)+1, c.opt.maxLine)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return io.EOF
	default:
	}
	if _, err := c.rw.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

func (c *Conn) readLoop() {
	for {
		msg, err := readMessage(c.br, c.opt.maxLine)
		if err != nil {
			c.drainOnReadEnd(err)
			return
		}
		switch {
		case msg.IsResponse():
			c.mu.Lock()
			key := string(msg.ID)
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.IsRequest():
			c.wg.Add(1)
			go c.dispatch(msg, true)
		case msg.IsNotification():
			if c.opt.notificationFn != nil {
				c.opt.notificationFn(msg)
			}
			c.wg.Add(1)
			go c.dispatch(msg, false)
		default:
			c.fail(fmt.Errorf("jsonrpc2: 非法消息"))
			return
		}
	}
}

func (c *Conn) dispatch(msg Message, reply bool) {
	defer c.wg.Done()
	c.mu.Lock()
	h := c.handlers[msg.Method]
	c.mu.Unlock()
	if h == nil {
		if reply {
			_ = c.write(Message{JSONRPC: Version, ID: msg.ID, Error: &Error{
				Code: CodeMethodNotFound, Message: "方法不存在: " + msg.Method,
			}})
		}
		return
	}
	out, err := h(c.ctx, msg.Params)
	if !reply {
		return
	}
	resp := Message{JSONRPC: Version, ID: msg.ID}
	if err != nil {
		var rerr *Error
		if errors.As(err, &rerr) {
			resp.Error = rerr
		} else {
			resp.Error = &Error{Code: CodeInternalError, Message: "内部错误"}
		}
	} else {
		b, merr := marshal(out)
		if merr != nil {
			resp.Error = &Error{Code: CodeInternalError, Message: "结果编码失败"}
		} else {
			if b == nil {
				// 成功但无返回值:必须显式写 "result": null,
				// 否则响应既不是 result 也不是 error,对端会判为非法消息。
				b = json.RawMessage("null")
			}
			resp.Result = b
		}
	}
	if werr := c.write(resp); werr != nil {
		c.fail(werr)
	}
}

func readMessage(br *bufio.Reader, max int) (Message, error) {
	for {
		var line []byte
		for {
			part, err := br.ReadSlice('\n')
			line = append(line, part...)
			if max > 0 && len(line) > max {
				return Message{}, fmt.Errorf("jsonrpc2: 消息超过上限 %d", max)
			}
			if err == nil {
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			return Message{}, err
		}
		// 去掉行尾 CR/LF。
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			// 容忍空行:部分客户端会发送多余的换行,不应因此断连。
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return Message{}, fmt.Errorf("jsonrpc2: 解析失败: %w", err)
		}
		if msg.JSONRPC != Version {
			return Message{}, fmt.Errorf("jsonrpc2: 版本不是 2.0")
		}
		return msg, nil
	}
}
