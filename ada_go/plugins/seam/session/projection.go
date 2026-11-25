package session

import (
	"context"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProjectionTurnStats 是默认轮次统计投影的稳定名字。
const ProjectionTurnStats = "turn-stats"

// Projection 是会话派生状态的纯函数。
//
// 约束:
//   - Apply 必须同步、无副作用;changed=false 表示 state 未变化;
//   - Version 变化时缓存必须整体重放;
//   - View 只读取投影状态,不得推进状态。
type Projection interface {
	Name() string
	Version() int
	Apply(state any, ev Event) (next any, changed bool)
	View(state any) any
}

// ProjectionRegistry 是投影注册面与 (session, projection) 缓存。
//
// 缓存按 session id + projection 名隔离;每次 Project 只重放上次 seq 之后的事件。
type ProjectionRegistry struct {
	mu          sync.Mutex
	projections map[string]Projection
	cache       map[string]*projectionCache
}

type projectionCache struct {
	name    string
	version int
	last    uint64
	state   any
}

// NewProjectionRegistry 构造空投影注册表。
func NewProjectionRegistry() *ProjectionRegistry {
	return &ProjectionRegistry{
		projections: make(map[string]Projection),
		cache:       make(map[string]*projectionCache),
	}
}

// Register 注册投影;同名重复注册返回 EINVALID。
func (r *ProjectionRegistry) Register(p Projection) (kernel.Dispose, error) {
	if p == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "projection 不能为 nil", false)
	}
	name := p.Name()
	if name == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "projection 的 Name 不能为空", false)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.projections[name]; ok {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "同名 projection 已注册: "+name, false)
	}
	r.projections[name] = p
	return func(context.Context) error {
		r.mu.Lock()
		delete(r.projections, name)
		for key, c := range r.cache {
			if c.name == name {
				delete(r.cache, key)
			}
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// Project 对已注册投影重放缓存尾部并返回当前视图。
func (r *ProjectionRegistry) Project(s Session, name string) (any, error) {
	if name == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "projection 名不能为空", false)
	}
	r.mu.Lock()
	_, ok := r.projections[name]
	r.mu.Unlock()
	if !ok {
		return nil, seamerr.Unimplemented("session", "projection 未注册: "+name)
	}

	events, err := s.Events(0, 0)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// 注册/注销可能在读事件期间发生,拿到锁后重新确认。
	p, ok := r.projections[name]
	if !ok {
		return nil, seamerr.Unimplemented("session", "projection 未注册: "+name)
	}
	key := s.ID() + "\x00" + name
	c := r.cache[key]
	if c == nil || c.version != p.Version() {
		c = &projectionCache{name: name, version: p.Version()}
		r.cache[key] = c
	}
	state := c.state
	for _, ev := range events {
		if ev.Seq <= c.last {
			continue
		}
		next, changed := p.Apply(state, ev)
		if changed {
			state = next
		}
		c.last = ev.Seq
	}
	c.state = state
	return p.View(state), nil
}
