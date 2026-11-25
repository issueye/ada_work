package quota_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/quota"
	"github.com/issueye/ada_go_v2/platform/sched"
)

func base() time.Time { return time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC) }

// 1. 未配置配额的 key 一律放行:配额是「按需收紧」,不是默认拒绝。
func TestUnconfiguredIsAllowed(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()))
	release, err := g.Acquire(context.Background(), "unknown")
	if err != nil || release == nil {
		t.Fatalf("未配置配额应放行: %v", err)
	}
	release()
}

// 2. 并发上限:满了返回 EBUSY,释放后可再取。
func TestConcurrencyLimit(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()))
	if err := g.Set("p", quota.Quota{MaxConcurrent: 2}); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	r1, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("第一次申请失败: %v", err)
	}
	r2, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("第二次申请失败: %v", err)
	}

	_, err = g.Acquire(context.Background(), "p")
	if !quota.Is(err, quota.CodeBusy) {
		t.Fatalf("并发满应返回 EBUSY,实际 %v", err)
	}
	var qe *quota.Error
	if !errors.As(err, &qe) || !qe.Retryable {
		t.Fatalf("EBUSY 应可重试: %+v", qe)
	}

	r1()
	release, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("释放后应可再申请: %v", err)
	}
	release()
	r2()
}

// 3. 每分钟调用上限:用尽返回 EQUOTA + 退避时间;窗口重置后可再申请。
func TestPerMinuteLimit(t *testing.T) {
	clk := sched.NewFakeClock(base())
	g := quota.NewGovernor(clk)
	if err := g.Set("p", quota.Quota{MaxCallsPerMinute: 2}); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		release, err := g.Acquire(context.Background(), "p")
		if err != nil {
			t.Fatalf("第 %d 次申请失败: %v", i+1, err)
		}
		release()
	}

	_, err := g.Acquire(context.Background(), "p")
	if !quota.Is(err, quota.CodeQuota) {
		t.Fatalf("速率用尽应返回 EQUOTA,实际 %v", err)
	}
	var qe *quota.Error
	if !errors.As(err, &qe) || !qe.Retryable {
		t.Fatalf("EQUOTA 应可重试: %+v", qe)
	}
	if qe.RetryAfter <= 0 || qe.RetryAfter > time.Minute {
		t.Fatalf("退避时间应在 (0, 1m],实际 %s", qe.RetryAfter)
	}

	clk.Advance(time.Minute)
	release, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("窗口重置后应可申请: %v", err)
	}
	release()
	if stats := g.Snapshot()["p"]; stats.CallsInWindow != 1 || stats.InFlight != 0 {
		t.Fatalf("窗口重置后的计数不正确: %+v", stats)
	}
}

// 4. release 幂等:重复调用不会把计数减成负数。
func TestReleaseIdempotent(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()))
	if err := g.Set("p", quota.Quota{MaxConcurrent: 1}); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	release, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	release()
	release()
	if stats := g.Snapshot()["p"]; stats.InFlight != 0 {
		t.Fatalf("重复 release 后并发计数应为 0,实际 %d", stats.InFlight)
	}
	if _, err := g.Acquire(context.Background(), "p"); err != nil {
		t.Fatalf("release 后应可再申请: %v", err)
	}
}

// 5. 非法配置在 Set 时被拒绝,而不是留到运行期。
func TestValidate(t *testing.T) {
	g := quota.NewGovernor(nil)
	if err := g.Set("p", quota.Quota{MaxCallsPerMinute: -1}); err == nil {
		t.Fatalf("负配额应被拒绝")
	}
	if err := g.Set("", quota.Quota{MaxConcurrent: 1}); err == nil {
		t.Fatalf("空 key 应被拒绝")
	}
}

