package app

import (
	"path/filepath"
	"testing"
	"time"
)

// queueEngine 只记录被发出的消息,不真的起进程。
type queueEngine struct {
	sends []string
}

func (f *queueEngine) Start(*Bot) error { return nil }
func (f *queueEngine) Stop(int) error   { return nil }
func (f *queueEngine) Send(_ *Bot, _ int, text string, _ int) error {
	f.sends = append(f.sends, text)
	return nil
}
func (f *queueEngine) Cancel(int, int) error                   { return nil }
func (f *queueEngine) Approve(int, string, bool, string) error { return nil }
func (f *queueEngine) Close() error                            { return nil }

func busySession(id, botID int) *App {
	a := &App{ActiveID: id, nextID: 200, Engine: &queueEngine{}}
	a.Bots = []*Bot{{ID: botID, Name: "小助手", State: BotRunning}}
	s := &Session{ID: id, BotID: botID, Title: "队列测试", Updated: time.Now()}
	s.Messages = []*Message{{ID: 100, Role: RoleAssistant, Streaming: true}}
	a.Sessions = []*Session{s}
	return a
}

// TestSendQueuesWhileBusy 守住「生成中可以继续发送」:草稿进队列,不丢。
func TestSendQueuesWhileBusy(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := busySession(1, 1)
	a.Draft = "排队的第一条"
	a.Send()

	if len(a.Sessions[0].Queue) != 1 || a.Sessions[0].Queue[0] != "排队的第一条" {
		t.Fatalf("忙时发送应进队列,实际 %+v", a.Sessions[0].Queue)
	}
	if a.Draft != "" {
		t.Fatalf("入队后草稿应清空")
	}
	if got := a.Engine.(*queueEngine).sends; len(got) != 0 {
		t.Fatalf("忙时不应立刻发出去: %v", got)
	}
}

// TestTurnDoneDrainsQueue 守住队列排空:这一轮结束,下一条自动发出。
func TestTurnDoneDrainsQueue(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := busySession(1, 1)
	a.Draft = "排队的第一条"
	a.Send()
	a.Draft = "排队的第二条"
	a.Send()

	a.BotTurnDone(1, 100, "第一轮完成")

	s := a.Sessions[0]
	if len(s.Queue) != 1 || s.Queue[0] != "排队的第二条" {
		t.Fatalf("应只剩第二条排队,实际 %+v", s.Queue)
	}
	if got := a.Engine.(*queueEngine).sends; len(got) != 1 || got[0] != "排队的第一条" {
		t.Fatalf("第一轮结束应发出第一条: %v", got)
	}
	if !s.Running() {
		t.Fatalf("发出排队消息后应有新一轮在生成")
	}
	last := s.Messages[len(s.Messages)-1]
	if last.Role != RoleAssistant || !last.Streaming {
		t.Fatalf("队列排空应新增一条流式助手消息: %+v", last)
	}
}

// TestStopTurnClearsQueue 停止生成时排队消息一并丢弃,不会停下来又自动发。
func TestStopTurnClearsQueue(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := busySession(1, 1)
	a.Draft = "排队"
	a.Send()
	if !a.StopTurn() {
		t.Fatalf("应能停止当前生成")
	}
	if len(a.Sessions[0].Queue) != 0 {
		t.Fatalf("停止后队列应清空,实际 %+v", a.Sessions[0].Queue)
	}
}

// TestDropQueued 移除指定的排队消息。
func TestDropQueued(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := busySession(1, 1)
	a.Draft = "一"
	a.Send()
	a.Draft = "二"
	a.Send()
	if !a.DropQueued(a.Sessions[0], 0) {
		t.Fatalf("移除第一条应成功")
	}
	if len(a.Sessions[0].Queue) != 1 || a.Sessions[0].Queue[0] != "二" {
		t.Fatalf("移除后应只剩第二条: %+v", a.Sessions[0].Queue)
	}
}
