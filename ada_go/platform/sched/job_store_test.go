package sched_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/sched"
)

func newFileStore(t *testing.T) (*sched.FileJobStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "jobs.json")
	store, err := sched.NewFileJobStore(path)
	if err != nil {
		t.Fatalf("创建文件存储失败: %v", err)
	}
	return store, path
}

// 1. 文件存储:状态跨实例可见,租约通过文件生效。
func TestFileJobStoreLeaseAcrossInstances(t *testing.T) {
	store, path := newFileStore(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	if ok, err := store.AcquireLease(ctx, "job", now, time.Minute); err != nil || !ok {
		t.Fatalf("首次应抢到租约: ok=%v err=%v", ok, err)
	}

	other, err := sched.NewFileJobStore(path)
	if err != nil {
		t.Fatalf("打开另一个实例失败: %v", err)
	}
	if ok, err := other.AcquireLease(ctx, "job", now, time.Minute); err != nil || ok {
		t.Fatalf("租约生效时不应再次抢到: ok=%v err=%v", ok, err)
	}

	if err := store.SetLastRun(ctx, "job", now); err != nil {
		t.Fatalf("写 lastRun 失败: %v", err)
	}
	if ok, err := other.AcquireLease(ctx, "job", now, time.Minute); err != nil || !ok {
		t.Fatalf("SetLastRun 应释放租约: ok=%v err=%v", ok, err)
	}
	if last, ok, err := other.LastRun(ctx, "job"); err != nil || !ok || !last.Equal(now) {
		t.Fatalf("lastRun 未跨实例可见: %s ok=%v err=%v", last, ok, err)
	}
}

// 2. 原子写:目录里只留下目标文件,没有临时文件残留。
func TestFileJobStoreAtomicWrite(t *testing.T) {
	store, path := newFileStore(t)
	if err := store.SetLastRun(context.Background(), "job", time.Now()); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "jobs.json" {
		t.Fatalf("目录里只应有 jobs.json: %v", entries)
	}
}

// 3. 存储损坏 → 明确报错,不是静默当成空。
func TestFileJobStoreCorrupted(t *testing.T) {
	store, path := newFileStore(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("写坏文件失败: %v", err)
	}
	if _, _, err := store.LastRun(context.Background(), "job"); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("损坏的存储应报错,实际 %v", err)
	}
}

// 4. 首次使用:文件不存在时为空状态,写入后创建文件。
func TestFileJobStoreFresh(t *testing.T) {
	store, path := newFileStore(t)
	if _, ok, err := store.LastRun(context.Background(), "job"); err != nil || ok {
		t.Fatalf("空存储应无记录: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcquireLease(context.Background(), "job", time.Now(), time.Minute); err != nil || !ok {
		t.Fatalf("空存储应能抢租约: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("写入后应创建文件: %v", err)
	}
}

// 5. 两个调度器共享同一 store:同一触发点只执行一次,补跑也只执行一次。
func TestJobsSharedStoreRunsOnce(t *testing.T) {
	store, _ := newFileStore(t)
	ctx := context.Background()
	if err := store.SetLastRun(ctx, "job", time.Date(2026, 1, 1, 10, 2, 0, 0, time.UTC)); err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	clockA := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	clockB := sched.NewFakeClock(time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC))
	sA := sched.New(sched.Options{Clock: clockA})
	sB := sched.New(sched.Options{Clock: clockB})
	t.Cleanup(func() {
		_ = sA.Stop(context.Background())
		_ = sB.Stop(context.Background())
	})

	var runs int32
	handler := func(context.Context) error { atomic.AddInt32(&runs, 1); return nil }
	for _, s := range []*sched.Scheduler{sA, sB} {
		if _, err := s.Job(sched.JobSpec{ID: "job", Cron: "* * * * *", Policy: sched.JobOnce, Handler: handler}, store); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
	}
	waitWaiters(t, clockA, 1)
	waitWaiters(t, clockB, 1)
	if got := atomic.LoadInt32(&runs); got != 1 {
		t.Fatalf("补跑应只执行一次,实际 %d", got)
	}

	// 同一分钟两个调度器都触发:同一触发点仍只执行一次。
	clockA.Advance(time.Minute)
	clockB.Advance(time.Minute)
	waitWaiters(t, clockA, 1)
	waitWaiters(t, clockB, 1)
	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Fatalf("下一分钟应只再执行一次,总次数 %d", got)
	}
}

// 6. 跨进程锁:两个 store 实例并发抢同一租约,只有 1 个成功。
func TestFileJobStoreCrossProcessLock(t *testing.T) {
	_, path := newFileStore(t)
	a, err := sched.NewFileJobStore(path)
	if err != nil {
		t.Fatalf("创建 A 失败: %v", err)
	}
	b, err := sched.NewFileJobStore(path)
	if err != nil {
		t.Fatalf("创建 B 失败: %v", err)
	}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	var (
		wg      sync.WaitGroup
		succeed atomic.Int32
		start   = make(chan struct{})
	)
	try := func(s *sched.FileJobStore) {
		defer wg.Done()
		<-start
		if ok, err := s.AcquireLease(context.Background(), "job", now, time.Minute); err == nil && ok {
			succeed.Add(1)
		}
	}
	wg.Add(2)
	go try(a)
	go try(b)
	close(start)
	wg.Wait()
	if got := succeed.Load(); got != 1 {
		t.Fatalf("并发抢租约应只有 1 个成功,实际 %d", got)
	}

	// 释放租约后另一个实例可以抢到。
	if err := a.SetLastRun(context.Background(), "job", now); err != nil {
		t.Fatalf("释放租约失败: %v", err)
	}
	if ok, err := b.AcquireLease(context.Background(), "job", now, time.Minute); err != nil || !ok {
		t.Fatalf("释放后应可抢到: ok=%v err=%v", ok, err)
	}
}

// 7. 过期锁(超过 JobLease)会被回收;锁文件在释放后不存在。
func TestFileJobStoreStaleLock(t *testing.T) {
	store, path := newFileStore(t)
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(lock, []byte("stale"), 0o644); err != nil {
		t.Fatalf("写锁失败: %v", err)
	}
	old := time.Now().Add(-2 * sched.JobLease)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatalf("改锁时间失败: %v", err)
	}
	if ok, err := store.AcquireLease(context.Background(), "job", time.Now(), time.Minute); err != nil || !ok {
		t.Fatalf("过期锁应被回收: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Fatalf("释放后锁文件不应存在")
	}
}

// 8. 锁被其它进程持有时,SetLastRun 明确报错而不是静默覆盖。
func TestFileJobStoreSetLastRunLockConflict(t *testing.T) {
	store, path := newFileStore(t)
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(lock, []byte("held"), 0o644); err != nil {
		t.Fatalf("写锁失败: %v", err)
	}
	if err := store.SetLastRun(context.Background(), "job", time.Now()); err == nil || !strings.Contains(err.Error(), "锁") {
		t.Fatalf("锁被持有应报错,实际 %v", err)
	}
	_ = os.Remove(lock)
}
