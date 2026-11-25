# M4 编排(第一刀:sched)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §12.2(交互编排)、§12.3(调度编排)、§12.4(组合编排)。
> 本页记录**已实现**的部分与**已知边界**;flow / 激活回收 / 配额 / jobs 的补跑策略都不在这一刀里。

## 1. 范围

M4 要回答「插件之间怎么按时间与顺序协作」。第一刀交付的是**调度的纯计算底座**:

```
RealClock / FakeClock ──▶ Scheduler
                             ├─ Every(d, fn)   周期(上次返回后才计时 → 不重叠)
                             ├─ After(d, fn)   一次性
                             └─ Cron(expr, fn) 5 段表达式,每次触发后重算下一次
```

它是 **L2 基础设施(不是插件)**:只懂「时间」与「回调」,不懂业务;内核不依赖它
(guard 的 `go list -deps ./kernel` 强制)。插件通过宿主注入的服务使用它;配额与回压由同层的 `quota.Governor` 提供(见 §4);声明式流水线见 [m4-flow.md](m4-flow.md)。

## 2. API

| 符号 | 语义 |
|---|---|
| `Clock` | `Now()` + `After(d)`;真实实现 `RealClock`,测试/模拟用 `FakeClock` |
| `NewFakeClock(start)` | 可 `Advance(d)` 的假时钟;`WaitForWaiters(ctx,n)` 用于确定性同步 |
| `New(Options{Clock,Logf})` | 创建调度器;每个任务一个协程 |
| `Every(d, fn)` | 周期任务;`fn` 返回后才开始下一个间隔(天然不重叠) |
| `After(d, fn)` | 一次性任务 |
| `Cron(expr, fn)` | 5 段 cron;触发前用 `Now()` 重算下一次,避免漂移累积 |
| `Stop(ctx)` | 取消全部任务并等待退出;`ctx` 到期返回超时错误 |
| `Jobs()` | 当前任务数(诊断) |

注册返回 `kernel.Dispose`(P5:注册必须有主);dispose 取消任务并**等它退出**后才返回。

## 3. 语义要点

- **不重叠**:`Every` 的同一个任务内串行;`fn` 返回前不会启动下一次。需要并发由调用方显式 fork。
- **panic 不拖垮调度器**:单次 `fn` 的 panic 被 recover 并记日志,后续照常触发。
- **错误不中断**:`fn` 返回 error 只记日志,不停止任务(停止与否由调用方在 `fn` 里决定)。
- **cron 语义**:5 段(分 时 日 月 周);支持 `*`、`*/step`、`a-b`、`a-b/step`、逗号列表、月份/星期英文缩写;
  周日的 0 与 7 等价;**日与周都受限时任一满足**(标准 cron 语义);5 年内无解返回 false(如 2 月 30 日)。
- **严格晚于**:`Next(after)` 从 `after` 的下一分钟开始,不会在同一分钟触发两次。
- **错误码**:非法参数/非法 cron 一律 `EINVALID`(走 `kernel.Fail`),不做「尽量理解」。
- **FakeClock 不撤销等待者**:`Advance` 会把旧等待者计数并触发通道,但对应任务可能已经取消——
  测试断言应看「回调是否发生」,而不是「触发了几个等待者」。

## 4. 配额与回压(quota)

`quota.Governor` 按 key(插件 id)记账,供宿主/调用方在调用前申请许可:

| API | 语义 |
|---|---|
| `Quota{MaxCallsPerMinute, MaxConcurrent}` | 0 表示不限;`MaxCPUTime`/`MaxMemory` 是预留字段(需计时/计量,暂不生效) |
| `WithGlobalLimit(n)` | 全局并发上限(默认 `min(4*GOMAXPROCS, 64)`);对未配置 key 同样生效 |
| `Set(key, Quota)` | 设置并重置该 key 的窗口与计数;非法值在 Set 时拒绝 |
| `Acquire(ctx, key)` | 返回 `release`;超出速率 → `EQUOTA`(可重试 + `RetryAfter`),并发满 → `EBUSY`(可重试) |
| `Snapshot()` | 诊断:窗口内调用数 / 在途并发 / 窗口结束时间 |

语义:

- **未配置的 key 不限制 per-key 速率/并发**,但仍计入全局并发上限;全局超限与 per-key 一样返回 `EBUSY`(可重试);
- 窗口按 `sched.Clock` 计算(测试用 FakeClock 推进,不依赖真实时间);
- `release` 幂等:重复调用不会把并发计数减成负数;
- 配额是**服务侧约束**,因此放在 L2 基础设施;调用方按错误码选择退避或降级。

## 5. 定时任务(jobs)

`sched.Scheduler.Job(spec, store)` 在 `Cron` 之上加两件事:

