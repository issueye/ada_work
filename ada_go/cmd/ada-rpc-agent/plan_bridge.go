package main

import (
	"context"
	"encoding/json"
	"sync"

	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	seamsession "github.com/issueye/ada_go_v2/plugins/seam/session"
)

// forwardPlanUpdates 把一轮里新增的 plan 事件实时转成 chat/plan。
//
// plan 不是总线事件,而是 session 事件;chat.send 在 Run 前订阅、Run 后 drain,
// 这样计划工具一执行,桌面端就能更新任务面板。
func forwardPlanUpdates(sess seamsession.Session, from uint64, turn int, publish func(string, map[string]any)) func() {
	updates, dispose := sess.Subscribe(from)
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once

	handle := func(ev seamsession.Event) {
		if ev.Type != planseam.EventUpdated {
			return
		}
		var p planseam.Plan
		if json.Unmarshal(ev.Data, &p) != nil {
			return
		}
		publish("chat/plan", map[string]any{
			"turn": turn, "title": p.Title, "steps": p.Steps,
		})
	}
	drain := func() {
		for {
			select {
			case ev := <-updates:
				handle(ev)
			default:
				return
			}
		}
	}

	go func() {
		defer close(done)
		for {
			select {
			case ev := <-updates:
				handle(ev)
			case <-stop:
				drain()
				return
			}
		}
	}()

	return func() {
		once.Do(func() {
			close(stop)
			<-done
			_ = dispose(context.Background())
		})
	}
}

// publishLatestPlan 在 Run 结束后再发一次最新计划,防止订阅通道背压时
// 最后一条 plan 被丢掉;客户端重复设置同一份计划是幂等的。
func publishLatestPlan(sess seamsession.Session, turn int, publish func(string, map[string]any)) {
	events, err := sess.Events(0, 0)
	if err != nil {
		return
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != planseam.EventUpdated {
			continue
		}
		var p planseam.Plan
		if json.Unmarshal(events[i].Data, &p) != nil {
			return
		}
		publish("chat/plan", map[string]any{
			"turn": turn, "title": p.Title, "steps": p.Steps,
		})
		return
	}
}
