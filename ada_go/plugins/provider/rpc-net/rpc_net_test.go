package rpcnet

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	rpcjson "github.com/issueye/ada_go_v2/plugins/provider/rpc-json"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

func TestTCP(t *testing.T) {
	tr := transport{}
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:0"}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr()
	go func() {
		c, aerr := ln.Accept(context.Background())
		if aerr != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write(buf)
	}()
	c, err := tr.Dial(context.Background(), addr)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 4)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显不正确: %q", buf)
	}
}

func TestUnix(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.sock"
	tr := transport{}
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Unix, Name: path}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if runtime.GOOS != "windows" {
		info, serr := os.Stat(path)
		if serr != nil {
			t.Fatalf("socket 文件不存在: %v", serr)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("socket 权限 %o,期望 600", perm)
		}
	}
	go func() {
		c, aerr := ln.Accept(context.Background())
		if aerr != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		_, _ = io.ReadFull(c, buf)
		_, _ = c.Write(buf)
	}()
	c, err := tr.Dial(context.Background(), seamrpc.Address{Network: seamrpc.Unix, Name: path})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 4)
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显不正确: %q", buf)
	}
}

func TestUnixStale(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/s.sock"
	raw, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("构造 stale socket 失败: %v", err)
	}
	if ul, ok := raw.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = raw.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket 未保留: %v", err)
	}
	tr := transport{}
	if _, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Unix, Name: path}, seamrpc.ListenOptions{}); !seamerr.Is(err, seamerr.ErrInvalid) {
		t.Fatalf("未允许清理时应拒绝: %v", err)
	}
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Unix, Name: path}, seamrpc.ListenOptions{RemoveStale: true})
	if err != nil {
		t.Fatalf("允许清理时应成功: %v", err)
	}
	_ = ln.Close()
}

func TestAcceptCancel(t *testing.T) {
	tr := transport{}
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:0"}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ln.Accept(ctx); !seamerr.Is(err, seamerr.ErrCanceled) {
		t.Fatalf("取消应 ECANCELED: %v", err)
	}
}

func TestUnsupported(t *testing.T) {
	tr := transport{}
	if _, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: "x"}, seamrpc.ListenOptions{}); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("pipe 应 EUNIMPLEMENTED: %v", err)
	}
	if _, err := tr.Dial(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: "x"}); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("pipe 拨号应 EUNIMPLEMENTED: %v", err)
	}
}

// TestKernelStack 验证 seam/rpc + rpc-net + rpc-json 三个插件的装配与真实 TCP 回路。
func TestKernelStack(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamrpc.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam/rpc 失败: %v", err)
	}
	if _, err := app.Plugin(Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 rpc-net 失败: %v", err)
	}
	if _, err := app.Plugin(rpcjson.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 rpc-json 失败: %v", err)
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
	addr := seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:0"}
	// 监听后再取实际端口:Listen 返回的 Server.Stats().Listeners 是实际地址。
	srv, err := svc.Listen(ctx, addr, seamrpc.ServerOptions{
		Auth:    seamrpc.Auth{Mode: seamrpc.AuthToken, Token: []byte("secret")},
		PeerIDs: []string{"test"},
	})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = srv.Close() }()
	actual := srv.Stats().Listeners[0]
	go func() { _ = srv.Serve(ctx) }()
	if _, err := srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params, &in)
		return in, nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	p, err := svc.Dial(ctx, actual, seamrpc.DialOptions{
		Client: "test", Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: []byte("secret")},
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer func() { _ = p.Close() }()
	var out struct {
		Text string `json:"text"`
	}
	if err := p.Call(ctx, "echo", map[string]string{"text": "ok"}, &out); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("结果不正确: %+v", out)
	}
}
