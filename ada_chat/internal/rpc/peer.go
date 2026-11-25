package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type peer struct {
	stream   Stream
	rd       *bufio.Reader
	maxFrame int
	remote   string

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan response

	events    chan Event
	eventMu   sync.Mutex
	evClosed  bool
	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	seq    atomic.Uint64
	heartD time.Duration
}

// newPeer handshakes a stream and starts its read and heartbeat loops.
func newPeer(ctx context.Context, stream Stream, opts DialOptions, conf config) (*peer, error) {
	p := &peer{
		stream: stream, rd: bufio.NewReader(stream), maxFrame: conf.maxFrameBytes,
		pending: make(map[string]chan response), events: make(chan Event, conf.maxEventQueue),
		done: make(chan struct{}), heartD: conf.heartbeat,
	}
	remote, err := p.handshake(ctx, opts)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	p.remote = remote
	go p.readLoop()
	go p.pingLoop()
	return p, nil
}

func (p *peer) handshake(ctx context.Context, opts DialOptions) (string, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(hctx, func() { _ = p.stream.Close() })
	defer stop()
	_ = p.stream.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = p.stream.SetDeadline(time.Time{}) }()

	kind, body, err := readFrame(p.rd, p.maxFrame)
	if err != nil {
		return "", Unavailable("读取 challenge 失败: " + err.Error())
	}
	if kind != kindChallenge {
		return "", Invalid("握手期望 challenge")
	}
	var ch challenge
	if err := json.Unmarshal(body, &ch); err != nil || ch.V != protocolVersion {
		return "", Invalid("challenge 非法")
	}
	client := opts.Client
	if client == "" {
		client = "ada"
	}
	mode := opts.Auth.Mode
	if mode == "" {
		mode = modeFor(ch.Modes)
	}
	if mode == AuthNone && !opts.Auth.AllowInsecure {
		return "", AuthFailed("不允许无鉴权连接")
	}
	if mode == AuthToken && len(opts.Auth.Token) == 0 {
		return "", AuthFailed("缺少 token")
	}
	if !modeAllowed(mode, ch.Modes) {
		return "", AuthFailed("服务端不支持鉴权模式 " + string(mode))
	}
	a := authMsg{V: protocolVersion, Client: client}
	if mode == AuthToken {
		a.Proof = proof(opts.Auth.Token, ch.Nonce, client)
	}
	if err := writeFrame(p.stream, kindAuth, a, p.maxFrame); err != nil {
		return "", Unavailable("发送 auth 失败: " + err.Error())
	}
	kind, body, err = readFrame(p.rd, p.maxFrame)
	if err != nil {
		return "", Unavailable("读取 ready 失败: " + err.Error())
	}
	if kind == kindBye {
		var b byeMsg
		_ = json.Unmarshal(body, &b)
		return "", fromWire(&wireError{Code: Code(b.Code), Message: b.Message})
	}
	if kind != kindReady {
		return "", Invalid("握手期望 ready")
	}
	var r ready
	if err := json.Unmarshal(body, &r); err != nil || r.V != protocolVersion || r.Peer == "" {
		return "", Invalid("ready 非法")
	}
	return r.Peer, nil
}

func modeFor(modes []string) AuthMode {
	for _, m := range modes {
		if m == string(AuthOS) {
			return AuthOS
		}
	}
	return AuthToken
}

func modeAllowed(mode AuthMode, modes []string) bool {
	for _, m := range modes {
		if m == string(mode) {
			return true
		}
	}
	return false
}

