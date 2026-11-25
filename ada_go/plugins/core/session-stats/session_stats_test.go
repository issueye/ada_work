package sessionstats_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	sessionstats "github.com/issueye/ada_go_v2/plugins/core/session-stats"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

func TestApplyStats(t *testing.T) {
	p := sessionstats.New()
	var state any
	apply := func(typ, data string) {
		next, changed := p.Apply(state, session.Event{Type: typ, Data: []byte(data)})
		if changed {
			state = next
		}
	}
	apply(session.TypeTurnStart, `{"id":"t1","index":1}`)
	apply(session.TypeUserMessage, `{"role":"user","content":[{"kind":"text","text":"hello world"}]}`)
	apply(session.TypeAssistantMessage, `{"message":{"role":"assistant","content":[{"kind":"text","text":"hi"}]},"usage":{"inputTokens":10,"outputTokens":2}}`)
	apply(session.TypeToolCall, `{"id":"c1","name":"read_file"}`)
	apply(session.TypeToolResult, `{"call":{"id":"c1"},"result":{"status":"ok","content":[{"kind":"text","text":"data"}]}}`)
	apply(session.TypeCompactSummary, `{"upTo":2}`)
	// 控制面事件不改变统计。
	apply(session.TypeStepStart, `{"index":1}`)

	stats, ok := state.(*sessionstats.Stats)
	if !ok {
		t.Fatalf("投影状态类型不对: %T", state)
	}
	if stats.Turns != 1 || stats.Messages != 2 || stats.ToolCalls != 1 {
		t.Fatalf("统计不正确: %+v", stats)
	}
	if stats.Tokens == 0 || stats.CompactedTo != 2 {
		t.Fatalf("token/压缩边界不正确: %+v", stats)
	}
}

func TestProjectionThroughKernel(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(ctx) })

	mount := func(p kernel.Plugin) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			t.Fatalf("挂载插件失败: %v", err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("插件未就绪: %v", err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(sessionstats.Plugin())

	registry, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatalf("取 session 服务失败: %v", err)
	}
	s, err := registry.Open(ctx, "", session.Meta{Title: "stats"})
	if err != nil {
		t.Fatalf("打开会话失败: %v", err)
	}
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
	stats, ok := v.(*sessionstats.Stats)
	if !ok {
		t.Fatalf("投影视图类型不对: %T", v)
	}
	if stats.Turns != 1 || stats.Messages != 1 || stats.Tokens == 0 {
		t.Fatalf("投影结果不正确: %+v", stats)
	}

	// 追加后缓存应只重放尾部。
	if _, err := s.Append(session.Event{
		Type: session.TypeAssistantMessage,
		Data: []byte(`{"message":{"role":"assistant","content":[{"kind":"text","text":"ok"}]}}`),
	}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	v, err = s.Project(sessionstats.ProjectionName)
	if err != nil {
		t.Fatalf("二次投影失败: %v", err)
	}
	stats = v.(*sessionstats.Stats)
	if stats.Messages != 2 {
		t.Fatalf("增量投影不正确: %+v", stats)
	}
}

func TestProjectionUnregistered(t *testing.T) {
	p := memory.New()
	s, err := p.Open(context.Background(), session.Meta{})
	if err != nil {
		t.Fatalf("打开会话失败: %v", err)
	}
	if _, err := s.Project("missing"); err == nil {
		t.Fatalf("未注册投影应返回错误")
	}
}
