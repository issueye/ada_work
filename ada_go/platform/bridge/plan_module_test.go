package bridge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// ada:plan 让 TS 工具直接读写会话计划:不需要把 sessionId 暴露给脚本。
func TestJSPlanModule(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { get, set, update } from "ada:plan"
type Ctx = { tool: (spec: any, handler: (args: any) => any) => void }
export function apply(ctx: Ctx) {
  ctx.tool(
    { name: "plan_demo", summary: "TS 会话计划", readOnly: false, input: { type: "object", properties: {} } },
    () => {
      set({ title: "TS", steps: [{ content: "a" }, { content: "b", priority: "high" }] })
      update(1, "completed")
      return JSON.stringify(get())
    },
  )
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "js-plan", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools"}},
	})
	if err != nil {
		t.Fatalf("构造 bridge 失败: %v", err)
	}

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(plugin kernel.Plugin) {
		t.Helper()
		if _, err := app.Plugin(plugin, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", plugin.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(seamtools.Plugin())
	bf, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 bridge 失败: %v", err)
	}
	if err := bf.Ready(context.Background()); err != nil {
		t.Fatalf("bridge 未就绪: %v", err)
	}

	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.Open(context.Background(), "", session.Meta{Title: "plan"})
	if err != nil {
		t.Fatal(err)
	}

	ctx := session.WithSession(context.Background(), sess)
	res, err := reg.Invoke(ctx, seamtools.CallInfo{
		Name: "plan_demo", SessionID: sess.ID(), Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res.Status != seamtools.StatusOK || len(res.Content) == 0 {
		t.Fatalf("工具结果不符: %+v", res)
	}
	var got planseam.Plan
	if err := json.Unmarshal([]byte(res.Content[0].Text), &got); err != nil {
		t.Fatalf("返回的计划不是 JSON: %q err=%v", res.Content[0].Text, err)
	}
	if got.Title != "TS" || len(got.Steps) != 2 || got.Steps[0].Status != planseam.StatusCompleted {
		t.Fatalf("计划内容不符: %+v", got)
	}

	events, err := sess.Events(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var planEvents int
	for _, ev := range events {
		if ev.Type == planseam.EventUpdated {
			planEvents++
		}
	}
	if planEvents != 2 {
		t.Fatalf("set + update 应写两条计划事件,实际 %d", planEvents)
	}

	// 没有会话上下文时必须失败,不能写到任意会话。
	res, err = reg.Invoke(context.Background(), seamtools.CallInfo{
		Name: "plan_demo", Args: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != seamtools.StatusError || !strings.Contains(res.Content[0].Text, "会话") {
		t.Fatalf("缺少会话上下文应失败: %+v", res)
	}
}
