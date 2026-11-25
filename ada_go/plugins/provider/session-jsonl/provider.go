package jsonl

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

type config struct {
	Dir  string `json:"dir"`
	Sync *bool  `json:"sync,omitempty"`
}

// Provider 是会话工厂:持有后端 Store 与已打开的会话。
type Provider struct {
	store       Store
	projections *session.ProjectionRegistry

	mu       sync.Mutex
	sessions map[string]*Session
	seq      atomic.Uint64
}

// New 用给定存储后端构造 provider;后端可替换为内存、SQLite 等实现。
func New(store Store) *Provider {
	return &Provider{store: store, sessions: make(map[string]*Session)}
}

// Name 返回 provider 名。
func (p *Provider) Name() string { return ProviderName }

// BindProjections 由 session.Registry 在注册时调用,把投影表注入 provider。
func (p *Provider) BindProjections(r *session.ProjectionRegistry) {
	p.projections = r
}

// Version 返回 session 契约主版本。
func (p *Provider) Version() int { return 1 }

// Open 创建一个新会话,id 由 provider 生成。
func (p *Provider) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return p.add(ctx, p.newID(), meta, true)
}

// OpenID 打开指定 id 的会话;已存在则恢复,不存在则创建。
//
// 它是 provider 扩展,不属于 session.Seam 的最小接口;调用方需要恢复旧会话时使用。
func (p *Provider) OpenID(ctx context.Context, id string, meta session.Meta) (session.Session, error) {
	return p.add(ctx, id, meta, false)
}

func (p *Provider) add(ctx context.Context, id string, meta session.Meta, create bool) (session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "打开会话前已取消", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.sessions[id]; ok {
		return nil, seamerr.New(seamerr.ErrInvalid, ProviderName, "会话已打开: "+id, false)
	}
	var (
		j      Journal
		stored session.Meta
		err    error
	)
	if create {
		j, err = p.store.Create(id, meta)
		stored = meta
	} else {
		j, stored, err = p.store.Open(id, meta)
	}
	if err != nil {
		return nil, err
	}
	s, err := newSession(p, id, stored, j, p.projections)
	if err != nil {
		_ = j.Close()
		return nil, err
	}
	p.sessions[id] = s
	return s, nil
}

func (p *Provider) fork(parent *Session, at uint64) (session.Session, kernel.Dispose, error) {
	events := parent.snapshot(at)
	id := p.newID()
	meta := parent.meta
	meta.Parent = parent.id
	j, err := p.store.Fork(events, id, meta)
	if err != nil {
		return nil, nil, err
	}
	s, err := newSession(p, id, meta, j, p.projections)
	if err != nil {
		_ = j.Close()
		return nil, nil, err
	}
	p.mu.Lock()
	p.sessions[id] = s
	p.mu.Unlock()
	return s, func(context.Context) error { return nil }, nil
}

func (p *Provider) newID() string {
	return fmt.Sprintf("sess-%d-%d", time.Now().UTC().UnixNano(), p.seq.Add(1))
}

func (p *Provider) forget(id string, s *Session) {
	p.mu.Lock()
	if cur, ok := p.sessions[id]; ok && cur == s {
		delete(p.sessions, id)
	}
	p.mu.Unlock()
}

// CloseAll 关闭所有已打开会话(插件卸载/测试清理用)。
func (p *Provider) CloseAll() {
	p.mu.Lock()
	list := make([]*Session, 0, len(p.sessions))
	for _, s := range p.sessions {
		list = append(list, s)
	}
	p.mu.Unlock()
	for _, s := range list {
		_ = s.Close()
	}
}

// Plugin 返回 session-jsonl 插件:inject session seam,注册文件型 provider。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		var c config
		if err := cfg.Decode(&c); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "session-jsonl: 解析配置失败: %v", err)
		}
		if c.Dir == "" {
			return nil, kernel.Fail(kernel.CodeInvalid, "session-jsonl: dir 不能为空")
		}
		syncWrites := true
		if c.Sync != nil {
			syncWrites = *c.Sync
		}
		registry, err := kernel.Service[*session.Registry](ctx, "session")
		if err != nil {
			return nil, err
		}
		p := New(NewFileStore(c.Dir, syncWrites))
		dispose, err := registry.Register(p)
		if err != nil {
			return nil, err
		}
		return func(dctx context.Context) error {
			p.CloseAll()
			return dispose(dctx)
		}, nil
	}).WithInject("session")
}
