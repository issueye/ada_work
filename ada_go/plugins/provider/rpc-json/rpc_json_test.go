package rpcjson

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// memTransport 是测试用传输:不占端口,用 net.Pipe 配对。
type memTransport struct {
	ch chan net.Conn
}

func newMemTransport() *memTransport { return &memTransport{ch: make(chan net.Conn, 8)} }

func (m *memTransport) Dial(ctx context.Context, _ seamrpc.Address) (seamrpc.Stream, error) {
	c1, c2 := net.Pipe()
	select {
	case m.ch <- c2:
		return c1, nil
	case <-ctx.Done():
		_ = c1.Close()
		_ = c2.Close()
		return nil, ctx.Err()
	}
}

func (m *memTransport) Listen(_ context.Context, a seamrpc.Address, _ seamrpc.ListenOptions) (seamrpc.Listener, error) {
	return &memListener{m: m, addr: a}, nil
}

type memListener struct {
	m    *memTransport
	addr seamrpc.Address
}

func (l *memListener) Accept(ctx context.Context) (seamrpc.Stream, error) {
	select {
	case c := <-l.m.ch:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (l *memListener) Addr() seamrpc.Address { return l.addr }
func (l *memListener) Close() error          { return nil }

func testService(t *testing.T) (*service, seamrpc.Address) {
	t.Helper()
	conf := config{}
	conf.normalize()
	reg := seamrpc.NewRegistry()
	if _, err := reg.Register(seamrpc.TCP, newMemTransport()); err != nil {
		t.Fatalf("注册内存传输失败: %v", err)
	}
	return &service{reg: reg, conf: conf}, seamrpc.Address{Network: seamrpc.TCP, Name: "mem"}
}

var token = []byte("test-token")

func startServer(t *testing.T, svc *service, addr seamrpc.Address) (seamrpc.Server, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv, err := svc.Listen(ctx, addr, seamrpc.ServerOptions{
		Auth:    seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
		PeerIDs: []string{"test"},
	})
	if err != nil {
		cancel()
		t.Fatalf("监听失败: %v", err)
	}
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	return srv, ctx
}

func dial(t *testing.T, svc *service, addr seamrpc.Address, client string, tok []byte) seamrpc.Peer {
	t.Helper()
	p, err := svc.Dial(context.Background(), addr, seamrpc.DialOptions{
		Client: client, Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: tok},
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestCallAndNotify(t *testing.T) {
	svc, addr := testService(t)
	srv, _ := startServer(t, svc, addr)
	notified := make(chan string, 1)
	if _, err := srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, seamrpc.Invalid("参数不是对象")
		}
		return in, nil
	}); err != nil {
		t.Fatalf("注册 echo 失败: %v", err)
	}
	if _, err := srv.Handle("note", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params, &in)
		notified <- in.Text
		return nil, nil
	}); err != nil {
		t.Fatalf("注册 note 失败: %v", err)
	}
	p := dial(t, svc, addr, "test", token)
	var out struct {
		Text string `json:"text"`
	}
	if err := p.Call(context.Background(), "echo", map[string]string{"text": "hi"}, &out); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out.Text != "hi" {
		t.Fatalf("结果不正确: %+v", out)
	}
	if err := p.Notify(context.Background(), "note", map[string]string{"text": "n"}); err != nil {
		t.Fatalf("通知失败: %v", err)
	}
	select {
	case got := <-notified:
		if got != "n" {
			t.Fatalf("通知内容 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("通知未到达")
	}
}

func TestUnknownMethod(t *testing.T) {
	svc, addr := testService(t)
	startServer(t, svc, addr)
	p := dial(t, svc, addr, "test", token)
	err := p.Call(context.Background(), "no.such", nil, nil)
	if !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("未知方法应 EUNIMPLEMENTED: %v", err)
	}
}

func TestBadTokenAndPeer(t *testing.T) {
	svc, addr := testService(t)
	startServer(t, svc, addr)
	if _, err := svc.Dial(context.Background(), addr, seamrpc.DialOptions{
		Client: "test", Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: []byte("wrong")},
	}); !seamerr.Is(err, seamerr.ErrAuth) {
		t.Fatalf("错误 token 应 EAUTH: %v", err)
	}
	if _, err := svc.Dial(context.Background(), addr, seamrpc.DialOptions{
		Client: "evil", Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
	}); !seamerr.Is(err, seamerr.ErrAuth) {
		t.Fatalf("未授权客户端应 EAUTH: %v", err)
	}
}

