package jsonl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	sessionstats "github.com/issueye/ada_go_v2/plugins/core/session-stats"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/session/sessiontest"
)

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	p := New(NewFileStore(t.TempDir(), true))
	t.Cleanup(p.CloseAll)
	return p
}

func openTest(t *testing.T, p *Provider, meta session.Meta) *Session {
	t.Helper()
	s, err := p.Open(context.Background(), meta)
	if err != nil {
		t.Fatalf("打开会话失败: %v", err)
	}
	return s.(*Session)
}

// 契约测试:provider 必须全绿才能进 preset。
func TestConformance(t *testing.T) {
	p := newTestProvider(t)
	sessiontest.Run(t, sessiontest.Options{
		New: func(t *testing.T) session.Session {
			return openTest(t, p, session.Meta{Title: t.Name()})
		},
		SupportsFork:      true,
		SupportsCompact:   true,
		SupportsSubscribe: true,
	})
}

// 一个会话一个文件;重新打开后事件、seq、元信息不变。
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	p := New(NewFileStore(dir, true))
	defer p.CloseAll()
	s, err := p.OpenID(context.Background(), "sess-persist", session.Meta{Title: "persist", Model: "m1", Workspace: "docs"})
	if err != nil {
		t.Fatalf("打开会话失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Append(session.Event{
			Type: session.TypeUserMessage,
			Data: []byte(fmt.Sprintf(`{"text":"m%d"}`, i)),
		}); err != nil {
			t.Fatalf("写入第 %d 条失败: %v", i, err)
		}
	}
	if err := s.(*Session).Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "sess-persist.jsonl")); err != nil {
		t.Fatalf("会话文件不存在: %v", err)
	} else if fi.Size() == 0 {
		t.Fatalf("会话文件为空")
	}

	p2 := New(NewFileStore(dir, true))
	defer p2.CloseAll()
	s2, err := p2.OpenID(context.Background(), "sess-persist", session.Meta{})
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	if s2.Meta().Title != "persist" || s2.Meta().Model != "m1" || s2.Meta().Workspace != "docs" {
		t.Fatalf("元信息未恢复: %+v", s2.Meta())
	}
	events, err := s2.Events(0, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("应恢复 3 条事件,实际 %d", len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq 不连续: 第 %d 条为 %d", i+1, ev.Seq)
		}
	}
}

// 多个会话并发写:不同文件、不同 actor,不应互相排队或丢事件。
func TestConcurrentSessions(t *testing.T) {
	p := newTestProvider(t)
	const sessions = 32
	const perSession = 40

	errCh := make(chan error, sessions)
	var wg sync.WaitGroup
	list := make([]*Session, sessions)
	for i := 0; i < sessions; i++ {
		s := openTest(t, p, session.Meta{Title: fmt.Sprintf("s-%d", i)})
		list[i] = s
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			for j := 0; j < perSession; j++ {
				if _, err := s.Append(session.Event{
					Type: session.TypeUserMessage,
					Data: []byte(fmt.Sprintf(`{"i":%d}`, j)),
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发写入失败: %v", err)
	}

	for i, s := range list {
		events, err := s.Events(0, 0)
		if err != nil {
			t.Fatalf("会话 %d 读取失败: %v", i, err)
		}
		if len(events) != perSession {
			t.Fatalf("会话 %d 应写入 %d 条,实际 %d", i, perSession, len(events))
		}
		for j, ev := range events {
			if ev.Seq != uint64(j+1) {
				t.Fatalf("会话 %d 第 %d 条 seq 应为 %d,实际 %d", i, j, j+1, ev.Seq)
			}
		}
	}
}

// 同一会话多个生产者:actor 串行分配 seq,不出现重复或交错。
func TestSameSessionActorSerializes(t *testing.T) {
	p := newTestProvider(t)
	s := openTest(t, p, session.Meta{Title: "actor"})
	const writers = 16
	const perWriter = 25

	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				if _, err := s.Append(session.Event{Type: session.TypeToolResult}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发写入失败: %v", err)
	}

	events, err := s.Events(0, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(events) != writers*perWriter {
		t.Fatalf("应写入 %d 条,实际 %d", writers*perWriter, len(events))
	}
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("seq 应从 1 连续,实际第 %d 条为 %d", i, ev.Seq)
		}
	}
}

// 崩溃留下的半行应在打开时截断,后续 seq 从完整事件之后继续。
func TestTornTailRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess-torn.jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "{\"seq\":1,\"ts\":\"2026-01-01T00:00:00Z\",\"type\":\"user/message\",\"sessionId\":\"sess-torn\",\"source\":\"core\"}\n" +
		"{\"seq\":2,\"ts\":\"2026-01-01T00:00:01Z\",\"type\":\"assistant/message\",\"sessionId\":\"sess-torn\",\"source\":\"core\"}\n" +
		"{\"seq\":3,\"type\":\"user/message\""
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(NewFileStore(dir, true))
	defer p.CloseAll()
	s, err := p.OpenID(context.Background(), "sess-torn", session.Meta{})
	if err != nil {
		t.Fatalf("打开带半行的会话失败: %v", err)
	}
	events, err := s.Events(0, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("半行应被丢弃,实际 %d 条", len(events))
	}
	if _, err := s.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		t.Fatalf("恢复后写入失败: %v", err)
	}
	if got, _ := s.Events(0, 0); len(got) != 3 || got[2].Seq != 3 {
		t.Fatalf("恢复后的 seq 不正确: %+v", got)
	}
	if data, _ := os.ReadFile(path); len(data) != 0 {
		// 只要文件里不再含半行即可;检查最后一条是完整 JSON + 换行。
		if data[len(data)-1] != '\n' {
			t.Fatalf("恢复后的文件末尾不是换行")
		}
	}
}

