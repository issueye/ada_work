package rpcguard_test

import (
	"context"
	"errors"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	rpcguard "github.com/issueye/ada_go_v2/plugins/policy/rpc-guard"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

type fakePeer struct {
	called     []string
	notified   []string
	subscribed []string
}

func (p *fakePeer) Call(_ context.Context, method string, _ any, _ any) error {
	p.called = append(p.called, method)
	return nil
}
func (p *fakePeer) Notify(_ context.Context, method string, _ any) error {
	p.notified = append(p.notified, method)
	return nil
}
func (p *fakePeer) Subscribe(_ context.Context, topics ...string) error {
	p.subscribed = append(p.subscribed, topics...)
	return nil
}
func (p *fakePeer) Events() <-chan seamrpc.Event { return nil }
func (p *fakePeer) Remote() string               { return "srv" }
func (p *fakePeer) Close() error                 { return nil }

type fakeServer struct {
	methods []string
}

func (s *fakeServer) Handle(method string, _ seamrpc.Handler) (kernel.Dispose, error) {
	s.methods = append(s.methods, method)
	return nil, nil
}
func (s *fakeServer) Publish(string, any) error   { return nil }
func (s *fakeServer) Serve(context.Context) error { return nil }
func (s *fakeServer) Stats() seamrpc.Stats        { return seamrpc.Stats{} }
func (s *fakeServer) Close() error                { return nil }

type fakeService struct {
	dialAddr   seamrpc.Address
	dialOpts   seamrpc.DialOptions
	listenAddr seamrpc.Address
	listenOpts seamrpc.ServerOptions
	peer       seamrpc.Peer
	srv        seamrpc.Server
}

func (f *fakeService) Dial(_ context.Context, addr seamrpc.Address, opts seamrpc.DialOptions) (seamrpc.Peer, error) {
	f.dialAddr, f.dialOpts = addr, opts
	if f.peer == nil {
		f.peer = &fakePeer{}
	}
	return f.peer, nil
}
func (f *fakeService) Listen(_ context.Context, addr seamrpc.Address, opts seamrpc.ServerOptions) (seamrpc.Server, error) {
	f.listenAddr, f.listenOpts = addr, opts
	if f.srv == nil {
		f.srv = &fakeServer{}
	}
	return f.srv, nil
}

type fakeGetter struct{ token string }

func (g *fakeGetter) Get(_ context.Context, name string) (string, error) {
	if name != "TOKEN" {
		return "", errors.New("missing")
	}
	return g.token, nil
}

func secretChain(t *testing.T, token string) *seamsecrets.Chain {
	t.Helper()
	chain := seamsecrets.NewChain()
	if _, err := chain.Install(&fakeGetter{token: token}); err != nil {
		t.Fatalf("安装 secret 失败: %v", err)
	}
	return chain
}

func principal(id string) context.Context {
	return kernel.WithPrincipal(context.Background(), id)
}

// 1. 出站:地址白名单 + 方法白名单 + rpc.call.<method>。
func TestDial(t *testing.T) {
	broker := caps.NewBroker(nil)
	if err := broker.Grant("p1", "rpc.call.read"); err != nil {
		t.Fatalf("授予失败: %v", err)
	}
	inner := &fakeService{peer: &fakePeer{}}
	g := rpcguard.New(inner, broker, nil, rpcguard.Config{
		Call: []string{"tcp://127.0.0.1:9000"}, Methods: []string{"read"}, Topics: []string{"agent/*"},
	})
	peer, err := g.Dial(principal("p1"), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:9000"}, seamrpc.DialOptions{})
	if err != nil {
		t.Fatalf("白名单内应放行: %v", err)
	}
	if err := peer.Call(principal("p1"), "read", nil, nil); err != nil {
		t.Fatalf("已授权方法应放行: %v", err)
	}
	if err := peer.Call(principal("p1"), "write", nil, nil); err == nil {
		t.Fatalf("方法不在白名单应拒绝")
	}
	if err := peer.Notify(principal("p1"), "write", nil); err == nil {
		t.Fatalf("Notify 方法不在白名单应拒绝")
	}
	if err := peer.Notify(principal("p1"), "read", nil); err != nil {
		t.Fatalf("已授权 Notify 应放行: %v", err)
	}
	if err := peer.Subscribe(principal("p1"), "agent/session"); err != nil {
		t.Fatalf("topic 白名单内应放行: %v", err)
	}
	if err := peer.Subscribe(principal("p1"), "secret/events"); err == nil {
		t.Fatalf("topic 不在白名单应拒绝")
	}
	if _, err := g.Dial(principal("p1"), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:9001"}, seamrpc.DialOptions{}); err == nil {
		t.Fatalf("地址不在白名单应拒绝")
	}
}

// 2. 入站:监听白名单 + rpc.listen.<network> + peer 白名单 + token secret。
func TestListen(t *testing.T) {
	broker := caps.NewBroker(nil)
	if err := broker.Grant("p1", "rpc.listen.unix"); err != nil {
		t.Fatalf("授予失败: %v", err)
	}
	inner := &fakeService{}
	g := rpcguard.New(inner, broker, secretChain(t, "s3"), rpcguard.Config{
		Listen: []string{"unix:///tmp/ada.sock"}, Peers: []string{"cli"},
		Methods: []string{"ping"}, Auth: "token", TokenSecret: "TOKEN",
	})
	srv, err := g.Listen(principal("p1"), seamrpc.Address{Network: seamrpc.Unix, Name: "/tmp/ada.sock"}, seamrpc.ServerOptions{})
	if err != nil {
		t.Fatalf("监听应放行: %v", err)
	}
	if len(inner.listenOpts.PeerIDs) != 1 || inner.listenOpts.PeerIDs[0] != "cli" {
		t.Fatalf("peer 白名单未注入: %+v", inner.listenOpts.PeerIDs)
	}
	if inner.listenOpts.Auth.Mode != seamrpc.AuthToken || string(inner.listenOpts.Auth.Token) != "s3" {
		t.Fatalf("token 未注入: %+v", inner.listenOpts.Auth)
	}
	if _, err := srv.Handle("other", nil); err == nil {
		t.Fatalf("入站方法不在白名单应拒绝")
	}
}

// 3. 非 loopback TCP 默认拒绝;缺 peer 白名单拒绝。
func TestListenDenied(t *testing.T) {
	inner := &fakeService{}
	g := rpcguard.New(inner, nil, nil, rpcguard.Config{
		Listen: []string{"tcp://0.0.0.0:9000"}, Peers: []string{"cli"},
	})
	if _, err := g.Listen(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "0.0.0.0:9000"}, seamrpc.ServerOptions{}); err == nil {
		t.Fatalf("非 loopback 应拒绝")
	}
	g2 := rpcguard.New(inner, nil, nil, rpcguard.Config{
		Listen: []string{"tcp://127.0.0.1:9000"},
	})
	if _, err := g2.Listen(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:9000"}, seamrpc.ServerOptions{}); err == nil {
		t.Fatalf("缺 peer 白名单应拒绝")
	}
}

// 4. auth=token 但 secrets 不可用应失败。
func TestAuthRequiresSecret(t *testing.T) {
	g := rpcguard.New(&fakeService{}, nil, nil, rpcguard.Config{
		Call: []string{"tcp://127.0.0.1:9000"}, Methods: []string{"m"},
		Auth: "token", TokenSecret: "TOKEN",
	})
	if _, err := g.Dial(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:9000"}, seamrpc.DialOptions{}); err == nil {
		t.Fatalf("缺 secrets 服务应失败")
	}
}
