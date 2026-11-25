package app

import (
	"path/filepath"
	"testing"
	"time"
)

type fakeEngine struct {
	approved []string
}

func (f *fakeEngine) Start(*Bot) error                  { return nil }
func (f *fakeEngine) Stop(int) error                    { return nil }
func (f *fakeEngine) Send(*Bot, int, string, int) error { return nil }
func (f *fakeEngine) Cancel(int, int) error             { return nil }
func (f *fakeEngine) Close() error                      { return nil }
func (f *fakeEngine) Approve(botID int, id string, allow bool, reason string) error {
	f.approved = append(f.approved, id)
	return nil
}

func TestApprovalQueueAndDecision(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := New()
	eng := &fakeEngine{}
	a.Engine = eng

	a.BotApproval(7, 3, Approval{ID: "ap-1", Target: "read_file", Reason: "需要审批", Timeout: time.Second})
	a.BotApproval(7, 3, Approval{ID: "ap-1", Target: "read_file", Reason: "重复"})
	if len(a.Approvals) != 1 {
		t.Fatalf("同一 id 不应重复入队: %+v", a.Approvals)
	}

	if err := a.DecideApproval("ap-1", true, "用户允许"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(a.Approvals) != 0 {
		t.Fatalf("决定后队列应为空: %+v", a.Approvals)
	}
	if len(eng.approved) != 1 || eng.approved[0] != "ap-1" {
		t.Fatalf("引擎没有收到审批结果: %+v", eng.approved)
	}

	// 超时后被 runtime 拒绝的请求：宿主这里找不到 id 时不应报错。
	if err := a.DecideApproval("ap-late", false, "用户拒绝"); err != nil {
		t.Fatalf("未知 id 不应报错: %v", err)
	}

	// 回合结束要清掉该轮残留的审批请求。
	a.BotApproval(7, 4, Approval{ID: "ap-2"})
	a.removeApprovals(7, 4)
	if len(a.Approvals) != 0 {
		t.Fatalf("回合结束后应清掉审批: %+v", a.Approvals)
	}
}
