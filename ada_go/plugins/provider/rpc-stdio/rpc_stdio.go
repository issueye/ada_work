// Package rpcstdio 是 seam/rpc 的 stdio 传输:单进程、单连接。
//
// 它不注册为 builtin,因为 stdio 的所有权属于宿主进程;ACP/CLI attach
// 在自己的入口里用 New(os.Stdin, os.Stdout) 构造并 Listen。
package rpcstdio

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// Transport 是 stdin/stdout 传输。
type Transport struct {
	in  io.Reader
	out io.Writer
}

// New 构造 stdio 传输。
func New(in io.Reader, out io.Writer) *Transport {
	return &Transport{in: in, out: out}
}

// Dial 不支持:stdio 是宿主持有的单连接,不能由协议层主动拨号。
func (t *Transport) Dial(context.Context, seamrpc.Address) (seamrpc.Stream, error) {
	return nil, seamrpc.Unimplemented("stdio 传输不支持 Dial")
}

// Listen 返回一个只接受一次连接的监听器。
func (t *Transport) Listen(_ context.Context, addr seamrpc.Address, _ seamrpc.ListenOptions) (seamrpc.Listener, error) {
	if addr.Network != seamrpc.Stdio {
		return nil, seamrpc.Unimplemented("rpc-stdio 不支持传输 " + string(addr.Network))
	}
	return &listener{addr: addr, stream: &stdioStream{in: t.in, out: t.out, closed: make(chan struct{})}, closed: make(chan struct{})}, nil
}

type listener struct {
	addr   seamrpc.Address
	stream *stdioStream
	closed chan struct{}
	once   sync.Once
	mu     sync.Mutex
	taken  bool
}

func (l *listener) Accept(ctx context.Context) (seamrpc.Stream, error) {
	l.mu.Lock()
	if !l.taken {
		l.taken = true
		l.mu.Unlock()
		return l.stream, nil
	}
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, seamrpc.Unavailable("stdio 监听器已关闭")
	}
}

func (l *listener) Addr() seamrpc.Address { return l.addr }

func (l *listener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

type stdioStream struct {
	in     io.Reader
	out    io.Writer
	closed chan struct{}
	once   sync.Once
}

func (s *stdioStream) init() {
	if s.closed == nil {
		s.closed = make(chan struct{})
	}
}

func (s *stdioStream) Read(p []byte) (int, error) {
	s.init()
	select {
	case <-s.closed:
		return 0, io.EOF
	default:
	}
	return s.in.Read(p)
}

func (s *stdioStream) Write(p []byte) (int, error) {
	s.init()
	select {
	case <-s.closed:
		return 0, seamrpc.Unavailable("stdio 流已关闭")
	default:
	}
	return s.out.Write(p)
}

func (s *stdioStream) Close() error {
	s.init()
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *stdioStream) SetDeadline(t time.Time) error {
	// stdin/stdout 没有通用 deadline;协议层用 ctx + Close。
	return errors.ErrUnsupported
}
