// Package memory 是 session seam 的内存 provider:append-only 事件日志、
// fork 复制历史、订阅推送、压缩摘要。
//
// 它不持久化——持久化是别的 provider 的职责(seam-api §4.5);
// 内存实现的价值是「零依赖、可被契约测试覆盖」。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// ProviderName 同时是 provider 名与插件 id(kebab-case)。
const ProviderName = "session-memory"

// Provider 是会话工厂:每个 Open 产生一个相互独立的会话。
type Provider struct {
	mu          sync.Mutex
	sessions    map[string]*memorySession
	seq         atomic.Uint64
	projections *session.ProjectionRegistry
}

// New 构造内存 provider。
func New() *Provider {
	return &Provider{sessions: make(map[string]*memorySession)}
}

// Name 返回 provider 名。
func (p *Provider) Name() string { return ProviderName }

// BindProjections 由 session.Registry 在注册时调用,把投影表注入 provider。
func (p *Provider) BindProjections(r *session.ProjectionRegistry) {
	p.projections = r
}

// Version 返回契约主版本。
func (p *Provider) Version() int { return 1 }

// Open 打开一个新会话。
func (p *Provider) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "打开会话前已取消", err)
	}
	id := fmt.Sprintf("mem-%d", p.seq.Add(1))
	s := &memorySession{
		owner:       p,
		id:          id,
		meta:        meta,
		subs:        make(map[chan session.Event]struct{}),
		projections: p.projections,
	}
	p.mu.Lock()
	p.sessions[id] = s
	p.mu.Unlock()
	return s, nil
}

// Sessions 返回当前打开的会话数(诊断用)。
func (p *Provider) Sessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}

type memorySession struct {
	owner       *Provider
	id          string
	meta        session.Meta
	projections *session.ProjectionRegistry

	mu          sync.Mutex
	events      []session.Event
	subs        map[chan session.Event]struct{}
	turn        session.TurnInfo
	compactedTo uint64
}

// ID 返回会话 id。
func (s *memorySession) ID() string { return s.id }

// Meta 返回会话元信息(值拷贝,调用方改它不影响会话)。
func (s *memorySession) Meta() session.Meta { return s.meta }

