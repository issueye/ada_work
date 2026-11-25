package rpc

import (
	"context"
	"errors"
	"testing"

	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Address
		err  bool
	}{
		{"tcp://127.0.0.1:7331", Address{TCP, "127.0.0.1:7331"}, false},
		{"tcp://[::1]:7331", Address{TCP, "[::1]:7331"}, false},
		{"unix:///run/ada/agent.sock", Address{Unix, "/run/ada/agent.sock"}, false},
		{"pipe://ada-agent", Address{Pipe, "ada-agent"}, false},
		{"tcp://127.0.0.1", Address{}, true},
		{"unix://relative.sock", Address{}, true},
		{"pipe://../x", Address{}, true},
		{"http://127.0.0.1:1", Address{}, true},
		{"nonsense", Address{}, true},
		{"tcp://127.0.0.1:99999", Address{}, true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("%q 应失败", c.in)
			}
			if !seamerr.Is(err, seamerr.ErrInvalid) {
				t.Fatalf("%q 错误码不对: %v", c.in, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q 解析失败: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q => %+v,期望 %+v", c.in, got, c.want)
		}
		if got.String() != c.in {
			t.Fatalf("%q => %q(往返不一致)", c.in, got.String())
		}
	}
}

type fakeService struct{ name string }

func (f fakeService) Dial(context.Context, Address, DialOptions) (Peer, error) {
	return nil, Unimplemented(f.name)
}
func (f fakeService) Listen(context.Context, Address, ServerOptions) (Server, error) {
	return nil, Unimplemented(f.name)
}

func TestChainOrder(t *testing.T) {
	c := NewChain()
	if _, err := c.Get(); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("空链应未实现: %v", err)
	}
	if _, err := c.Install(fakeService{"base"}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := c.Install(fakeService{"again"}); err == nil {
		t.Fatalf("重复安装应失败")
	}
	var order []int
	for _, p := range []int{10, 30, 20} {
		p := p
		if _, err := c.Decorate(func(s Service) Service {
			order = append(order, p)
			return wrapService{s}
		}, p); err != nil {
			t.Fatalf("装饰失败: %v", err)
		}
	}
	if _, err := c.Get(); err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	// priority 小在内层:10 → 20 → 30 依次包裹。
	want := []int{10, 20, 30}
	if len(order) != len(want) {
		t.Fatalf("装饰顺序 %v,期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("装饰顺序 %v,期望 %v", order, want)
		}
	}
	if got := c.Applied(); len(got) != 3 || got[0] != 30 {
		t.Fatalf("Applied=%v", got)
	}
}

type wrapService struct{ inner Service }

func (w wrapService) Dial(ctx context.Context, a Address, o DialOptions) (Peer, error) {
	return w.inner.Dial(ctx, a, o)
}
func (w wrapService) Listen(ctx context.Context, a Address, o ServerOptions) (Server, error) {
	return w.inner.Listen(ctx, a, o)
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup(TCP); ok {
		t.Fatalf("空表不应有 tcp")
	}
	if _, err := r.Register("", fakeTransport{}); !seamerr.Is(err, seamerr.ErrInvalid) {
		t.Fatalf("空类型应拒绝: %v", err)
	}
	d, err := r.Register(TCP, fakeTransport{})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if _, ok := r.Lookup(TCP); !ok {
		t.Fatalf("注册后应可查到")
	}
	if _, err := r.Register(TCP, fakeTransport{}); err == nil {
		t.Fatalf("重复注册应失败")
	}
	if err := d(context.Background()); err != nil {
		t.Fatalf("反注册失败: %v", err)
	}
	if _, ok := r.Lookup(TCP); ok {
		t.Fatalf("反注册后仍可见")
	}
	ns := r.Networks()
	if len(ns) != 0 {
		t.Fatalf("Networks=%v", ns)
	}
}

type fakeTransport struct{}

func (fakeTransport) Dial(context.Context, Address) (Stream, error) {
	return nil, errors.New("no")
}
func (fakeTransport) Listen(context.Context, Address, ListenOptions) (Listener, error) {
	return nil, errors.New("no")
}