// 会话 id 不能变成路径穿越。
func TestRejectBadID(t *testing.T) {
	p := newTestProvider(t)
	for _, id := range []string{"../escape", "a/b", `a\b`, "", ".hidden", "-bad"} {
		if _, err := p.OpenID(context.Background(), id, session.Meta{}); err != nil {
			if !seamerr.Is(err, seamerr.ErrInvalid) {
				t.Fatalf("id %q 应返回 EINVALID,实际 %v", id, err)
			}
		} else {
			t.Fatalf("id %q 应被拒绝", id)
		}
	}
}

// jsonl 会话也要支持投影:注册表注入后,Project 走同一套缓存/重放。
func TestProjectionWithJSONL(t *testing.T) {
	p := New(NewFileStore(t.TempDir(), true))
	defer p.CloseAll()
	reg := session.NewProjectionRegistry()
	p.BindProjections(reg)
	if _, err := reg.Register(sessionstats.New()); err != nil {
		t.Fatalf("注册投影失败: %v", err)
	}
	s := openTest(t, p, session.Meta{})
	if _, err := s.Append(
		session.Event{Type: session.TypeTurnStart, Data: []byte(`{"id":"t1","index":1}`)},
		session.Event{Type: session.TypeUserMessage, Data: []byte(`{"role":"user","content":[{"kind":"text","text":"hello"}]}`)},
	); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	v, err := s.Project(sessionstats.ProjectionName)
	if err != nil {
		t.Fatalf("投影失败: %v", err)
	}
	st, ok := v.(*sessionstats.Stats)
	if !ok || st.Turns != 1 || st.Messages != 1 || st.Tokens == 0 {
		t.Fatalf("投影结果不正确: %T %+v", v, v)
	}
}

// 插件经内核装配:seam 提供 Registry,provider 注册进来。
func TestPluginThroughKernel(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(session.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam: %v", err)
	}
	fiber, err := app.Plugin(Plugin(), kernel.NewConfig(map[string]any{"dir": dir, "sync": false}))
	if err != nil {
		t.Fatalf("挂载 provider: %v", err)
	}
	if fiber.State() != kernel.Active {
		t.Fatalf("provider 应 active,实际 %s", fiber.State())
	}
	registry, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatalf("取 session 服务: %v", err)
	}
	s, err := registry.Open(context.Background(), "", session.Meta{Title: "kernel"})
	if err != nil {
		t.Fatalf("打开会话: %v", err)
	}
	if _, err := s.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		t.Fatalf("写入: %v", err)
	}
	if events, _ := s.Events(0, 0); len(events) != 1 {
		t.Fatalf("应读到 1 条,实际 %d", len(events))
	}
	if err := fiber.Dispose(context.Background()); err != nil {
		t.Fatalf("卸载 provider: %v", err)
	}
	if _, err := registry.Provider(""); err == nil {
		t.Fatalf("卸载后不应还有 provider")
	}
}

// Close 后 Append 必须返回 ECANCELED,而不是挂起。
func TestAppendAfterClose(t *testing.T) {
	p := newTestProvider(t)
	s := openTest(t, p, session.Meta{})
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, err := s.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		if !seamerr.Is(err, seamerr.ErrCanceled) {
			t.Fatalf("关闭后写入应返回 ECANCELED,实际 %v", err)
		}
	} else {
		t.Fatalf("关闭后写入应失败")
	}
}
