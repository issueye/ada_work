// Package sessiontest 是 session seam 的契约测试(conformance suite)。
//
// provider 必须全绿才能进 preset(seam-api §1.6)。这里只断言契约里写死的不变量,
// 不测某个 provider 的实现细节——否则 provider 换个后端就要重写测试。
package sessiontest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// Options 描述被测 provider 的能力与工厂。
type Options struct {
	// New 每个用例给一个干净会话。
	New func(t *testing.T) session.Session
	// 能力声明:不支持的子用例跳过,而不是假装通过。
	SupportsFork      bool
	SupportsCompact   bool
	SupportsSubscribe bool
	// Project 提供时额外测投影;nil 表示该 provider 不支持投影。
	Project func(t *testing.T, s session.Session) (any, error)
}

// Run 跑全部适用用例。
func Run(t *testing.T, opts Options) {
	t.Helper()
	if opts.New == nil {
		t.Fatalf("Options.New 不能为空")
	}

	t.Run("append 顺序", func(t *testing.T) { testAppend(t, opts) })
	t.Run("空类型被拒绝", func(t *testing.T) { testRejectEmptyType(t, opts) })
	t.Run("未知事件可忽略", func(t *testing.T) { testIgnorable(t, opts) })
	t.Run("turn 状态", func(t *testing.T) { testTurn(t, opts) })
	t.Run("订阅回放与推送", func(t *testing.T) {
		if !opts.SupportsSubscribe {
			t.Skip("provider 不支持订阅")
		}
		testSubscribe(t, opts)
	})
	t.Run("fork 隔离", func(t *testing.T) {
		if !opts.SupportsFork {
			t.Skip("provider 不支持 fork")
		}
		testFork(t, opts)
	})
	t.Run("压缩追加摘要", func(t *testing.T) {
		if !opts.SupportsCompact {
			t.Skip("provider 不支持 compact")
		}
		testCompact(t, opts)
	})
	t.Run("投影", func(t *testing.T) {
		if opts.Project == nil {
			t.Skip("provider 不支持投影")
		}
		if _, err := opts.Project(t, opts.New(t)); err != nil {
			t.Fatalf("投影失败: %v", err)
		}
	})
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func testAppend(t *testing.T, opts Options) {
	s := opts.New(t)
	seqs, err := s.Append(
		session.Event{Type: session.TypeUserMessage, Data: raw(`{"text":"hi"}`)},
		session.Event{Type: session.TypeAssistantMessage, Data: raw(`{"text":"hello"}`)},
	)
	if err != nil {
		t.Fatalf("append 失败: %v", err)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seq 应从 1 连续分配,实际 %v", seqs)
	}

	events, err := s.Events(1, 0)
	if err != nil {
		t.Fatalf("events 失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("应读到 2 条事件,实际 %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("第 %d 条的 seq 应为 %d,实际 %d", i, i+1, ev.Seq)
		}
		if ev.SessionID != s.ID() {
			t.Fatalf("事件的 SessionID 应为 %q,实际 %q", s.ID(), ev.SessionID)
		}
		if ev.TS.IsZero() {
			t.Fatalf("事件缺少时间戳: %+v", ev)
		}
		if ev.Source == "" {
			t.Fatalf("事件缺少来源: %+v", ev)
		}
	}

	// 分页:从 seq=2 开始取 1 条。
	page, err := s.Events(2, 1)
	if err != nil {
		t.Fatalf("分页失败: %v", err)
	}
	if len(page) != 1 || page[0].Seq != 2 {
		t.Fatalf("分页结果不正确: %+v", page)
	}
}

func testRejectEmptyType(t *testing.T, opts Options) {
	s := opts.New(t)
	if _, err := s.Append(session.Event{}); err == nil || !seamerr.Is(err, seamerr.ErrInvalid) {
		t.Fatalf("空 type 应返回 EINVALID,实际 %v", err)
	}
}

func testIgnorable(t *testing.T, opts Options) {
	s := opts.New(t)
	name := session.PluginEvent("demo", "note")
	if _, err := s.Append(session.Event{Type: name, Ignorable: true, Data: raw(`{"x":1}`)}); err != nil {
		t.Fatalf("插件事件写入失败: %v", err)
	}
	events, err := s.Events(0, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("读取失败: %v %d", err, len(events))
	}
	if !events[0].Ignorable {
		t.Fatalf("插件事件应带 Ignorable 标记")
	}
	if session.ModelVisible(name) {
		t.Fatalf("插件事件不应进入模型可见上下文")
	}
}

func testTurn(t *testing.T, opts Options) {
	s := opts.New(t)
	if _, err := s.Append(session.Event{
		Type: session.TypeTurnStart, Data: raw(`{"id":"t1","index":1,"state":"running"}`),
	}); err != nil {
		t.Fatalf("写入 turn/start 失败: %v", err)
	}
	turn := s.Turn()
	if turn.ID != "t1" || turn.Index != 1 || turn.State != session.TurnRunning {
		t.Fatalf("turn 状态不正确: %+v", turn)
	}
	if _, err := s.Append(session.Event{
		Type: session.TypeTurnEnd, Data: raw(`{"id":"t1","index":1,"state":"done"}`),
	}); err != nil {
		t.Fatalf("写入 turn/end 失败: %v", err)
	}
	if turn = s.Turn(); turn.State != session.TurnDone {
		t.Fatalf("turn 应结束,实际 %+v", turn)
	}
}

func readWithTimeout(t *testing.T, ch <-chan session.Event) (session.Event, bool) {
	t.Helper()
	select {
	case ev := <-ch:
		return ev, true
	case <-time.After(time.Second):
		return session.Event{}, false
	}
}

func testSubscribe(t *testing.T, opts Options) {
	s := opts.New(t)
	if _, err := s.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	ch, dispose := s.Subscribe(1)
	// 回放既有事件
	if ev, ok := readWithTimeout(t, ch); !ok || ev.Seq != 1 {
		t.Fatalf("订阅应从历史回放开始,实际 %+v(ok=%v)", ev, ok)
	}
	// 实时推送
	if _, err := s.Append(session.Event{Type: session.TypeAssistantMessage}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if ev, ok := readWithTimeout(t, ch); !ok || ev.Seq != 2 {
		t.Fatalf("订阅应收到新事件,实际 %+v(ok=%v)", ev, ok)
	}
	if err := dispose(context.Background()); err != nil {
		t.Fatalf("取消订阅失败: %v", err)
	}
	if _, err := s.Append(session.Event{Type: session.TypeAssistantMessage}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	select {
	case ev := <-ch:
		t.Fatalf("dispose 之后不应再收到事件,实际 %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func testFork(t *testing.T, opts Options) {
	s := opts.New(t)
	if _, err := s.Append(
		session.Event{Type: session.TypeUserMessage, Data: raw(`{"text":"one"}`)},
		session.Event{Type: session.TypeAssistantMessage, Data: raw(`{"text":"two"}`)},
	); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	child, dispose, err := s.Fork(2)
	if err != nil {
		t.Fatalf("fork 失败: %v", err)
	}
	if err := dispose(context.Background()); err != nil {
		t.Fatalf("fork dispose 失败: %v", err)
	}
	if child.ID() == s.ID() {
		t.Fatalf("fork 应产生新 id")
	}
	if child.Meta().Parent != s.ID() {
		t.Fatalf("子会话 Meta.Parent 应为父 id,实际 %q", child.Meta().Parent)
	}
	childEvents, err := child.Events(0, 0)
	if err != nil || len(childEvents) != 2 {
		t.Fatalf("子会话应复制 2 条历史: %v %d", err, len(childEvents))
	}

	// 父追加不影响子,子追加不影响父。
	if _, err := s.Append(session.Event{Type: session.TypeToolResult}); err != nil {
		t.Fatalf("父写入失败: %v", err)
	}
	if after, _ := child.Events(0, 0); len(after) != 2 {
		t.Fatalf("父追加不应出现在子会话,实际 %d 条", len(after))
	}
	if _, err := child.Append(session.Event{Type: session.TypeToolResult}); err != nil {
		t.Fatalf("子写入失败: %v", err)
	}
	if after, _ := s.Events(0, 0); len(after) != 3 {
		t.Fatalf("子追加不应出现在父会话,实际 %d 条", len(after))
	}
}

func testCompact(t *testing.T, opts Options) {
	s := opts.New(t)
	if _, err := s.Append(
		session.Event{Type: session.TypeUserMessage},
		session.Event{Type: session.TypeAssistantMessage},
	); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := s.Compact(context.Background(), 2, session.Event{Data: raw(`{"text":"summary"}`)}); err != nil {
		t.Fatalf("compact 失败: %v", err)
	}
	events, err := s.Events(3, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("compact 应追加一条摘要事件: %v %d", err, len(events))
	}
	if events[0].Type != session.TypeCompactSummary {
		t.Fatalf("摘要事件类型应为 %q,实际 %q", session.TypeCompactSummary, events[0].Type)
	}
	if err := s.Compact(context.Background(), 0, session.Event{}); err == nil || !seamerr.Is(err, seamerr.ErrInvalid) {
		t.Fatalf("非法 upTo 应返回 EINVALID,实际 %v", err)
	}
}