| 能力 | 语义 |
|---|---|
| `JobPolicy` | `skip`(不补跑)/ `once`(补最近一次,默认)/ `all`(补全部,上限 1000 次) |
| `JobStore` | `LastRun` / `SetLastRun`(同时释放租约)/ `AcquireLease`(原子抢租约) |
| 租约 | 单次执行 `JobLease=5m`;被其它实例持有时跳过本次,**不推进 lastRun** |
| 首次运行 | 没有 lastRun 时不补跑历史,等下一次触发 |

- 补跑发生在任务启动时(停机/重启后),之后按 cron 正常触发;
- `SetLastRun` 同时释放租约;成功与失败都推进 lastRun(避免失败后无限重跑);
- `MemoryJobStore`(测试/最小宿主)与 `FileJobStore`(JSON 文件 + 原子写 + `O_CREATE|O_EXCL` 锁文件)两种实现;文件存储跨实例共享状态,**并发抢租约只会有 1 个成功**,过期锁(超过 JobLease 的 mtime)自动回收。

## 6. 测试矩阵

| 场景 | 用例 |
|---|---|
| cron 通配/步长/范围/列表/月份与星期缩写/闰日 | `sched.TestCronNext` |
| dom 与 dow 都受限时取先到的 | `sched.TestCronDOMOrDOW` |
| 永不存在的日期 5 年内返回 false | `sched.TestCronImpossibleDate` |
| 非法 cron 表达式全部拒绝 | `sched.TestCronRejectsBadSpecs` |
| `Next` 严格晚于 `after` | `sched.TestCronStrictlyAfter` |
| Every 每次推进触发一次、dispose 后停止 | `sched.TestEveryFiresEachInterval` |
| 任务内不重叠 | `sched.TestEveryDoesNotOverlap` |
| After 只触发一次 | `sched.TestAfterFiresOnce` |
| Cron 按表达式触发 | `sched.TestCronFires` |
| panic 后继续 | `sched.TestPanicDoesNotKillScheduler` |
| Stop 后不再触发且拒绝新注册 | `sched.TestStopStopsEverything` |
| 并发上限 / 速率上限 / 窗口重置 / release 幂等 / 并发申请(-race) | `quota` 6 个用例 |
| 全局并发上限默认值/阻断/与 per-key 叠加/不限值/-race | `quota` 的 5 个全局 governor 用例 |
| 非法参数/非法 cron 报错 | `sched.TestInvalidRegistrations` |
| jobs 补跑策略(skip/once/all)与首次运行 | `sched.TestJobCatchUpOnce`、`TestJobCatchUpAll`、`TestJobCatchUpSkip`、`TestJobFirstRunWaitsNext` |
| 租约占用/释放、非法声明 | `sched.TestJobLeaseBlocks`、`TestMemoryJobStoreLease`、`TestJobValidation` |
| FileJobStore:跨实例可见/原子写/损坏报错/空存储 | `sched.TestFileJobStoreLeaseAcrossInstances`、`TestFileJobStoreAtomicWrite`、`TestFileJobStoreCorrupted`、`TestFileJobStoreFresh` |
| 两个调度器共享 store:同一触发点只执行一次 | `sched.TestJobsSharedStoreRunsOnce` |
| 跨进程锁:并发抢租约只有一个成功 | `sched.TestFileJobStoreCrossProcessLock` |
| 过期锁回收、锁冲突时 SetLastRun 报错 | `sched.TestFileJobStoreStaleLock`、`TestFileJobStoreSetLastRunLockConflict` |

## 7. 已知边界与下一步

1. **`flow` 已做顺序/并发子集**:use/event/parallel/forEach/compensate/when/retry/timeout/checkpoint/resume 已落地(见 [m4-flow.md](m4-flow.md));fanout/subflow 未做。
2. **激活与闲置回收未做**:§12.3 的 `onService/onCron/...` 延迟激活、`idleTTL` 回收需要 kernel 侧调度挂钩。
3. **配额做了速率/并发/全局上限**:`WithGlobalLimit` 已实现(默认 `min(4*GOMAXPROCS,64)`);`MaxCPUTime`/`MaxMemory` 需要计量,仍未生效。
4. **jobs 已做补跑/租约/文件存储/跨进程锁**:`skip/once/all` + `MemoryJobStore`/`FileJobStore`;锁文件用 `O_CREATE|O_EXCL` + mtime 过期回收,但依赖文件系统语义(不支持 O_EXCL 的网络文件系统不保证),也不是带 owner 校验的完整分布式锁。
5. **时区**:cron 用传入 `time.Location` 计算;`Asia/Shanghai` 这类 IANA 时区由宿主的 `Clock.Now()` 负责。
6. 与内核的接线未做:目前 `sched` 只能由宿主/插件在代码里使用,还没有作为服务暴露。

## 8. 验证命令

```powershell
go test -race -count=1 ./sched
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
