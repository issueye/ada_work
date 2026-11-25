package compaction_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	compaction "github.com/issueye/ada_go_v2/plugins/core/compaction-basic"
	replay "github.com/issueye/ada_go_v2/plugins/provider/llm-replay"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

func TestCompactAppendsSummary(t *testing.T) {
	ctx := context.Background()
	provider := memory.New()
	s, err := provider.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatalf("打开会话失败: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := s.Append(session.Event{
			Type: session.TypeUserMessage,
			Data: []byte(`{"role":"user","content":[{"kind":"text","text":"message number ` + string(rune('0'+i%10)) + `"}]}`),
		}); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}

	c := compaction.New(4, 1024)
	if err := c.Compact(ctx, s, nil); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	events, err := s.Events(0, 0)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(events) != 13 {
		t.Fatalf("应追加 1 条摘要,实际 %d 条", len(events))
	}
	last := events[len(events)-1]
	if last.Type != session.TypeCompactSummary {
		t.Fatalf("最后一条应为摘要,实际 %s", last.Type)
	}
	var payload struct {
		UpTo    uint64 `json:"upTo"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(last.Data, &payload); err != nil {
		t.Fatalf("摘要载荷不合法: %v", err)
	}
	if payload.UpTo == 0 || payload.Summary == "" {
		t.Fatalf("摘要内容不完整: %+v", payload)
	}
}

func TestCompactWithModelSummary(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(ctx) })
	mount := func(p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载插件失败: %v", err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("插件未就绪: %v", err)
		}
	}
	mount(session.Plugin(), nil)
	mount(memory.Plugin(), nil)
	mount(llm.Plugin(), nil)
	mount(replay.Plugin(), map[string]any{
		"steps": []replay.Step{{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "MODEL SUMMARY"}}},
			Finish:  llm.FinishStop,
		}}},
		"models":       []llm.ModelInfo{{ID: "summary"}},
		"defaultModel": "summary",
	})
	mount(compaction.Plugin(), map[string]any{
		"keepRecent": 4, "useModel": true, "model": "summary",
	})

	registry, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	s, err := registry.Open(ctx, "", session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if _, err := s.Append(session.Event{
			Type: session.TypeUserMessage,
			Data: []byte(`{"role":"user","content":[{"kind":"text","text":"history"}]}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := app.Root().Service(compaction.ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := raw.(*compaction.Compactor)
	if !ok {
		t.Fatalf("compaction 服务类型不对: %T", raw)
	}
	if err := c.Compact(ctx, s, nil); err != nil {
		t.Fatalf("模型摘要压缩失败: %v", err)
	}
	events, _ := s.Events(0, 0)
	last := events[len(events)-1]
	var payload struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(last.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Summary != "MODEL SUMMARY" {
		t.Fatalf("摘要应来自模型,实际 %q", payload.Summary)
	}
}

func TestCompactNoopWhenShort(t *testing.T) {
	ctx := context.Background()
	provider := memory.New()
	s, err := provider.Open(ctx, session.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.Event{Type: session.TypeUserMessage}); err != nil {
		t.Fatal(err)
	}
	c := compaction.New(8, 1024)
	if err := c.Compact(ctx, s, nil); err != nil {
		t.Fatalf("短会话不应报错: %v", err)
	}
	if events, _ := s.Events(0, 0); len(events) != 1 {
		t.Fatalf("短会话不应追加摘要,实际 %d 条", len(events))
	}
}
