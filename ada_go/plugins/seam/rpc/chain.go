// Chain 与 Registry 是 seam/rpc 的无 I/O 编排部分,与 seam/fs 的 Chain 同构。
package rpc

import (
	"context"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type decoration struct {
	id       uint64
	fn       func(Service) Service
	priority int
}

// Chain 是装饰器链:provider Install 底座,policy Decorate 叠加,consumer Get。
type Chain struct {
	mu    sync.Mutex
	base  Service
	items []decoration
	seq   uint64
}

// NewChain 构造空链。
func NewChain() *Chain { return &Chain{} }

// Install 安装协议底座;重复安装报错。
func (c *Chain) Install(base Service) (kernel.Dispose, error) {
	if base == nil {
		return nil, Invalid("rpc: 底座实现不能为 nil")
	}
	c.mu.Lock()
	if c.base != nil {
		c.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, "rpc", "rpc 底座已安装", false)
	}
	c.base = base
	c.mu.Unlock()
	return func(context.Context) error {
		c.mu.Lock()
		c.base = nil
		c.items = nil
		c.mu.Unlock()
		return nil
	}, nil
}

// Decorate 叠加装饰器;priority 越大越靠外。
func (c *Chain) Decorate(fn func(Service) Service, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, Invalid("rpc: 装饰器不能为 nil")
	}
	c.mu.Lock()
	c.seq++
	d := decoration{id: c.seq, fn: fn, priority: priority}
	c.items = append(c.items, d)
	c.mu.Unlock()
	return func(context.Context) error {
		c.mu.Lock()
		kept := c.items[:0]
		for _, it := range c.items {
			if it.id != d.id {
				kept = append(kept, it)
			}
		}
		c.items = kept
		c.mu.Unlock()
		return nil
	}, nil
}

// Get 组装并返回最终 Service;没有底座时报未实现。
func (c *Chain) Get() (Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return nil, seamerr.Unimplemented("rpc", "没有安装 rpc 协议实现")
	}
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool { return items[i].priority < items[j].priority })
	out := c.base
	for _, it := range items {
		out = it.fn(out)
	}
	return out, nil
}

// Applied 返回当前装饰器优先级(外层到内层,诊断用)。
func (c *Chain) Applied() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, 0, len(c.items))
	for _, it := range c.items {
		out = append(out, it.priority)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// Registry 是传输注册表:每个 Network 只能注册一次。
type Registry struct {
	mu sync.RWMutex
	m  map[Network]Transport
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry { return &Registry{m: make(map[Network]Transport)} }

// Register 注册一个传输;Dispose 反注册。
func (r *Registry) Register(n Network, t Transport) (kernel.Dispose, error) {
	if t == nil {
		return nil, Invalid("rpc: 传输实现不能为 nil")
	}
	if n == "" {
		return nil, Invalid("rpc: 传输类型不能为空")
	}
	r.mu.Lock()
	if _, ok := r.m[n]; ok {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, "rpc", "传输已注册: "+string(n), false)
	}
	r.m[n] = t
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		delete(r.m, n)
		r.mu.Unlock()
		return nil
	}, nil
}

// Lookup 按传输类型取实现。
func (r *Registry) Lookup(n Network) (Transport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.m[n]
	return t, ok
}

// Networks 返回已注册的传输类型(排序,诊断用)。
func (r *Registry) Networks() []Network {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Network, 0, len(r.m))
	for n := range r.m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
