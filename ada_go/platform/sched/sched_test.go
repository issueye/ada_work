package sched_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/sched"
)

func startClock() *sched.FakeClock {
	return sched.NewFakeClock(time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC))
}

func waitWaiters(t *testing.T, clk *sched.FakeClock, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := clk.WaitForWaiters(ctx, n); err != nil {
		t.Fatalf("等待定时器失败: %v", err)
	}
}

func recvInt(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatalf("等待回调超时")
		return 0
	}
}

// 1. Every:每次推进一个间隔触发一次;dispose 后不再触发。
func TestEveryFiresEachInterval(t *testing.T) {
	clk := startClock()
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	calls := make(chan int, 4)
	var n int
	dispose, err := s.Every(time.Second, func(ctx context.Context) error {
		n++
		calls <- n
		return nil
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	waitWaiters(t, clk, 1)
	clk.Advance(time.Second)
	if got := recvInt(t, calls); got != 1 {
		t.Fatalf("第一次触发应是 1,实际 %d", got)
	}
	waitWaiters(t, clk, 1)
	clk.Advance(time.Second)
	if got := recvInt(t, calls); got != 2 {
		t.Fatalf("第二次触发应是 2,实际 %d", got)
	}

	if err := dispose(context.Background()); err != nil {
		t.Fatalf("dispose 失败: %v", err)
	}
	clk.Advance(time.Second) // 定时器可能仍登记在假时钟里,但已没有协程在等它
	select {
	case v := <-calls:
		t.Fatalf("dispose 后不应再触发,实际收到 %d", v)
	case <-time.After(200 * time.Millisecond):
	}
}

// 2. 任务内串行:上一次没返回前不会重叠执行。
func TestEveryDoesNotOverlap(t *testing.T) {
	clk := startClock()
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	gate := make(chan struct{})
	started := make(chan int, 4)
	var n int
	var mu sync.Mutex
	_, err := s.Every(time.Second, func(ctx context.Context) error {
		mu.Lock()
		n++
		cur := n
		mu.Unlock()
		started <- cur
		<-gate
		return nil
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	waitWaiters(t, clk, 1)
	clk.Advance(time.Second)
	if got := recvInt(t, started); got != 1 {
		t.Fatalf("应先执行第一次,实际 %d", got)
	}
	// 回调阻塞中:再次推进时间不应产生第二个等待者/第二次执行。
	if triggered := clk.Advance(time.Second); triggered != 0 {
		t.Fatalf("回调未返回时不应触发新的定时器,实际 %d", triggered)
	}
	close(gate)

	waitWaiters(t, clk, 1)
	clk.Advance(time.Second)
	if got := recvInt(t, started); got != 2 {
		t.Fatalf("释放后应执行第二次,实际 %d", got)
	}
}

// 3. After 只触发一次。
func TestAfterFiresOnce(t *testing.T) {
	clk := startClock()
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	calls := make(chan int, 4)
	var n int
	if _, err := s.After(2*time.Second, func(ctx context.Context) error {
		n++
		calls <- n
		return nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	waitWaiters(t, clk, 1)
	clk.Advance(2 * time.Second)
	if got := recvInt(t, calls); got != 1 {
		t.Fatalf("After 应触发一次,实际 %d", got)
	}
	if triggered := clk.Advance(time.Second); triggered != 0 {
		t.Fatalf("After 触发后不应再有定时器,实际 %d", triggered)
	}
}

// 4. Cron 按表达式触发。
func TestCronFires(t *testing.T) {
	clk := startClock() // 10:00:30
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	calls := make(chan time.Time, 4)
	if _, err := s.Cron("* * * * *", func(ctx context.Context) error {
		calls <- clk.Now()
		return nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	waitWaiters(t, clk, 1)
	clk.Advance(30 * time.Second) // 10:01:00
	select {
	case at := <-calls:
		if !at.Equal(time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)) {
			t.Fatalf("Cron 应在 10:01 触发,实际 %s", at)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Cron 未触发")
	}
	waitWaiters(t, clk, 1)
	clk.Advance(time.Minute) // 10:02:00
	select {
	case at := <-calls:
		if !at.Equal(time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC)) {
			t.Fatalf("Cron 应在 10:02 触发,实际 %s", at)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Cron 第二次未触发")
	}
}

// 5. 任务 panic 不拖垮调度器,后续仍然触发;错误与 panic 都写日志。
func TestPanicDoesNotKillScheduler(t *testing.T) {
	clk := startClock()
	var logs []string
	var mu sync.Mutex
	s := sched.New(sched.Options{
		Clock: clk,
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	calls := make(chan int, 4)
	var n int
	if _, err := s.Every(time.Second, func(ctx context.Context) error {
		n++
		if n == 1 {
			panic("boom")
		}
		calls <- n
		return nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	waitWaiters(t, clk, 1)
	clk.Advance(time.Second) // 第一次 panic

	waitWaiters(t, clk, 1)
	clk.Advance(time.Second) // 第二次正常
	if got := recvInt(t, calls); got != 2 {
		t.Fatalf("panic 后任务应继续,实际 %d", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatalf("panic 应被记录")
	}
}

// 6. Stop 之后不能再注册,也不再有触发。
func TestStopStopsEverything(t *testing.T) {
	clk := startClock()
	s := sched.New(sched.Options{Clock: clk})
	calls := make(chan int, 4)
	if _, err := s.Every(time.Second, func(ctx context.Context) error {
		calls <- 1
		return nil
	}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if got := s.Jobs(); got != 0 {
		t.Fatalf("Stop 后不应有任务,实际 %d", got)
	}
	clk.Advance(time.Second) // 假时钟里可能仍登记着旧等待者,但已没有协程在监听
	select {
	case <-calls:
		t.Fatalf("Stop 后不应再触发")
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := s.Every(time.Second, func(context.Context) error { return nil }); err == nil {
		t.Fatalf("Stop 后注册应报错")
	}
}

// 7. 非法参数与非法 cron 报错,而不是静默成功。
func TestInvalidRegistrations(t *testing.T) {
	clk := startClock()
	s := sched.New(sched.Options{Clock: clk})
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	if _, err := s.Every(0, func(context.Context) error { return nil }); err == nil {
		t.Fatalf("Every(0) 应报错")
	}
	if _, err := s.After(-time.Second, func(context.Context) error { return nil }); err == nil {
		t.Fatalf("After(负数) 应报错")
	}
	if _, err := s.Cron("bad spec", func(context.Context) error { return nil }); err == nil {
		t.Fatalf("坏 cron 应报错")
	}
}
