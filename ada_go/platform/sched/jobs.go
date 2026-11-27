// Job 是 M4 的定时任务:5 段 cron + 补跑策略 + 租约。
//
// 与 Every/Cron 的区别:Job 带「错过补跑」与「多实例防重」语义,适合产品级
// 周期任务(每日清理、定期同步);普通周期任务仍用 Every。
package sched

import (
	"context"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// JobPolicy 是停机/重启后对错过的触发的处理策略。
type JobPolicy string

const (
	// JobSkip 错过的不补跑,直接等下一次。
	JobSkip JobPolicy = "skip"
	// JobOnce 补跑一次(最近一次错过),默认。
	JobOnce JobPolicy = "once"
	// JobAll 补跑全部错过的触发(上限 1000 次,防跑飞)。
	JobAll JobPolicy = "all"
)

// JobLease 是单次执行的租约时长:租约内其它实例不应重复执行。
const JobLease = 5 * time.Minute

// maxCatchUp 是补跑的上限,防止长时间停机后补跑爆炸。
const maxCatchUp = 1000

// JobSpec 是定时任务声明。
type JobSpec struct {
	ID      string
	Cron    string
	Policy  JobPolicy
	Handler func(ctx context.Context) error
}

// JobStore 记录任务的上次运行与租约。
type JobStore interface {
	// LastRun 返回上次运行时间;没有记录时 ok=false。
	LastRun(ctx context.Context, id string) (at time.Time, ok bool, err error)
	// SetLastRun 记录本次运行并释放租约。
	SetLastRun(ctx context.Context, id string, at time.Time) error
	// AcquireLease 尝试获取租约;已被占用(leaseUntil > now)时返回 false。
	AcquireLease(ctx context.Context, id string, now time.Time, ttl time.Duration) (bool, error)
}

// Job 注册一个定时任务;返回的 dispose 停止该任务。
func (s *Scheduler) Job(spec JobSpec, store JobStore) (kernel.Dispose, error) {
	if spec.ID == "" {
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: Job 的 ID 不能为空")
	}
	if spec.Handler == nil {
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: Job %q 的 Handler 不能为空", spec.ID)
	}
	if store == nil {
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: Job %q 需要 JobStore(补跑与租约依赖它)", spec.ID)
	}
	policy := spec.Policy
	if policy == "" {
		policy = JobOnce
	}
	switch policy {
	case JobSkip, JobOnce, JobAll:
	default:
		return nil, kernel.Fail(kernel.CodeInvalid, "sched: Job %q 的 Policy 非法: %q", spec.ID, policy)
	}
	parsed, err := ParseCron(spec.Cron)
	if err != nil {
		return nil, err
	}

	return s.add("job:"+spec.ID, func(jobCtx context.Context) {
		s.catchUpJob(jobCtx, spec.ID, parsed, policy, spec.Handler, store)
		for {
			next, ok := parsed.Next(s.clock.Now())
			if !ok {
				s.logf("sched: job %q 的 cron 没有下一次触发", spec.ID)
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
			s.runJob(jobCtx, spec.ID, spec.Handler, store, s.clock.Now())
		}
	})
}

// catchUpJob 处理停机期间错过的触发。
func (s *Scheduler) catchUpJob(ctx context.Context, id string, parsed *Cron, policy JobPolicy, handler func(context.Context) error, store JobStore) {
	last, ok, err := store.LastRun(ctx, id)
	if err != nil {
		s.logf("sched: job %q 读取上次运行失败: %v", id, err)
		return
	}
	if !ok {
		// 首次运行:不补跑历史,等下一次触发。
		return
	}
	now := s.clock.Now()
	var missed []time.Time
	cur := last
	for i := 0; i < maxCatchUp; i++ {
		next, ok := parsed.Next(cur)
		if !ok || next.After(now) {
			break
		}
		missed = append(missed, next)
		cur = next
	}
	if len(missed) == 0 {
		return
	}

	switch policy {
	case JobSkip:
		if err := store.SetLastRun(ctx, id, missed[len(missed)-1]); err != nil {
			s.logf("sched: job %q 推进 lastRun 失败: %v", id, err)
		}
		s.logf("sched: job %q 跳过 %d 次错过的触发", id, len(missed))
	case JobOnce:
		s.runJobAt(ctx, id, handler, store, missed[len(missed)-1])
	case JobAll:
		for _, at := range missed {
			if ctx.Err() != nil {
				return
			}
			s.runJobAt(ctx, id, handler, store, at)
		}
		s.logf("sched: job %q 补跑 %d 次错过的触发", id, len(missed))
	}
}

// runJob 以「当前时刻」作为 lastRun 执行。
func (s *Scheduler) runJob(ctx context.Context, id string, handler func(context.Context) error, store JobStore, at time.Time) {
	s.runJobAt(ctx, id, handler, store, at)
}

// runJobAt 先抢租约再执行;成功/失败都推进 lastRun(避免失败后无限重跑)。
func (s *Scheduler) runJobAt(ctx context.Context, id string, handler func(context.Context) error, store JobStore, at time.Time) {
	// 先看 lastRun:已经跑过这个触发点(另一个实例刚写过)→ 跳过,避免同一周期重复执行。
	if triggerDone(ctx, store, id, at) {
		return
	}
	now := s.clock.Now()
	held, err := store.AcquireLease(ctx, id, now, JobLease)
	if err != nil {
		s.logf("sched: job %q 获取租约失败: %v", id, err)
		return
	}
	if !held {
		s.logf("sched: job %q 的租约被其它实例持有,跳过本次", id)
		return
	}
	// 抢到租约后再确认一次:上面的「读 lastRun」与「抢租约」不是原子的,另一个实例
	// 可能正好在这两步之间跑完并写了 lastRun;此时放弃本次,并把刚拿到的租约清掉,
	// 否则下一次合法触发会被这段空租约挡住。
	if triggerDone(ctx, store, id, at) {
		// 用同一个 lastRun 再写一次:SetLastRun 会清空租约,值不变即幂等。
		last, ok, lerr := store.LastRun(ctx, id)
		if lerr == nil && ok {
			if err := store.SetLastRun(ctx, id, last); err != nil {
				s.logf("sched: job %q 释放空租约失败: %v", id, err)
			}
		}
		return
	}
	s.run("job:"+id, ctx, handler) // run 内部 recover + 日志;失败也推进 lastRun。
	if err := store.SetLastRun(ctx, id, at); err != nil {
		s.logf("sched: job %q 写入 lastRun 失败: %v", id, err)
	}
}

// triggerDone 报告这个触发点是否已经执行过:lastRun >= at 即视为完成。
func triggerDone(ctx context.Context, store JobStore, id string, at time.Time) bool {
	last, ok, err := store.LastRun(ctx, id)
	return err == nil && ok && !last.Before(at)
}

// JobState 是内存存储里单个任务的状态。
type JobState struct {
	LastRun    time.Time
	LeaseUntil time.Time
}

// MemoryJobStore 是内存 JobStore(测试与最小宿主使用)。
type MemoryJobStore struct {
	mu    sync.Mutex
	state map[string]JobState
}

// NewMemoryJobStore 创建内存任务存储。
func NewMemoryJobStore() *MemoryJobStore {
	return &MemoryJobStore{state: make(map[string]JobState)}
}

// LastRun 返回上次运行时间。
func (m *MemoryJobStore) LastRun(_ context.Context, id string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.state[id]
	return st.LastRun, ok && !st.LastRun.IsZero(), nil
}

// SetLastRun 记录运行时间并释放租约。
func (m *MemoryJobStore) SetLastRun(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[id]
	st.LastRun = at
	st.LeaseUntil = time.Time{}
	m.state[id] = st
	return nil
}

// AcquireLease 尝试获取租约。
func (m *MemoryJobStore) AcquireLease(_ context.Context, id string, now time.Time, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[id]
	if st.LeaseUntil.After(now) {
		return false, nil
	}
	st.LeaseUntil = now.Add(ttl)
	m.state[id] = st
	return true, nil
}

// State 返回某任务的当前状态(诊断/测试用)。
func (m *MemoryJobStore) State(id string) (JobState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.state[id]
	return st, ok
}
