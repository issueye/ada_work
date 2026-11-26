package main

import (
	"context"
	"testing"
	"time"

	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	seamsession "github.com/issueye/ada_go_v2/plugins/seam/session"
)

func TestForwardPlanUpdatesAndLatest(t *testing.T) {
	reg := seamsession.NewRegistry()
	if _, err := reg.Register(memory.New()); err != nil {
		t.Fatal(err)
	}
	sess, err := reg.Open(context.Background(), "", seamsession.Meta{Title: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan map[string]any, 4)
	publish := func(topic string, payload map[string]any) {
		if topic != "chat/plan" {
			t.Errorf("topic = %q,期望 chat/plan", topic)
		}
		got <- payload
	}
	stop := forwardPlanUpdates(sess, 1, 7, publish)
	defer stop()

	plan := planseam.Plan{Title: "发布", Steps: []planseam.Step{
		{Content: "写代码", Status: planseam.StatusCompleted, Priority: planseam.PriorityHigh},
	}}
	if _, err := sess.Append(seamsession.Event{
		Type: planseam.EventUpdated, Data: planseam.Encode(plan), Source: "test", Ignorable: true,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-got:
		if payload["turn"] != 7 || payload["title"] != "发布" {
			t.Fatalf("chat/plan 载荷不符: %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有转发 chat/plan")
	}

	// 停掉实时转发后,收尾仍需把最新计划补发一次。
	stop()
	publishLatestPlan(sess, 7, publish)
	select {
	case payload := <-got:
		if payload["title"] != "发布" {
			t.Fatalf("最新计划载荷不符: %+v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有补发最新计划")
	}
}
