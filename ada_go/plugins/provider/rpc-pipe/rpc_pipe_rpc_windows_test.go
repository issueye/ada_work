//go:build windows

package rpcpipe_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	rpcjson "github.com/issueye/ada_go_v2/plugins/provider/rpc-json"
	rpcpipe "github.com/issueye/ada_go_v2/plugins/provider/rpc-pipe"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// TestRPCOverPipe 是命名管道的端到端回归测试。同步管道句柄上的
// 「读循环等待」会让同一连接上的写永久阻塞;这里用一次 Call + 事件验证
// 读写可以并发,并覆盖 Windows overlapped 管道的关闭路径。
func TestRPCOverPipe(t *testing.T) {
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	for _, p := range []kernel.Plugin{seamrpc.Plugin(), rpcpipe.Plugin(), rpcjson.Plugin()} {
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("mount: %v", err)
		}
	}
	chain, err := kernel.Service[*seamrpc.Chain](app.Root(), "rpc")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := chain.Get()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := []byte("pipe-rpc-secret")
	addr := seamrpc.Address{Network: seamrpc.Pipe, Name: fmt.Sprintf("ada-rpc-json-%d", time.Now().UnixNano())}
	srv, err := svc.Listen(ctx, addr, seamrpc.ServerOptions{
		Auth:    seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
		PeerIDs: []string{"test"},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srv.Close() }()
	go func() { _ = srv.Serve(ctx) }()
	if _, err := srv.Handle("chat.echo", func(context.Context, json.RawMessage) (any, error) {
		_ = srv.Publish("chat/event", map[string]string{"ok": "event"})
		return map[string]string{"ok": "call"}, nil
	}); err != nil {
		t.Fatal(err)
	}

	var peer seamrpc.Peer
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dialCtx, dialCancel := context.WithTimeout(ctx, 2*time.Second)
		peer, err = svc.Dial(dialCtx, addr, seamrpc.DialOptions{
			Client: "test",
			Auth:   seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
		})
		dialCancel()
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = peer.Close() }()
	// If pipe read/write serializes, the Call below blocks forever; close the
	// peer so the test fails instead of hanging the package.
	watchdog := time.AfterFunc(5*time.Second, func() { _ = peer.Close() })
	defer watchdog.Stop()

	if err := peer.Subscribe(ctx, "chat/*"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var result map[string]string
	if err := peer.Call(ctx, "chat.echo", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result["ok"] != "call" {
		t.Errorf("result %+v", result)
	}
	select {
	case ev, ok := <-peer.Events():
		if !ok {
			t.Fatal("events channel closed")
		}
		if ev.Topic != "chat/event" || string(ev.Data) != `{"ok":"event"}` {
			t.Errorf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered over pipe")
	}
}
