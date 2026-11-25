// 验证同一进程、同一 Agent、不同会话可以并发跑轮:会话之间没有共享轮次状态。
package agentloop_test

import (
	"context"
	"sync"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	agentloop "github.com/issueye/ada_go_v2/plugins/core/agent-loop"
	replay "github.com/issueye/ada_go_v2/plugins/provider/llm-replay"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// TestConcurrentSessionsInOneProcess:两个会话并发 Run,事件不串、各自完成。
func TestConcurrentSessionsInOneProcess(t *testing.T) {
	ctx := context.Background()
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	mount := func(id string, cfg map[string]any) {
		t.Helper()
		mk := builtins.Plugins()[id]
		if mk == nil {
			t.Fatalf("内置插件缺失: %s", id)
		}
		f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", id, err)
		}
		if err := f.Ready(ctx); err != nil {
			t.Fatalf("等待 %s 就绪失败: %v", id, err)
		}
	}
	mount("seam-session", nil)
	mount("session-memory", nil)
	mount("seam-prompts", nil)
	mount("seam-llm", nil)
	steps := make([]replay.Step, 4)
	for i := range steps {
		steps[i] = replay.Step{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}},
			Finish:  llm.FinishStop,
		}}
	}
	mount("llm-replay", map[string]any{
		"steps": steps, "models": []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}}, "defaultModel": "replay",
	})
	mount("seam-tools", nil)
	mount("agent-loop", map[string]any{"model": "replay"})

	raw, err := app.Root().Service("agent")
	if err != nil {
		t.Fatal(err)
	}
	ag := raw.(*agentloop.Agent)

	open := func() session.Session {
		s, err := ag.Open(ctx, session.Meta{})
		if err != nil {
			t.Fatalf("打开会话失败: %v", err)
		}
		return s
	}
	s1, s2 := open(), open()
	if s1.ID() == s2.ID() {
		t.Fatalf("会话 id 应不同")
	}

	type result struct {
		res agentloop.TurnResult
		err error
	}
	out := make(chan result, 2)
	var wg sync.WaitGroup
	for _, tc := range []struct {
		sess session.Session
		msg  string
	}{{s1, "one"}, {s2, "two"}} {
		wg.Add(1)
		go func(sess session.Session, msg string) {
			defer wg.Done()
			res, err := ag.Run(ctx, sess, msg)
			out <- result{res: res, err: err}
		}(tc.sess, tc.msg)
	}
	wg.Wait()
	close(out)
	for r := range out {
		if r.err != nil || r.res.Text != "ok" {
			t.Fatalf("并发轮次失败: %+v %v", r.res, r.err)
		}
	}

	for _, sess := range []session.Session{s1, s2} {
		evs, err := sess.Events(0, 0)
		if err != nil {
			t.Fatalf("读取会话 %s 失败: %v", sess.ID(), err)
		}
		for _, ev := range evs {
			if ev.SessionID != sess.ID() {
				t.Fatalf("会话 %s 混入其它会话事件: %+v", sess.ID(), ev)
			}
		}
		var users, ends int
		for _, ev := range evs {
			switch ev.Type {
			case session.TypeUserMessage:
				users++
			case session.TypeTurnEnd:
				ends++
			}
		}
		if users != 1 || ends != 1 {
			t.Fatalf("会话 %s 事件不完整: users=%d ends=%d", sess.ID(), users, ends)
		}
	}
}
