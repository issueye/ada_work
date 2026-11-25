// Package rpcobserve 是 RPC 的观察策略:成功/失败都发 rpc/observe,不阻断。
//
// payload 不含 params/result/token;error 只保留错误文本。
package rpcobserve

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// PolicyName 是插件 id。
const PolicyName = "rpc-observe"

// Priority 高于 rpc-guard(200),能看到拒绝结果。
const Priority = 250

// New 构造观察装饰器。
func New(inner seamrpc.Service, emit func(string, any)) seamrpc.Service {
	if emit == nil {
		emit = func(string, any) {}
	}
	return &observer{inner: inner, emit: emit}
}

type observer struct {
	inner seamrpc.Service
	emit  func(string, any)
}

func (o *observer) observe(ctx context.Context, direction string, addr seamrpc.Address, peer, method string, start time.Time, err error) {
	payload := map[string]any{
		"direction": direction, "transport": string(addr.Network),
		"peer": peer, "method": method, "durationMs": time.Since(start).Milliseconds(),
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	if id, ok := kernel.Principal(ctx); ok {
		payload["plugin"] = id
	}
	o.emit(seamrpc.EventObserve, payload)
}

func (o *observer) Dial(ctx context.Context, addr seamrpc.Address, opts seamrpc.DialOptions) (seamrpc.Peer, error) {
	start := time.Now()
	peer, err := o.inner.Dial(ctx, addr, opts)
	if err != nil {
		o.observe(ctx, "out", addr, "", "dial", start, err)
		return nil, err
	}
	o.observe(ctx, "out", addr, peer.Remote(), "dial", start, nil)
	return &peerObserver{Peer: peer, o: o, addr: addr}, nil
}

func (o *observer) Listen(ctx context.Context, addr seamrpc.Address, opts seamrpc.ServerOptions) (seamrpc.Server, error) {
	start := time.Now()
	srv, err := o.inner.Listen(ctx, addr, opts)
	if err != nil {
		o.observe(ctx, "in", addr, "", "listen", start, err)
		return nil, err
	}
	o.observe(ctx, "in", addr, "", "listen", start, nil)
	return &serverObserver{Server: srv, o: o, addr: addr}, nil
}

type peerObserver struct {
	seamrpc.Peer
	o    *observer
	addr seamrpc.Address
}

func (p *peerObserver) Call(ctx context.Context, method string, params, result any) error {
	start := time.Now()
	err := p.Peer.Call(ctx, method, params, result)
	p.o.observe(ctx, "out", p.addr, p.Remote(), method, start, err)
	return err
}

func (p *peerObserver) Notify(ctx context.Context, method string, params any) error {
	start := time.Now()
	err := p.Peer.Notify(ctx, method, params)
	p.o.observe(ctx, "out", p.addr, p.Remote(), method, start, err)
	return err
}

func (p *peerObserver) Subscribe(ctx context.Context, topics ...string) error {
	start := time.Now()
	err := p.Peer.Subscribe(ctx, topics...)
	p.o.observe(ctx, "out", p.addr, p.Remote(), "subscribe:"+strings.Join(topics, ","), start, err)
	return err
}

type serverObserver struct {
	seamrpc.Server
	o    *observer
	addr seamrpc.Address
}

func (s *serverObserver) Handle(method string, h seamrpc.Handler) (kernel.Dispose, error) {
	wrapped := func(ctx context.Context, params json.RawMessage) (any, error) {
		start := time.Now()
		result, err := h(ctx, params)
		s.o.observe(ctx, "in", s.addr, peerFrom(ctx), method, start, err)
		return result, err
	}
	return s.Server.Handle(method, wrapped)
}

func peerFrom(ctx context.Context) string {
	id, _ := kernel.Principal(ctx)
	return strings.TrimPrefix(id, "rpc:")
}

// Plugin 返回 rpc-observe 插件:inject rpc。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamrpc.Chain](ctx, "rpc")
		if err != nil {
			return nil, err
		}
		return chain.Decorate(func(inner seamrpc.Service) seamrpc.Service {
			return New(inner, func(event string, payload any) { ctx.Emit(event, payload) })
		}, Priority)
	}).WithInject("rpc")
}
