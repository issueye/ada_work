package jsonl

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// Session 是 jsonl provider 的会话句柄。
//
// 并发模型:
//   - 所有写操作(append/compact)进入本会话的 actor 队列,由同一个 goroutine 落盘;
//   - 不同 Session 各有自己的 actor,不共享锁;
//   - 读(turn/events/subscribe)走 RWMutex,不经过 actor,不阻塞写入。
type Session struct {
	owner       *Provider
	id          string
	meta        session.Meta
	journal     Journal
	projections *session.ProjectionRegistry

	cmds    chan any
	closeCh chan struct{}
	done    chan struct{}
	once    sync.Once

	mu          sync.RWMutex
	events      []session.Event
	turn        session.TurnInfo
	subs        map[chan session.Event]struct{}
	compactedTo uint64
	closed      bool
}

type appendCmd struct {
	events []session.Event
	ack    chan appendResult
}

type appendResult struct {
	seqs []uint64
	err  error
}

type compactCmd struct {
	ctx     context.Context
	upTo    uint64
	summary session.Event
	ack     chan error
}

func newSession(owner *Provider, id string, meta session.Meta, j Journal, projections *session.ProjectionRegistry) (*Session, error) {
	events, err := j.ReadAll()
	if err != nil {
		return nil, err
	}
	s := &Session{
		owner:       owner,
		id:          id,
		meta:        meta,
		journal:     j,
		projections: projections,
		cmds:        make(chan any, 64),
		closeCh:     make(chan struct{}),
		done:        make(chan struct{}),
		subs:        make(map[chan session.Event]struct{}),
		events:      events,
	}
	for i := range events {
		s.restore(&events[i])
	}
	go s.loop()
	return s, nil
}

// ID 返回会话 id。
func (s *Session) ID() string { return s.id }

// Meta 返回会话元信息(值拷贝)。
func (s *Session) Meta() session.Meta { return s.meta }

// Append 把事件交给本会话 actor,由 actor 分配 seq 并落盘。
func (s *Session) Append(events ...session.Event) ([]uint64, error) {
	if len(events) == 0 {
		return nil, nil
	}
	cmd := &appendCmd{events: events, ack: make(chan appendResult, 1)}
	select {
	case s.cmds <- cmd:
	case <-s.done:
		return nil, errClosed()
	}
	select {
	case res := <-cmd.ack:
		return res.seqs, res.err
	case <-s.done:
		return nil, errClosed()
	}
}

// Events 返回 seq >= from 的事件副本;limit<=0 不限。
func (s *Session) Events(from uint64, limit int) ([]session.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	start := 0
	if from > 0 {
		start = sort.Search(len(s.events), func(i int) bool { return s.events[i].Seq >= from })
	}
	end := len(s.events)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	out := make([]session.Event, end-start)
	copy(out, s.events[start:end])
	return out, nil
}

// Subscribe 先回放既有事件,再推送新事件;慢订阅者丢事件。
func (s *Session) Subscribe(from uint64) (<-chan session.Event, kernel.Dispose) {
	ch := make(chan session.Event, 256)
	s.mu.Lock()
	if s.closed {
		close(ch)
		s.mu.Unlock()
		return ch, func(context.Context) error { return nil }
	}
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
func (s *Session) Turn() session.TurnInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turn
}

// Fork 复制 at 为止的历史为子会话。
func (s *Session) Fork(at uint64) (session.Session, kernel.Dispose, error) {
	return s.owner.fork(s, at)
}

// Project 返回命名投影的当前视图。
func (s *Session) Project(name string) (any, error) {
	if s.projections == nil {
		return nil, seamerr.Unimplemented(ProviderName, "未绑定投影注册表")
	}
	return s.projections.Project(s, name)
}

// Compact 把 upTo 之前的范围标记为已压缩,并追加摘要事件。
func (s *Session) Compact(ctx context.Context, upTo uint64, summary session.Event) error {
	cmd := &compactCmd{ctx: ctx, upTo: upTo, summary: summary, ack: make(chan error, 1)}
	select {
	case s.cmds <- cmd:
	case <-s.done:
		return errClosed()
	}
	select {
	case err := <-cmd.ack:
		return err
	case <-s.done:
		return errClosed()
	}
}

// Close 停止 actor 并关闭文件句柄;可重复调用。
func (s *Session) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.closeCh)
	})
	<-s.done
	s.owner.forget(s.id, s)
	return nil
}

func (s *Session) loop() {
	defer close(s.done)
	for {
		select {
		case raw := <-s.cmds:
			switch cmd := raw.(type) {
			case *appendCmd:
				s.doAppend(cmd)
			case *compactCmd:
				s.doCompact(cmd)
			}
		case <-s.closeCh:
			_ = s.journal.Close()
			return
		}
	}
}

func (s *Session) doAppend(cmd *appendCmd) {
	events, err := s.journal.Append(cmd.events)
	if err != nil {
		cmd.ack <- appendResult{err: err}
		return
	}
	s.mu.Lock()
	for i := range events {
		ev := events[i]
		s.events = append(s.events, ev)
		s.restore(&ev)
		notify(s.subs, ev)
	}
	s.mu.Unlock()
	seqs := make([]uint64, len(events))
	for i := range events {
		seqs[i] = events[i].Seq
	}
	cmd.ack <- appendResult{seqs: seqs}
}

func (s *Session) doCompact(cmd *compactCmd) {
	if cmd.upTo == 0 || cmd.upTo > uint64(len(s.events)) {
		cmd.ack <- seamerr.New(seamerr.ErrInvalid, ProviderName,
			fmt.Sprintf("upTo=%d 超出范围(1..%d)", cmd.upTo, len(s.events)), false)
		return
	}
	summary, err := s.journal.Compact(cmd.ctx, cmd.upTo, cmd.summary)
	if err != nil {
		cmd.ack <- err
		return
	}
	s.mu.Lock()
	s.events = append(s.events, summary)
	s.compactedTo = cmd.upTo
	s.restore(&summary)
	notify(s.subs, summary)
	s.mu.Unlock()
	cmd.ack <- nil
}

func (s *Session) restore(ev *session.Event) {
	switch ev.Type {
	case session.TypeTurnStart:
		var info session.TurnInfo
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &info)
		}
		if info.ID == "" {
			info.ID = fmt.Sprintf("turn-%d", ev.Seq)
		}
		info.State = session.TurnRunning
		if info.Started.IsZero() {
			info.Started = ev.TS
		}
		s.turn = info
	case session.TypeTurnEnd:
		var info session.TurnInfo
		if len(ev.Data) > 0 {
			_ = json.Unmarshal(ev.Data, &info)
		}
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
	case session.TypeCompactSummary:
		var p struct {
			UpTo uint64 `json:"upTo"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.UpTo > s.compactedTo {
			s.compactedTo = p.UpTo
		}
	}
	if ev.TurnID == "" && s.turn.ID != "" {
		ev.TurnID = s.turn.ID
	}
}

func (s *Session) snapshot(at uint64) []session.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := int(at)
	if at == 0 || limit > len(s.events) {
		limit = len(s.events)
	}
	out := make([]session.Event, limit)
	copy(out, s.events[:limit])
	return out
}

func notify(subs map[chan session.Event]struct{}, ev session.Event) {
	for ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
