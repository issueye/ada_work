package rpcjson

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type peer struct {
	stream   seamrpc.Stream
	rd       *bufio.Reader
	maxFrame int
	remote   string

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string]chan response

	events    chan seamrpc.Event
	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	seq      atomic.Uint64
	heartD   time.Duration
	lastSeen atomic.Int64
	features map[string]bool
}

// newPeer 建立连接:完成握手后启动读循环与心跳。
func newPeer(ctx context.Context, stream seamrpc.Stream, opts seamrpc.DialOptions, conf config) (*peer, error) {
	p := &peer{
		stream: stream, rd: bufio.NewReader(stream), maxFrame: conf.MaxFrameBytes,
		pending: make(map[string]chan response), events: make(chan seamrpc.Event, conf.MaxEventQueue),
		done: make(chan struct{}), heartD: conf.heartbeat(),
	}
	remote, err := p.handshake(ctx, opts)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	p.remote = remote
	p.lastSeen.Store(time.Now().UnixNano())
	go p.readLoop()
	go p.pingLoop()
	return p, nil
}

func (p *peer) handshake(ctx context.Context, opts seamrpc.DialOptions) (string, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 超时就关流:读帧可能阻塞在任意传输上(pipe 不支持 deadline)。
	stop := context.AfterFunc(hctx, func() { _ = p.stream.Close() })
	defer stop()
	_ = p.stream.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = p.stream.SetDeadline(time.Time{}) }()

	kind, body, err := readFrame(p.rd, p.maxFrame)
	if err != nil {
		return "", seamrpc.Unavailable("读取 challenge 失败: " + err.Error())
	}
	if kind != kindChallenge {
		return "", seamrpc.Invalid("握手期望 challenge")
	}
	var ch challenge
	if err := json.Unmarshal(body, &ch); err != nil || ch.V != protocolVersion {
		return "", seamrpc.Invalid("challenge 非法")
	}
	client := opts.Client
	if client == "" {
		client = "ada"
	}
	mode := opts.Auth.Mode
	if mode == "" {
		mode = modeFor(ch.Modes)
	}
	if mode == seamrpc.AuthNone && !opts.Auth.AllowInsecure {
		return "", seamrpc.AuthFailed("不允许无鉴权连接")
	}
	if mode == seamrpc.AuthToken && len(opts.Auth.Token) == 0 {
		return "", seamrpc.AuthFailed("缺少 token")
	}
	if !modeAllowed(mode, ch.Modes) {
		return "", seamrpc.AuthFailed("服务端不支持鉴权模式 " + string(mode))
	}
	a := authMsg{V: protocolVersion, Client: client}
	if mode == seamrpc.AuthToken {
		a.Proof = proof(opts.Auth.Token, ch.Nonce, client)
	}
	if err := writeFrame(p.stream, kindAuth, a, p.maxFrame); err != nil {
		return "", seamrpc.Unavailable("发送 auth 失败: " + err.Error())
	}
	kind, body, err = readFrame(p.rd, p.maxFrame)
	if err != nil {
		return "", seamrpc.Unavailable("读取 ready 失败: " + err.Error())
	}
	if kind == kindBye {
		var b byeMsg
		_ = json.Unmarshal(body, &b)
		return "", fromWire(&wireError{Code: b.Code, Message: b.Message})
	}
	if kind != kindReady {
		return "", seamrpc.Invalid("握手期望 ready")
	}
	var r ready
	if err := json.Unmarshal(body, &r); err != nil || r.V != protocolVersion || r.Peer == "" {
		return "", seamrpc.Invalid("ready 非法")
	}
	p.features = make(map[string]bool, len(r.Features))
	for _, f := range r.Features {
		p.features[f] = true
	}
	return r.Peer, nil
}

// modeFor 在客户端未指定模式时,按服务端宣告选择。
func modeFor(modes []string) seamrpc.AuthMode {
	for _, m := range modes {
		if m == string(seamrpc.AuthOS) {
			return seamrpc.AuthOS
		}
	}
	return seamrpc.AuthToken
}

