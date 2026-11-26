package bot

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ada_chat/internal/app"
	"ada_chat/internal/rpc"
)

type planPeer struct {
	events chan rpc.Event
}

func (p *planPeer) Call(context.Context, string, any, any) error { return nil }
func (p *planPeer) Notify(context.Context, string, any) error    { return nil }
func (p *planPeer) Subscribe(context.Context, ...string) error   { return nil }
func (p *planPeer) Events() <-chan rpc.Event                     { return p.events }
func (p *planPeer) Remote() string                               { return "test" }
func (p *planPeer) Close() error                                 { return nil }

// TestRPCPlanEventUpdatesTodo 守住 RPC 事件到会话任务列表的映射。
func TestRPCPlanEventUpdatesTodo(t *testing.T) {
	restore := app.SetStatePath(filepath.Join(t.TempDir(), "state.json"))
	t.Cleanup(restore)
	a := &app.App{
		Sessions: []*app.Session{{
			ID: 1, BotID: 1, Title: "plan",
			Messages: []*app.Message{{ID: 7, Role: app.RoleAssistant, At: time.Now()}},
		}},
	}
	var mu sync.Mutex
	a.SetDispatch(func(fn func()) {
		mu.Lock()
		defer mu.Unlock()
		fn()
	})
	r := &instance{
		key: "ws", turns: map[int]int{7: 1}, bots: map[int]struct{}{1: {}},
	}
	r.stopping.Store(true) // 事件循环结束时不触发 lost
	s := &Supervisor{app: a, instances: map[string]*instance{"ws": r}}
	peer := &planPeer{events: make(chan rpc.Event, 1)}
	go s.events(r, peer)

	data, _ := json.Marshal(map[string]any{
		"turn": 7, "title": "发布",
		"steps": []map[string]string{
			{"content": "写代码", "status": "completed", "priority": "high"},
			{"content": "跑测试", "status": "in_progress"},
		},
	})
	peer.events <- rpc.Event{Topic: "chat/plan", Data: data}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		var todo *app.Todo
		if s := a.SessionByID(1); s != nil {
			todo = s.Todo
		}
		mu.Unlock()
		if todo != nil && len(todo.Steps) == 2 {
			close(peer.events)
			if todo.Title != "发布" || todo.Steps[0].Status != "completed" {
				t.Fatalf("任务映射不符: %+v", todo)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("没有收到 chat/plan 更新")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
