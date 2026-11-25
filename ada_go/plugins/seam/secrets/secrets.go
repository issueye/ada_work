// Package secrets 是 `seam/secrets` 的契约层:按名读取 secret(不落日志、不进 config)。
//
// 契约来源:plugin-kernel-design §13(capability `secrets.<name>`)。实现由 provider 提供,
// 授权/审计由 policy 装饰器负责。
package secrets

import (
	"context"
	"regexp"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// nameRe 限制 secret 名:大写字母/数字/下划线,最长 64。
var nameRe = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

// Getter 是 secret 读取契约。
type Getter interface {
	Get(ctx context.Context, name string) (string, error)
}

type decoration struct {
	id       uint64
	fn       func(Getter) Getter
	priority int
}

// Chain 是装饰器链:provider Install 底座,policy Decorate 叠加,consumer Get。
type Chain struct {
	mu    sync.Mutex
	base  Getter
	items []decoration
	seq   uint64
}

// Install 安装底座实现;重复安装报错。
func (c *Chain) Install(base Getter) (kernel.Dispose, error) {
	if base == nil {
		return nil, Invalid("secrets: 底座实现不能为 nil")
	}
	c.mu.Lock()
	if c.base != nil {
		c.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, "secrets", "secrets 底座已安装", false)
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
func (c *Chain) Decorate(fn func(Getter) Getter, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, Invalid("secrets: 装饰器不能为 nil")
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

// Get 组装并返回最终 Getter;没有底座时报未实现。
func (c *Chain) Get() (Getter, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return nil, seamerr.New(seamerr.ErrUnimplemented, "secrets", "没有安装 secrets 实现", false)
	}
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].priority < items[j].priority
	})
	out := c.base
	for _, it := range items {
		out = it.fn(out)
	}
	return out, nil
}

// Applied 返回当前装饰器 id(诊断用)。
func (c *Chain) Applied() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, 0, len(c.items))
	for _, it := range c.items {
		out = append(out, it.id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidateName 校验 secret 名。
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return Invalid("secret 名非法(只允许大写字母/数字/下划线,最长 64): " + name)
	}
	return nil
}

// Invalid 构造参数错误。
func Invalid(msg string) error {
	return seamerr.New(seamerr.ErrInvalid, "secrets", msg, false)
}

// NotFound 构造「未配置」错误。
func NotFound(name string) error {
	return seamerr.New(seamerr.ErrInvalid, "secrets", "secret 未配置: "+name, false)
}

// Denied 构造拒绝错误。
func Denied(reason string) error {
	return seamerr.New(seamerr.ErrDenied, "secrets", reason, false)
}

const pluginID = "seam-secrets"

// Plugin 返回 seam/secrets 插件:只提供 secrets 服务(Chain)。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("secrets", NewChain(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("secrets")
}

// NewChain 构造空链。
func NewChain() *Chain { return &Chain{} }