func modeAllowed(mode seamrpc.AuthMode, modes []string) bool {
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
			p.fail(seamrpc.Unavailable("连接已断开"))
			return
		}
		switch kind {
		case kindResponse:
			var r response
			if err := json.Unmarshal(body, &r); err != nil || r.V != protocolVersion || r.ID == "" {
				p.fail(seamrpc.Invalid("response 非法"))
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
			if err := json.Unmarshal(body, &ev); err != nil || ev.V != protocolVersion || ev.Topic == "" {
				p.fail(seamrpc.Invalid("event 非法"))
				return
			}
			select {
			case p.events <- seamrpc.Event{Topic: ev.Topic, Seq: ev.Seq, Data: ev.Data}:
			default:
				p.fail(seamrpc.Limit("事件队列已满"))
				return
			}
		case kindPing:
			_ = p.send(kindPong, pingMsg{V: protocolVersion, Ts: time.Now().UnixMilli()})
		case kindPong:
			// 只做存活宣告;当前不因缺 pong 主动断开(宿主负责重连)。
		case kindBye:
			var b byeMsg
			_ = json.Unmarshal(body, &b)
			p.fail(fromWire(&wireError{Code: b.Code, Message: b.Message}))
			return
		default:
			p.fail(seamrpc.Invalid(fmt.Sprintf("未知帧 kind 0x%02x", kind)))
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
			// 2 个心跳周期内没有任何入站帧，视为半开连接。
			if last := p.lastSeen.Load(); last > 0 && time.Since(time.Unix(0, last)) > 2*p.heartD {
				p.fail(seamrpc.Unavailable("心跳超时"))
				return
			}
			if p.features != nil && !p.features["ping"] {
				continue
			}
			if err := p.send(kindPing, pingMsg{V: protocolVersion, Ts: time.Now().UnixMilli()}); err != nil {
				p.fail(seamrpc.Unavailable("心跳发送失败"))
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

// Call 发一次请求并等待响应。
func (p *peer) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if !validMethod(method) {
		return seamrpc.Invalid("方法名非法: " + method)
	}
	b, err := encodeParams(params)
	if err != nil {
		return seamrpc.Invalid("参数编码失败: " + err.Error())
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
				return seamrpc.Invalid("结果解码失败: " + err.Error())
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

// Notify 发一次无响应请求;失败只能从连接状态间接观察。
func (p *peer) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if !validMethod(method) {
		return seamrpc.Invalid("方法名非法: " + method)
	}
	b, err := encodeParams(params)
	if err != nil {
		return seamrpc.Invalid("参数编码失败: " + err.Error())
	}
	req := request{V: protocolVersion, ID: fmt.Sprintf("n-%d", p.seq.Add(1)), Method: method, Params: b, NoReply: true}
	return p.send(kindRequest, req)
}

// Subscribe 订阅 topic;空列表非法。
func (p *peer) Subscribe(ctx context.Context, topics ...string) error {
	if p.features != nil && !p.features["events"] {
		return seamrpc.Unimplemented("服务端未宣告 events 能力")
	}
	if err := ctx.Err(); err != nil {
		return ctxError(err)
	}
	if len(topics) == 0 || len(topics) > 64 {
		return seamrpc.Invalid("订阅 topic 数量必须在 1..64")
	}
	for _, topic := range topics {
		if topic == "" || len(topic) > 128 {
			return seamrpc.Invalid("订阅 topic 非法")
		}
	}
	return p.send(kindSubscribe, subscribeMsg{V: protocolVersion, Topics: topics})
}

func (p *peer) Events() <-chan seamrpc.Event { return p.events }

func (p *peer) Remote() string { return p.remote }

func (p *peer) Close() error {
	p.fail(seamrpc.Unavailable("peer 已关闭"))
	return nil
}

func (p *peer) fail(err error) {
	p.closeOnce.Do(func() {
		p.errMu.Lock()
		p.err = err
		p.errMu.Unlock()
		close(p.done)
		_ = p.stream.Close()
		close(p.events)
	})
}

func (p *peer) errOrUnavailable() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	if p.err != nil {
		return p.err
	}
	return seamrpc.Unavailable("连接已关闭")
}

func encodeParams(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func ctxError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return seamerr.New(seamerr.ErrTimeout, "rpc", "调用超时", true)
	}
	return seamerr.New(seamerr.ErrCanceled, "rpc", "调用已取消", false)
}