func (p *peer) readLoop() {
	for {
		kind, body, err := readFrame(p.rd, p.maxFrame)
		if err != nil {
			p.fail(Unavailable("连接已断开"))
			return
		}
		switch kind {
		case kindResponse:
			var r response
			if err := json.Unmarshal(body, &r); err != nil || r.ID == "" {
				p.fail(Invalid("response 非法"))
				return
			}
			p.mu.Lock()
			ch := p.pending[r.ID]
			delete(p.pending, r.ID)
			p.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		case kindEvent:
			var ev event
			if err := json.Unmarshal(body, &ev); err != nil || ev.Topic == "" {
				p.fail(Invalid("event 非法"))
				return
			}
			overflow := false
			p.eventMu.Lock()
			if p.evClosed {
				p.eventMu.Unlock()
				return
			}
			select {
			case p.events <- Event{Topic: ev.Topic, Seq: ev.Seq, Data: ev.Data}:
			default:
				overflow = true
			}
			p.eventMu.Unlock()
			if overflow {
				p.fail(Limit("事件队列已满"))
				return
			}
		case kindPing:
			_ = p.send(kindPong, pingMsg{V: protocolVersion, Ts: time.Now().UnixMilli()})
		case kindPong:
		// Heartbeats are advisory; hosts decide when to reconnect.
		case kindBye:
			var b byeMsg
			_ = json.Unmarshal(body, &b)
			p.fail(fromWire(&wireError{Code: Code(b.Code), Message: b.Message}))
			return
		default:
			p.fail(Invalid(fmt.Sprintf("未知帧 kind 0x%02x", kind)))
			return
		}
	}
}

func (p *peer) pingLoop() {
	t := time.NewTicker(p.heartD)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := p.send(kindPing, pingMsg{V: protocolVersion, Ts: time.Now().UnixMilli()}); err != nil {
				p.fail(Unavailable("心跳发送失败"))
				return
			}
		case <-p.done:
			return
		}
	}
}

func (p *peer) send(kind byte, body any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.done:
		return p.errOrUnavailable()
	default:
	}
	return writeFrame(p.stream, kind, body, p.maxFrame)
}

// Call sends one request and waits for its response.
func (p *peer) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if !validMethod(method) {
		return Invalid("方法名非法: " + method)
	}
	b, err := encodeParams(params)
	if err != nil {
		return Invalid("参数编码失败: " + err.Error())
	}
	id := fmt.Sprintf("c-%d", p.seq.Add(1))
	ch := make(chan response, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()

	req := request{V: protocolVersion, ID: id, Method: method, Params: b}
	if err := p.send(kindRequest, req); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return fromWire(r.Error)
		}
		if result != nil && len(r.Result) > 0 {
			if err := json.Unmarshal(r.Result, result); err != nil {
				return Invalid("结果解码失败: " + err.Error())
			}
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		_ = p.send(kindCancel, cancelMsg{V: protocolVersion, ID: id, Reason: ctx.Err().Error()})
		return ctxError(ctx.Err())
	case <-p.done:
		return p.errOrUnavailable()
	}
}

// Notify sends a request without a response.
func (p *peer) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if !validMethod(method) {
		return Invalid("方法名非法: " + method)
	}
	b, err := encodeParams(params)
	if err != nil {
		return Invalid("参数编码失败: " + err.Error())
	}
	req := request{V: protocolVersion, ID: fmt.Sprintf("n-%d", p.seq.Add(1)), Method: method, Params: b, NoReply: true}
	return p.send(kindRequest, req)
}

// Subscribe subscribes to topics; at least one is required.
func (p *peer) Subscribe(ctx context.Context, topics ...string) error {
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if len(topics) == 0 {
		return Invalid("订阅至少需要一个 topic")
	}
	return p.send(kindSubscribe, subscribeMsg{V: protocolVersion, Topics: topics})
}

func (p *peer) Events() <-chan Event { return p.events }

func (p *peer) Remote() string { return p.remote }

func (p *peer) Close() error {
	p.fail(Unavailable("peer 已关闭"))
	return nil
}

func (p *peer) fail(err error) {
	p.closeOnce.Do(func() {
		p.errMu.Lock()
		p.err = err
		p.errMu.Unlock()
		close(p.done)
		_ = p.stream.Close()
		p.eventMu.Lock()
		if !p.evClosed {
			p.evClosed = true
			close(p.events)
		}
		p.eventMu.Unlock()
	})
}

func (p *peer) errOrUnavailable() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	if p.err != nil {
		return p.err
	}
	return Unavailable("连接已关闭")
}

func encodeParams(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Validate params byte budget on the client too, as the doc requires.
	if len(b) > defaultMaxParams {
		return nil, errParamsTooBig
	}
	return b, nil
}

var errParamsTooBig = errors.New("参数超过上限")

func ctxError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return New(ErrTimeout, "rpc", "调用超时", true)
	}
	return New(ErrCanceled, "rpc", "调用已取消", false)
}
