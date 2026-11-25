package rpcobserve_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	rpcobserve "github.com/issueye/ada_go_v2/plugins/policy/rpc-observe"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

type fakePeer struct{}

func (fakePeer) Call(context.Context, string, any, any) error { return nil }
func (fakePeer) Notify(context.Context, string, any) error    { return nil }
func (fakePeer) Subscribe(context.Context, ...string) error   { return nil }
func (fakePeer) Events() <-chan seamrpc.Event                 { return nil }
func (fakePeer) Remote() string                               { return "srv" }
func (fakePeer) Close() error                                 { return nil }

type fakeServer struct{}

func (fakeServer) Handle(string, seamrpc.Handler) (kernel.Dispose, error) { return nil, nil }
func (fakeServer) Publish(string, any) error                              { return nil }
func (fakeServer) Serve(context.Context) error                            { return nil }
func (fakeServer) Stats() seamrpc.Stats                                   { return seamrpc.Stats{} }
func (fakeServer) Close() error                                           { return nil }

type fakeService struct{}

func (fakeService) Dial(context.Context, seamrpc.Address, seamrpc.DialOptions) (seamrpc.Peer, error) {
	return fakePeer{}, nil
}
func (fakeService) Listen(context.Context, seamrpc.Address, seamrpc.ServerOptions) (seamrpc.Server, error) {
	return fakeServer{}, nil
}

// 1. Dial/Call 发出 out 事件,含 peer/method/transport。
func TestObserveOutbound(t *testing.T) {
	var payloads []map[string]any
	g := rpcobserve.New(fakeService{}, func(_ string, p any) {
		if m, ok := p.(map[string]any); ok {
			payloads = append(payloads, m)
		}
	})
	peer, err := g.Dial(context.Background(), seamrpc.Address{Network: seamrpc.TCP, Name: "127.0.0.1:1"}, seamrpc.DialOptions{})
	if err != nil {
		t.Fatalf("Dial 失败: %v", err)
	}
	if err := peer.Call(context.Background(), "ping", nil, nil); err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if len(payloads) != 2 {
		t.Fatalf("应有 dial/call 两条事件,实际 %d", len(payloads))
	}
	if payloads[0]["direction"] != "out" || payloads[0]["method"] != "dial" || payloads[0]["transport"] != "tcp" {
		t.Fatalf("dial 事件不正确: %+v", payloads[0])
	}
	if payloads[1]["method"] != "ping" || payloads[1]["peer"] != "srv" {
		t.Fatalf("call 事件不正确: %+v", payloads[1])
	}
}

// 2. 入站 handler 事件带 rpc:<peer> 转出的 peer。
func TestObserveInbound(t *testing.T) {
	var payload map[string]any

	inner := &captureService{srv: &captureServer{}}
	g := rpcobserve.New(inner, func(_ string, p any) {
		if m, ok := p.(map[string]any); ok {
			payload = m
		}
	})
	srv, err := g.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Unix, Name: "/tmp/x"}, seamrpc.ServerOptions{})
	if err != nil {
		t.Fatalf("Listen 失败: %v", err)
	}
	_, _ = srv.Handle("ping", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	if _, err := inner.srv.(*captureServer).last(kernel.WithPrincipal(context.Background(), "rpc:cli"), nil); err != nil {
		t.Fatalf("handler 失败: %v", err)
	}
	if payload == nil || payload["direction"] != "in" || payload["peer"] != "cli" || payload["method"] != "ping" {
		t.Fatalf("入站事件不正确: %+v", payload)
	}
}

type captureService struct{ srv seamrpc.Server }

func (c *captureService) Dial(context.Context, seamrpc.Address, seamrpc.DialOptions) (seamrpc.Peer, error) {
	return fakePeer{}, nil
}
func (c *captureService) Listen(context.Context, seamrpc.Address, seamrpc.ServerOptions) (seamrpc.Server, error) {
	return c.srv, nil
}

type captureServer struct{ last seamrpc.Handler }

func (c *captureServer) Handle(_ string, h seamrpc.Handler) (kernel.Dispose, error) {
	c.last = h
	return nil, nil
}
func (c *captureServer) Publish(string, any) error   { return nil }
func (c *captureServer) Serve(context.Context) error { return nil }
func (c *captureServer) Stats() seamrpc.Stats        { return seamrpc.Stats{} }
func (c *captureServer) Close() error                { return nil }