// 6. 并发申请(带 -race):并发上限必须被真正守住。
func TestConcurrentAcquire(t *testing.T) {
	const maxConcurrent = 8
	const attempts = 64

	g := quota.NewGovernor(sched.NewFakeClock(base()))
	if err := g.Set("p", quota.Quota{MaxConcurrent: maxConcurrent, MaxCallsPerMinute: attempts}); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	var (
		wg       sync.WaitGroup
		success  atomic.Int32
		start    = make(chan struct{})
		releases = make(chan func(), attempts)
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := g.Acquire(context.Background(), "p")
			if err == nil {
				success.Add(1)
				releases <- release
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := success.Load(); got > maxConcurrent {
		t.Fatalf("并发上限应 ≤ %d,实际成功 %d", maxConcurrent, got)
	}
	for i := 0; i < int(success.Load()); i++ {
		(<-releases)()
	}
	if stats := g.Snapshot()["p"]; stats.InFlight != 0 {
		t.Fatalf("全部释放后并发计数应为 0,实际 %d", stats.InFlight)
	}
}

// 7. 默认全局并发上限:min(4*GOMAXPROCS, 64)。
func TestDefaultGlobalLimit(t *testing.T) {
	g := quota.NewGovernor(nil)
	if got := g.Global().Limit; got != min(4*runtime.GOMAXPROCS(0), 64) {
		t.Fatalf("默认全局上限不正确: %d", got)
	}
}

// 8. 全局上限对未配置 quota 的 key 同样生效。
func TestGlobalLimitBlocks(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()), quota.WithGlobalLimit(2))
	r1, err := g.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("第一次申请失败: %v", err)
	}
	r2, err := g.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatalf("第二次申请失败: %v", err)
	}
	_, err = g.Acquire(context.Background(), "c")
	if !quota.Is(err, quota.CodeBusy) {
		t.Fatalf("超过全局上限应 EBUSY,实际 %v", err)
	}
	if !strings.Contains(err.Error(), "全局") {
		t.Fatalf("错误应指明是全局上限: %v", err)
	}
	r1()
	if _, err := g.Acquire(context.Background(), "c"); err != nil {
		t.Fatalf("释放后应可再申请: %v", err)
	}
	r2()
}

// 9. per-key 配额与全局上限叠加。
func TestGlobalWithPerKey(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()), quota.WithGlobalLimit(3))
	if err := g.Set("a", quota.Quota{MaxConcurrent: 2}); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	r1, err := g.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("a1 失败: %v", err)
	}
	r2, err := g.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("a2 失败: %v", err)
	}
	if _, err := g.Acquire(context.Background(), "a"); !quota.Is(err, quota.CodeBusy) {
		t.Fatalf("per-key 上限应 EBUSY,实际 %v", err)
	}
	if _, err := g.Acquire(context.Background(), "b"); err != nil {
		t.Fatalf("全局还有余量,b 应放行: %v", err)
	}
	if _, err := g.Acquire(context.Background(), "c"); !quota.Is(err, quota.CodeBusy) {
		t.Fatalf("全局上限应 EBUSY,实际 %v", err)
	}
	r1()
	r2()
}

// 10. 全局 release 幂等;WithGlobalLimit(0) 表示不限。
func TestGlobalReleaseAndUnlimited(t *testing.T) {
	g := quota.NewGovernor(sched.NewFakeClock(base()), quota.WithGlobalLimit(1))
	release, err := g.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	release()
	release()
	if got := g.Global().Inflight; got != 0 {
		t.Fatalf("重复 release 后全局计数应为 0,实际 %d", got)
	}
	if _, err := g.Acquire(context.Background(), "a"); err != nil {
		t.Fatalf("释放后应可申请: %v", err)
	}

	unlimited := quota.NewGovernor(nil, quota.WithGlobalLimit(0))
	var releases []func()
	for i := 0; i < 70; i++ {
		release, err := unlimited.Acquire(context.Background(), "x")
		if err != nil {
			t.Fatalf("不限全局时应放行: %v", err)
		}
		releases = append(releases, release)
	}
	for _, r := range releases {
		r()
	}
	if got := unlimited.Global().Inflight; got != 0 {
		t.Fatalf("全部释放后全局计数应为 0,实际 %d", got)
	}
}

// 11. 全局并发上限在 -race 下真正守住。
func TestGlobalConcurrentAcquire(t *testing.T) {
	const limit = 8
	const attempts = 64

	g := quota.NewGovernor(sched.NewFakeClock(base()), quota.WithGlobalLimit(limit))
	var (
		wg       sync.WaitGroup
		success  atomic.Int32
		start    = make(chan struct{})
		releases = make(chan func(), attempts)
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := g.Acquire(context.Background(), "p")
			if err == nil {
				success.Add(1)
				releases <- release
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := success.Load(); got > limit {
		t.Fatalf("全局上限应 ≤ %d,实际成功 %d", limit, got)
	}
	for i := 0; i < int(success.Load()); i++ {
		(<-releases)()
	}
	if got := g.Global().Inflight; got != 0 {
		t.Fatalf("全部释放后全局计数应为 0,实际 %d", got)
	}
}
