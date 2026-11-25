package sched_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/sched"
)

func mustStore(t *testing.T, last time.Time) *sched.MemoryJobStore {
	t.Helper()
	store := sched.NewMemoryJobStore()
	if !last.IsZero() {
		if err := store.SetLastRun(context.Background(), "job", last); err != nil {
			t.Fatalf("预置 lastRun 失败: %v", err)
		}
	}
	return store
}

func mustJob(t *testing.T, s *sched.Scheduler, store sched.JobStore, policy sched.JobPolicy, calls chan<- struct{}) {
	t.Helper()
	if _, err := s.Job(sched.JobSpec{ID: "job", Cron: "* * * * *", Policy: policy,
		Handler: func(context.Context) error { calls <- struct{}{}; return nil },
	}, store); err != nil {
		t.Fatalf("注册 job 失败: %v", err)
	}
}

func recvCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待 job 执行超时")
	}
}

// 1. once:停机期间错过 3 次 → 只补跑一次,并推进 lastRun。
func TestJobCatchUpOnce(t *testing.T) {
	clk := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	store := mustStore(t, time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC))
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	calls := make(chan struct{}, 4)
	mustJob(t, s, store, sched.JobOnce, calls)

	recvCall(t, calls)
	waitWaiters(t, clk, 1) // catch-up 完成并进入等待后才断言状态
	if st, _ := store.State("job"); st.LastRun != time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC) {
		t.Fatalf("once 应把 lastRun 推进到最近一次错过: %s", st.LastRun)
	}
	select {
	case <-calls:
		t.Fatalf("once 不应补跑多次")
	case <-time.After(100 * time.Millisecond):
	}
}

// 2. all:补跑全部错过的触发。
func TestJobCatchUpAll(t *testing.T) {
	clk := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	store := mustStore(t, time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC))
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	calls := make(chan struct{}, 4)
	mustJob(t, s, store, sched.JobAll, calls)

	for i := 0; i < 3; i++ {
		recvCall(t, calls)
	}
	waitWaiters(t, clk, 1)
	if st, _ := store.State("job"); st.LastRun != time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC) {
		t.Fatalf("all 后 lastRun 不正确: %s", st.LastRun)
	}
}

// 3. skip:不补跑,但推进 lastRun;下一次正常触发。
func TestJobCatchUpSkip(t *testing.T) {
	clk := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	store := mustStore(t, time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC))
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	calls := make(chan struct{}, 4)
	mustJob(t, s, store, sched.JobSkip, calls)

	waitWaiters(t, clk, 1)
	if st, _ := store.State("job"); st.LastRun != time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC) {
		t.Fatalf("skip 应推进 lastRun: %s", st.LastRun)
	}
	clk.Advance(time.Minute)
	recvCall(t, calls)
}

// 4. 租约被其它实例持有 → 本次不执行,lastRun 不变。
func TestJobLeaseBlocks(t *testing.T) {
	clk := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	store := mustStore(t, time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC))
	if ok, err := store.AcquireLease(context.Background(), "job", clk.Now(), sched.JobLease); err != nil || !ok {
		t.Fatalf("预占租约失败: ok=%v err=%v", ok, err)
	}
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	calls := make(chan struct{}, 4)
	mustJob(t, s, store, sched.JobOnce, calls)

	select {
	case <-calls:
		t.Fatalf("租约被持有时不应执行")
	case <-time.After(100 * time.Millisecond):
	}
	waitWaiters(t, clk, 1)
	if st, _ := store.State("job"); st.LastRun != time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC) {
		t.Fatalf("租约被拒时不应推进 lastRun: %s", st.LastRun)
	}
}

// 5. 首次运行没有记录:不补跑历史,等下一次触发。
func TestJobFirstRunWaitsNext(t *testing.T) {
	clk := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	store := mustStore(t, time.Time{})
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	calls := make(chan struct{}, 4)
	mustJob(t, s, store, sched.JobOnce, calls)

	select {
	case <-calls:
		t.Fatalf("首次运行不应补跑历史")
	case <-time.After(100 * time.Millisecond):
	}
	waitWaiters(t, clk, 1)
	clk.Advance(time.Minute)
	recvCall(t, calls)
}

// 6. 内存存储的租约语义:占用/释放。
func TestMemoryJobStoreLease(t *testing.T) {
	store := sched.NewMemoryJobStore()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if ok, _ := store.AcquireLease(context.Background(), "j", now, time.Minute); !ok {
		t.Fatalf("首次应抢到租约")
	}
	if ok, _ := store.AcquireLease(context.Background(), "j", now, time.Minute); ok {
		t.Fatalf("租约未过期时不应再次抢到")
	}
	if err := store.SetLastRun(context.Background(), "j", now); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if ok, _ := store.AcquireLease(context.Background(), "j", now, time.Minute); !ok {
		t.Fatalf("SetLastRun 应释放租约")
	}
}

// 7. 非法声明在注册期拒绝。
func TestJobValidation(t *testing.T) {
	s := sched.New(sched.Options{Clock: sched.NewFakeClock(time.Now())})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	handler := func(context.Context) error { return nil }
	store := sched.NewMemoryJobStore()

	cases := []sched.JobSpec{
		{ID: "", Cron: "* * * * *", Handler: handler},
		{ID: "j", Cron: "bad", Handler: handler},
		{ID: "j", Cron: "* * * * *", Handler: nil},
		{ID: "j", Cron: "* * * * *", Policy: "sometimes", Handler: handler},
	}
	for _, spec := range cases {
		if _, err := s.Job(spec, store); err == nil {
			t.Fatalf("非法声明应被拒绝: %+v", spec)
		}
	}
	if _, err := s.Job(sched.JobSpec{ID: "j", Cron: "* * * * *", Handler: handler}, nil); err == nil {
		t.Fatalf("缺少 Store 应被拒绝")
	}
}
