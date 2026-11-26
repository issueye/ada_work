package coreplan_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	planplugin "github.com/issueye/ada_go_v2/plugins/core/plan"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
)

func mount(t *testing.T) (*seamtools.Registry, *session.Registry) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
		if err := f.Ready(context.Background()); err != nil {
			t.Fatalf("%s 未就绪: %v", p.Name(), err)
		}
	}
	mount(session.Plugin())
	mount(memory.Plugin())
	mount(seamtools.Plugin())
	mount(planplugin.Plugin())

	rawTools, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	rawSessions, err := app.Root().Service("session")
	if err != nil {
		t.Fatal(err)
	}
	return rawTools.(*seamtools.Registry), rawSessions.(*session.Registry)
}

func invoke(t *testing.T, reg *seamtools.Registry, sess session.Session, name string, args map[string]any) seamtools.Result {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx := session.WithSession(context.Background(), sess)
	res, err := reg.Invoke(ctx, seamtools.CallInfo{
		Name: name, SessionID: sess.ID(), Args: data,
	})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	return res
}

func text(res seamtools.Result) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

func TestPlanSetUpdateShow(t *testing.T) {
	reg, sessions := mount(t)
	sess, err := sessions.Open(context.Background(), "", session.Meta{Title: "plan"})
	if err != nil {
		t.Fatal(err)
	}

	res := invoke(t, reg, sess, "plan_set", map[string]any{
		"title": "发布",
		"steps": []map[string]any{
			{"content": "写代码", "priority": "high"},
			{"content": "跑测试", "status": "in_progress"},
		},
	})
	if res.Status != seamtools.StatusOK || !strings.Contains(text(res), "1. [ ] 写代码 (high)") {
		t.Fatalf("plan_set 结果不符: %+v", res)
	}

	events, err := sess.Events(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != planseam.EventUpdated || !last.Ignorable {
		t.Fatalf("计划事件不符: %+v", last)
	}
	p, err := planseam.Decode(last.Data)
	if err != nil || len(p.Steps) != 2 || p.Steps[1].Status != planseam.StatusInProgress {
		t.Fatalf("计划载荷不符: %+v err=%v", p, err)
	}

	if res := invoke(t, reg, sess, "plan_update", map[string]any{"index": 2, "status": "completed"}); res.Status != seamtools.StatusOK {
		t.Fatalf("plan_update 失败: %+v", res)
	}
	if res := invoke(t, reg, sess, "plan_show", map[string]any{}); !strings.Contains(text(res), "2. [x] 跑测试") {
		t.Fatalf("plan_show 不符: %q", text(res))
	}
}

func TestPlanInvalid(t *testing.T) {
	reg, sessions := mount(t)
	sess, _ := sessions.Open(context.Background(), "", session.Meta{Title: "plan"})

	if res := invoke(t, reg, sess, "plan_set", map[string]any{"steps": []any{}}); res.Status != seamtools.StatusError {
		t.Fatalf("空步骤应失败: %+v", res)
	}
	if res := invoke(t, reg, sess, "plan_update", map[string]any{"index": 1, "status": "pending"}); res.Status != seamtools.StatusError {
		t.Fatalf("没有计划时更新应失败: %+v", res)
	}
	invoke(t, reg, sess, "plan_set", map[string]any{"steps": []map[string]any{{"content": "只有一步"}}})
	if res := invoke(t, reg, sess, "plan_update", map[string]any{"index": 9, "status": "completed"}); res.Status != seamtools.StatusError {
		t.Fatalf("越界 index 应失败: %+v", res)
	}
	if res := invoke(t, reg, sess, "plan_show", map[string]any{}); !strings.Contains(text(res), "当前计划") {
		t.Fatalf("plan_show 应返回计划: %q", text(res))
	}

	// 没有会话上下文:工具必须拒绝,而不是写到一个猜出来的会话。
	raw, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"content": "x"}}})
	if res, err := reg.Invoke(context.Background(), seamtools.CallInfo{Name: "plan_set", Args: raw}); err != nil || res.Status != seamtools.StatusError {
		t.Fatalf("缺少会话上下文应失败: %+v err=%v", res, err)
	}
}
