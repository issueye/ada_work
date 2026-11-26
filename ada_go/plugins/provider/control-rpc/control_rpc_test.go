package controlrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/provider/control-rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

type fakePeer struct {
	method string
	params json.RawMessage
	result any
	err    error
}

func (p *fakePeer) Call(_ context.Context, method string, params, result any) error {
	p.method = method
	switch v := params.(type) {
	case json.RawMessage:
		p.params = append(json.RawMessage(nil), v...)
	default:
		b, _ := json.Marshal(v)
		p.params = b
	}
	if p.err != nil {
		return p.err
	}
	b, err := json.Marshal(p.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}
func (p *fakePeer) Notify(context.Context, string, any) error  { return nil }
func (p *fakePeer) Subscribe(context.Context, ...string) error { return nil }
func (p *fakePeer) Events() <-chan seamrpc.Event               { return nil }
func (p *fakePeer) Remote() string                             { return "srv" }
func (p *fakePeer) Close() error                               { return nil }

type fakeInvoker struct {
	method string
	params json.RawMessage
	out    json.RawMessage
	err    error
}

func (f *fakeInvoker) Invoke(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	f.method, f.params = method, append(json.RawMessage(nil), params...)
	return f.out, f.err
}

type fakeServer struct {
	handlers map[string]seamrpc.Handler
}

func (s *fakeServer) Handle(method string, h seamrpc.Handler) (kernel.Dispose, error) {
	if s.handlers == nil {
		s.handlers = make(map[string]seamrpc.Handler)
	}
	s.handlers[method] = h
	return func(context.Context) error { delete(s.handlers, method); return nil }, nil
}
func (s *fakeServer) Publish(string, any) error   { return nil }
func (s *fakeServer) Serve(context.Context) error { return nil }
func (s *fakeServer) Stats() seamrpc.Stats        { return seamrpc.Stats{} }
func (s *fakeServer) Close() error                { return nil }

func TestClientMapsCall(t *testing.T) {
	peer := &fakePeer{result: map[string]string{"id": "s1"}}
	c := controlrpc.New(peer)
	out, err := c.Invoke(context.Background(), control.MethodSessionCreate, json.RawMessage(`{"title":"demo"}`))
	if err != nil || string(out) != `{"id":"s1"}` {
		t.Fatalf("client invoke 不符: %s err=%v", out, err)
	}
	if peer.method != control.MethodSessionCreate || string(peer.params) != `{"title":"demo"}` {
		t.Fatalf("method/params 不符: %s %s", peer.method, peer.params)
	}
}

func TestClientPropagatesError(t *testing.T) {
	c := controlrpc.New(&fakePeer{err: errors.New("boom")})
	if _, err := c.Invoke(context.Background(), control.MethodSessionList, nil); err == nil {
		t.Fatal("应传播 peer 错误")
	}
}

func TestRegisterHandlers(t *testing.T) {
	srv := &fakeServer{}
	inv := &fakeInvoker{out: json.RawMessage(`{"id":"s1"}`)}
	disposers, err := controlrpc.Register(srv, inv)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if len(disposers) != len(controlrpc.Methods()) {
		t.Fatalf("注册数量不符: %d", len(disposers))
	}
	h := srv.handlers[control.MethodSessionCreate]
	if h == nil {
		t.Fatal("session.create 未注册")
	}
	out, err := h(context.Background(), json.RawMessage(`{"title":"demo"}`))
	if err != nil {
		t.Fatalf("handler 失败: %v", err)
	}
	raw, ok := out.(json.RawMessage)
	if !ok || string(raw) != `{"id":"s1"}` {
		t.Fatalf("handler 结果不符: %#v", out)
	}
	if inv.method != control.MethodSessionCreate {
		t.Fatalf("invoker 方法不符: %s", inv.method)
	}
	for _, d := range disposers {
		_ = d(context.Background())
	}
	if len(srv.handlers) != 0 {
		t.Fatalf("注销后仍有处理函数: %d", len(srv.handlers))
	}
}
