package agentloop_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/prompts"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// recordProvider 记录最后一次请求,用于断言计划被注入且位置正确。
type recordProvider struct {
	mu   sync.Mutex
	last llm.Request
}

func (p *recordProvider) Name() string { return "record" }
func (p *recordProvider) Version() int { return 1 }
func (p *recordProvider) Models(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "record", ContextWindow: 8192}}, nil
}

func (p *recordProvider) response() llm.Response {
	return llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}},
		Finish:  llm.FinishStop,
	}
}

func (p *recordProvider) capture(req llm.Request) {
	p.mu.Lock()
	p.last = req
	p.mu.Unlock()
}

func (p *recordProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.capture(req)
	return p.response(), nil
}

func (p *recordProvider) Stream(_ context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	p.capture(req)
	if onDelta != nil {
		_ = onDelta(llm.Delta{Kind: llm.DeltaText, Text: "ok"})
	}
	return p.response(), nil
}

func (p *recordProvider) messages() []llm.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Message(nil), p.last.Messages...)
}

func TestPlanInjectedBeforeLastUserMessage(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(ctx) })
	mount := func(p kernel.Plugin) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("%s 未就绪: %v", p.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(prompts.Plugin())
	mount(llm.Plugin())
	mount(tools.Plugin())
	mount(agentloop.Plugin())

	rawLLM, err := app.Root().Service("llm")
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordProvider{}
	if _, err := rawLLM.(*llm.Registry).Register(provider); err != nil {
		t.Fatal(err)
	}

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)
	sess, err := ag.Open(ctx, session.Meta{Title: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	p := planseam.Plan{
		Title: "验证",
		Steps: []planseam.Step{
			{Content: "第一步", Status: planseam.StatusPending, Priority: planseam.PriorityHigh},
			{Content: "第二步", Status: planseam.StatusInProgress},
		},
	}
	if _, err := sess.Append(session.Event{
		Type: planseam.EventUpdated, Data: planseam.Encode(p), Source: "test", Ignorable: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Run(ctx, sess, "开始执行"); err != nil {
		t.Fatalf("run 失败: %v", err)
	}

	msgs := provider.messages()
	planIdx, userIdx, planCount := -1, -1, 0
	for i, m := range msgs {
		if m.Role == llm.RoleSystem && strings.Contains(m.Text(), "# 当前计划") {
			planIdx = i
			planCount++
		}
		if m.Role == llm.RoleUser && m.Text() == "开始执行" {
			userIdx = i
		}
	}
	if planCount != 1 {
		t.Fatalf("计划应只注入一次,实际 %d: %+v", planCount, msgs)
	}
	if planIdx < 0 || userIdx < 0 || planIdx > userIdx {
		t.Fatalf("计划应在最后一条用户消息之前: plan=%d user=%d", planIdx, userIdx)
	}
	if !strings.Contains(msgs[planIdx].Text(), "2. [~] 第二步") {
		t.Fatalf("计划文本不符: %q", msgs[planIdx].Text())
	}
}
