// Package sched 是 M4 的调度基础:时钟抽象、5 段 cron 解析、按间隔/定时的一次性任务。
//
// 它属于 L2 基础设施(不是插件):只懂「时间」与「回调」,不懂任何业务;
// 插件通过宿主注入的服务使用它,内核不直接依赖它(guard 的 go list -deps 强制)。
package sched

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// Clock 是调度用到的全部时间来源。
//
// 为什么抽象成接口:调度测试必须能「把时间往前拨」,否则每个用例都要 sleep;
// 真实时间在测试里既慢又不稳定(sched 的契约测试只用 FakeClock)。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RealClock 是生产用的时钟。
type RealClock struct{}

// Now 返回当前时间。
func (RealClock) Now() time.Time { return time.Now() }

// After 在 d 之后向通道发送一次时间。
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock 是可手动推进的时钟。它只在测试与模拟(ada sim)里使用。
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
	changed chan struct{}
}

type fakeWaiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFakeClock 以 start 为起点创建假时钟。
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start, changed: make(chan struct{})}
}

// Now 返回当前(可被推进的)时间。
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After 注册一个等待者;到点由 Advance 触发。
func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.waiters = append(c.waiters, &fakeWaiter{deadline: c.now.Add(d), ch: ch})
	c.mu.Unlock()
	c.broadcast()
	return ch
}

// Advance 把时间往前拨 d,并触发所有到点的等待者。返回被触发的数量。
func (c *FakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	kept := c.waiters[:0]
	var due []*fakeWaiter
	for _, w := range c.waiters {
		if w.deadline.After(now) {
			kept = append(kept, w)
			continue
		}
		due = append(due, w)
	}
	c.waiters = kept
	c.mu.Unlock()

	for _, w := range due {
		select {
		case w.ch <- now:
		default:
		}
	}
	c.broadcast()
	return len(due)
}

// Waiters 返回当前等待中的任务数(诊断与测试同步用)。
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// WaitForWaiters 等到至少 n 个等待者注册;ctx 到期返回错误。
//
// 测试用它同步:没有这个,测试只能靠 sleep 猜「调度协程是否已经挂上定时器」。
func (c *FakeClock) WaitForWaiters(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		got := len(c.waiters)
		changed := c.changed
		c.mu.Unlock()
		if got >= n {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("sched: 等待定时器注册超时(期望 %d,实际 %d): %w", n, got, ctx.Err())
		}
	}
}

func (c *FakeClock) broadcast() {
	c.mu.Lock()
	ch := c.changed
	c.changed = make(chan struct{})
	c.mu.Unlock()
	close(ch)
}

// Options 配置调度器。
type Options struct {
	// Clock 缺省为 RealClock。
	Clock Clock
	// Logf 接收任务失败与 panic 的日志;nil 表示丢弃。
	Logf func(format string, args ...any)
}

// Scheduler 管理一组周期/定时任务,每个任务一个协程,任务内串行(不重叠)。
type Scheduler struct {
	clock Clock
	logf  func(format string, args ...any)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	jobs    map[uint64]*job
	seq     uint64
	stopped bool
}

// job 是一个注册的任务:dispose 会取消它并等它真正退出。
type job struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// New 创建调度器。
func New(opts Options) *Scheduler {
	clock := opts.Clock
	if clock == nil {
		clock = RealClock{}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		clock:  clock,
		logf:   logf,
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(map[uint64]*job),
	}
}

// Every 每 d 执行一次 fn(上一个 fn 返回后才开始计时,因此不会重叠)。
func (s *Scheduler) Every(d time.Duration, fn func(context.Context) error) (kernel.Dispose, error) {
	if d <= 0 {
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: Every 的间隔必须为正,得到 %s", d)
	}
	return s.add("every:"+d.String(), func(jobCtx context.Context) {
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-s.clock.After(d):
			}
			s.run("every:"+d.String(), jobCtx, fn)
		}
	})
}

// After 在 d 之后执行一次 fn。
func (s *Scheduler) After(d time.Duration, fn func(context.Context) error) (kernel.Dispose, error) {
	if d <= 0 {
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: After 的延迟必须为正,得到 %s", d)
	}
	return s.add("after:"+d.String(), func(jobCtx context.Context) {
		select {
		case <-jobCtx.Done():
			return
		case <-s.clock.After(d):
		}
		s.run("after:"+d.String(), jobCtx, fn)
	})
}

// Cron 按 5 段 cron 表达式执行 fn(每次触发后重新计算下一次,避免漂移累积)。
func (s *Scheduler) Cron(expr string, fn func(context.Context) error) (kernel.Dispose, error) {
	parsed, err := ParseCron(expr)
	if err != nil {
		return nil, err
	}
	return s.add("cron:"+expr, func(jobCtx context.Context) {
		for {
			next, ok := parsed.Next(s.clock.Now())
			if !ok {
				s.logf("sched: cron %q 在可搜索范围内没有下一次触发", expr)
				return
			}
			wait := next.Sub(s.clock.Now())
			if wait < 0 {
				wait = 0
			}
			select {
			case <-jobCtx.Done():
				return
			case <-s.clock.After(wait):
			}
			s.run("cron:"+expr, jobCtx, fn)
		}
	})
}

func (s *Scheduler) add(label string, loop func(jobCtx context.Context)) (kernel.Dispose, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: 调度器已停止,无法注册 %q", label)
	}
	jobCtx, cancel := context.WithCancel(s.ctx)
	j := &job{cancel: cancel, done: make(chan struct{})}
	s.seq++
	id := s.seq
	s.jobs[id] = j
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(j.done)
		loop(jobCtx)
	}()

	var once sync.Once
	dispose := func(context.Context) error {
		once.Do(func() {
			cancel()
			<-j.done
			s.mu.Lock()
			delete(s.jobs, id)
			s.mu.Unlock()
		})
		return nil
	}
	return dispose, nil
}

func (s *Scheduler) run(label string, ctx context.Context, fn func(context.Context) error) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logf("sched: 任务 %s panic: %v", label, rec)
		}
	}()
	if err := fn(ctx); err != nil {
		s.logf("sched: 任务 %s 失败: %v", label, err)
	}
}

// Jobs 返回仍在运行的任务数(诊断用)。
func (s *Scheduler) Jobs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Stop 停止全部任务并等待它们退出;ctx 到期返回超时错误。
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.cancel()
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.mu.Lock()
		s.jobs = make(map[uint64]*job)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sched: 停止超时: %w", ctx.Err())
	}
}
