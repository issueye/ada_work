package controlrpc_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlgo "github.com/issueye/ada_go_v2/plugins/core/agent-control"
	controlrpc "github.com/issueye/ada_go_v2/plugins/provider/control-rpc"
	rpcjson "github.com/issueye/ada_go_v2/plugins/provider/rpc-json"
	rpcnet "github.com/issueye/ada_go_v2/plugins/provider/rpc-net"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

type echoRunner struct {
	reg *session.Registry
}

func (r echoRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return r.reg.Open(ctx, "", meta)
}

func (r echoRunner) Run(_ context.Context, sess session.Session, text string) (seamagent.Result, error) {
	body, _ := json.Marshal(map[string]string{"text": "echo:" + text})
	if _, err := sess.Append(session.Event{Type: session.TypeAssistantMessage, Data: body}); err != nil {
		return seamagent.Result{}, err
	}
	return seamagent.Result{TurnID: "t1", Text: "echo:" + text}, nil
}

// TestControlOverRPCJSON 把 control-rpc 接到真实的 rpc-json + tcp 上:
// control.Client -> control-rpc.Client -> rpc.Peer -> rpc-json -> agent-control。
//
// 它验证第 3/4/5 层能拼起来,而不只是各自的 fake 单测。
func TestControlOverRPCJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	for _, p := range []kernel.Plugin{
		session.Plugin(),
		memory.Plugin(),
		seamrpc.Plugin(),
		rpcnet.Plugin(),
		rpcjson.Plugin(),
	} {
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
	}
	reg, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Plugin(kernel.Func("echo-agent", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, perr := ctx.Provide("agent.runner", echoRunner{reg: reg}, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, perr
	}).WithProvide("agent.runner"), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 echo agent 失败: %v", err)
	}
	if _, err := app.Plugin(controlgo.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 agent-control 失败: %v", err)
	}
	raw, err := app.Root().Service(control.ServiceName)
	if err != nil {
		t.Fatalf("control 服务未注册: %v", err)
	}
	inv, ok := raw.(control.Invoker)
	if !ok {
		t.Fatalf("control 服务类型不对: %T", raw)
	}

	chain, err := kernel.Service[*seamrpc.Chain](app.Root(), "rpc")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := chain.Get()
	if err != nil {
		t.Fatalf("rpc 底座未安装: %v", err)
	}
	token := []byte("control-rpc-e2e")
	srv, err := svc.Listen(ctx, seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:0"}, seamrpc.ServerOptions{
		Auth:    seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
		PeerIDs: []string{"e2e"},
	})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, err := controlrpc.Register(srv, inv); err != nil {
		t.Fatalf("注册控制方法失败: %v", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	listeners := srv.Stats().Listeners
	if len(listeners) == 0 {
		t.Fatal("没有监听地址")
	}
	peer, err := svc.Dial(ctx, listeners[0], seamrpc.DialOptions{
		Client: "e2e",
		Auth:   seamrpc.Auth{Mode: seamrpc.AuthToken, Token: token},
	})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	c := control.New(controlrpc.New(peer))
	info, err := c.Create(ctx, control.CreateRequest{Title: "e2e"})
	if err != nil || info.ID == "" {
		t.Fatalf("远程 create 失败: %+v err=%v", info, err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("远程 list 不符: %+v err=%v", list, err)
	}
	turn, err := c.Prompt(ctx, control.PromptRequest{SessionID: info.ID, Text: "hi"})
	if err != nil || turn.Text != "echo:hi" {
		t.Fatalf("远程 prompt 不符: %+v err=%v", turn, err)
	}
	if err := c.Close(ctx, info.ID); err != nil {
		t.Fatalf("远程 close 失败: %v", err)
	}
	if list, err := c.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("close 后 list 应为空: %+v err=%v", list, err)
	}

	_ = peer.Close()
	if err := srv.Close(); err != nil {
		t.Fatalf("关闭 server 失败: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve 异常退出: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve 没有退出")
	}
}