// Append 追加事件。整批先校验再写入:不允许「写了一半才报错」。
func (s *memorySession) Append(events ...session.Event) ([]uint64, error) {
	if len(events) == 0 {
		return nil, nil
	}
	for i, ev := range events {
		if ev.Type == "" {
			return nil, seamerr.New(seamerr.ErrInvalid, ProviderName, fmt.Sprintf("第 %d 条事件的 type 为空", i), false)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	seqs := make([]uint64, 0, len(events))
	for _, ev := range events {
		ev.Seq = uint64(len(s.events)) + 1
		if ev.TS.IsZero() {
			ev.TS = time.Now().UTC()
		}
		ev.SessionID = s.id
		if ev.Source == "" {
			ev.Source = "core"
		}
		s.trackTurn(&ev)
		s.events = append(s.events, ev)
		seqs = append(seqs, ev.Seq)
		notify(s.subs, ev)
	}
	return seqs, nil
}

// Events 返回 seq >= from 的事件;from=0 从头,limit<=0 不限。
func (s *memorySession) Events(from uint64, limit int) ([]session.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if from > 0 {
		for start < len(s.events) && s.events[start].Seq < from {
			start++
		}
	}
	end := len(s.events)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	out := make([]session.Event, end-start)
	copy(out, s.events[start:end])
	return out, nil
}

// Subscribe 先回放既有事件,再推送新事件。
//
// 通道是带缓冲的,订阅者不读就丢:会话的权威来源是日志,不是通道;
// 为了一个慢订阅者阻塞写方会违反 append-only 的可用性。
func (s *memorySession) Subscribe(from uint64) (<-chan session.Event, kernel.Dispose) {
	ch := make(chan session.Event, 256)
	s.mu.Lock()
	for _, ev := range s.events {
		if ev.Seq < from {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	s.subs[ch] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	dispose := func(context.Context) error {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, ch)
			s.mu.Unlock()
		})
		return nil
	}
	return ch, dispose
}

// Turn 返回当前轮次状态。
func (s *memorySession) Turn() session.TurnInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// Fork 复制 at 为止的历史为新会话(新会话 seq 从 1 重新编号)。
func (s *memorySession) Fork(at uint64) (session.Session, kernel.Dispose, error) {
	s.mu.Lock()
	limit := at
	if limit == 0 || limit > uint64(len(s.events)) {
		limit = uint64(len(s.events))
	}
	s.mu.Unlock()

	id := fmt.Sprintf("%s-fork-%d", s.id, s.owner.seq.Add(1))
	child := &memorySession{
		owner:       s.owner,
		id:          id,
		meta:        s.meta,
		subs:        make(map[chan session.Event]struct{}),
		projections: s.projections,
	}
	child.meta.Parent = s.id

	s.mu.Lock()
	copied := make([]session.Event, 0, limit)
	for _, ev := range s.events {
		if ev.Seq > limit {
			break
		}
		ev.Seq = uint64(len(copied)) + 1
		ev.SessionID = id
		copied = append(copied, ev)
	}
	s.mu.Unlock()

	child.mu.Lock()
	child.events = copied
	child.mu.Unlock()

	s.owner.mu.Lock()
	s.owner.sessions[id] = child
	s.owner.mu.Unlock()

	return child, func(context.Context) error { return nil }, nil
}

// Project 返回命名投影的当前视图。
func (s *memorySession) Project(name string) (any, error) {
	if s.projections == nil {
		return nil, seamerr.Unimplemented(ProviderName, "未绑定投影注册表")
	}
	return s.projections.Project(s, name)
}

// Compact 标记 upTo 之前的范围已压缩,并追加一条摘要事件。
func (s *memorySession) Compact(ctx context.Context, upTo uint64, summary session.Event) error {
	if err := ctx.Err(); err != nil {
		return seamerr.Wrap(seamerr.ErrCanceled, ProviderName, "压缩前已取消", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if upTo == 0 || upTo > uint64(len(s.events)) {
		return seamerr.New(seamerr.ErrInvalid, ProviderName,
			fmt.Sprintf("upTo=%d 超出范围(1..%d)", upTo, len(s.events)), false)
	}
	if summary.Type == "" {
		summary.Type = session.TypeCompactSummary
	}
	summary.Seq = uint64(len(s.events)) + 1
	if summary.TS.IsZero() {
		summary.TS = time.Now().UTC()
	}
	summary.SessionID = s.id
	summary.Source = "core"
	summary.Ignorable = false
	if len(summary.Data) == 0 {
		summary.Data = json.RawMessage(fmt.Sprintf(`{"upTo":%d}`, upTo))
	}
	s.events = append(s.events, summary)
	s.compactedTo = upTo
	notify(s.subs, summary)
	return nil
}

// trackTurn 从 turn/start 与 turn/end 的事件载荷里维护当前轮次。
func (s *memorySession) trackTurn(ev *session.Event) {
	if ev.Type != session.TypeTurnStart && ev.Type != session.TypeTurnEnd {
		return
	}
	var info session.TurnInfo
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &info)
	}
	switch ev.Type {
	case session.TypeTurnStart:
		if info.ID == "" {
			info.ID = fmt.Sprintf("turn-%d", ev.Seq)
		}
		info.State = session.TurnRunning
		if info.Started.IsZero() {
			info.Started = ev.TS
		}
		s.turn = info
	case session.TypeTurnEnd:
		if info.ID == "" {
			info.ID = s.turn.ID
		}
		if info.Index == 0 {
			info.Index = s.turn.Index
		}
		if info.State == "" {
			info.State = session.TurnDone
		}
		if info.Started.IsZero() {
			info.Started = s.turn.Started
		}
		s.turn = info
	}
	if ev.TurnID == "" {
		ev.TurnID = s.turn.ID
	}
}

// notify 非阻塞推送:慢订阅者丢事件,不能阻塞写方。
func notify(subs map[chan session.Event]struct{}, ev session.Event) {
	for ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Plugin 返回 session-memory 插件:inject session seam,把内存 provider 注册进去。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*session.Registry](ctx, "session")
		if err != nil {
			return nil, err
		}
		return registry.Register(New())
	}).WithInject("session")
}