func TestCancelPropagates(t *testing.T) {
	svc, addr := testService(t)
	srv, _ := startServer(t, svc, addr)
	seen := make(chan struct{}, 1)
	if _, err := srv.Handle("slow", func(ctx context.Context, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		seen <- struct{}{}
		return nil, ctx.Err()
	}); err != nil {
		t.Fatalf("注册 slow 失败: %v", err)
	}
	p := dial(t, svc, addr, "test", token)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := p.Call(ctx, "slow", nil, nil)
	if !seamerr.Is(err, seamerr.ErrTimeout) {
		t.Fatalf("超时应 ETIMEOUT: %v", err)
	}
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatalf("服务端没有收到取消")
	}
}

func TestEvents(t *testing.T) {
	svc, addr := testService(t)
	srv, _ := startServer(t, svc, addr)
	noop := func(context.Context, json.RawMessage) (any, error) { return nil, nil }
	if _, err := srv.Handle("noop", noop); err != nil {
		t.Fatalf("注册 noop 失败: %v", err)
	}
	p := dial(t, svc, addr, "test", token)
	if err := p.Subscribe(context.Background(), "agent/*"); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	// 用一个调用把 subscribe 帧的先后顺序钉住(服务端读循环按序处理帧)。
	if err := p.Call(context.Background(), "noop", nil, nil); err != nil {
		t.Fatalf("noop 失败: %v", err)
	}
	if err := srv.Publish("agent/session", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	select {
	case ev := <-p.Events():
		if ev.Topic != "agent/session" || ev.Seq == 0 || string(ev.Data) != `{"k":"v"}` {
			t.Fatalf("事件不正确: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("没有收到事件")
	}
}

func TestConcurrentCalls(t *testing.T) {
	svc, addr := testService(t)
	srv, _ := startServer(t, svc, addr)
	if _, err := srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			N int `json:"n"`
		}
		_ = json.Unmarshal(params, &in)
		return in, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	p := dial(t, svc, addr, "test", token)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var out struct {
				N int `json:"n"`
			}
			if err := p.Call(context.Background(), "echo", map[string]int{"n": n}, &out); err != nil {
				errs <- err
				return
			}
			if out.N != n {
				errs <- fmt.Errorf("结果串线: 发 %d 收 %d", n, out.N)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("%v", err)
	}
}

func TestFrameLimits(t *testing.T) {
	if _, _, err := readFrame(nilFrameReader{}, 1024); err == nil {
		t.Fatalf("短帧应失败")
	}
	// 长度为 0 的帧。
	if _, _, err := readFrame(&byteReader{b: []byte{0, 0, 0, 0}}, 1024); err == nil {
		t.Fatalf("零长度应失败")
	}
	// 超过上限。
	if _, _, err := readFrame(&byteReader{b: []byte{0, 0, 0, 9, 1, 2}}, 4); err == nil {
		t.Fatalf("超限应失败")
	}
	var buf writerBuf
	if err := writeFrame(&buf, kindPing, pingMsg{V: 1}, 4); err == nil {
		t.Fatalf("写入超限应失败")
	}
}

type nilFrameReader struct{}

func (nilFrameReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }

type byteReader struct{ b []byte }

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, context.DeadlineExceeded
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

type writerBuf struct{ b []byte }

func (w *writerBuf) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func TestKernelIntegration(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamrpc.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam/rpc 失败: %v", err)
	}
	if _, err := app.Plugin(Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 rpc-json 失败: %v", err)
	}
	mem := newMemTransport()
	if _, err := app.Plugin(kernel.Func("mem-transport", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamrpc.Registry](ctx, "rpc-transport")
		if err != nil {
			return nil, err
		}
		return reg.Register(seamrpc.TCP, mem)
	}).WithInject("rpc-transport"), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载内存传输失败: %v", err)
	}
	chain, err := kernel.Service[*seamrpc.Chain](app.Root(), "rpc")
	if err != nil {
		t.Fatalf("取 rpc 服务失败: %v", err)
	}
	svc, err := chain.Get()
	if err != nil {
		t.Fatalf("取 rpc 实现失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := seamrpc.Address{Network: seamrpc.TCP, Name: "mem"}
	srv, err := svc.Listen(ctx, addr, seamrpc.ServerOptions{
		Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token}, PeerIDs: []string{"test"},
	})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = srv.Close() }()
	go func() { _ = srv.Serve(ctx) }()
	if _, err := srv.Handle("ping", func(context.Context, json.RawMessage) (any, error) {
		return map[string]string{"pong": "1"}, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	p, err := svc.Dial(ctx, addr, seamrpc.DialOptions{
		Client: "test", Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = p.Close() }()
	var out map[string]string
	if err := p.Call(ctx, "ping", nil, &out); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out["pong"] != "1" {
		t.Fatalf("结果不正确: %+v", out)
	}
}
