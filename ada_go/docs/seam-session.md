# seam/session、session-memory/jsonl、session-stats 与 compaction

> 契约来源:[seam-api.md](seam-api.md) §4。本页记录**已实现子集**、与契约的偏差、以及 provider 的约定;
> 未实现的部分明确列在 §5。

## 1. 范围

```
plugins/seam/seamerr/            跨 seam 的统一错误码(seam-api §1.2 子集)
plugins/seam/session/            session seam:类型 + 接口 + Registry + seam 插件
plugins/seam/session/sessiontest/ 契约测试(provider 必须全绿)
plugins/provider/session-memory/ 内存 provider + 内核插件
plugins/provider/session-jsonl/  文件 provider:一个会话一个 JSONL + 每会话 actor
plugins/provider/session-jsonl/journal/ 持久化抽象(Store / Journal)
plugins/provider/session-jsonl/file/    JSONL 文件后端
plugins/core/session-stats/     turn-stats 投影:轮次/消息/工具/token 统计
plugins/core/compaction-basic/  确定性摘要 + compact/summary
```

装载方式(kernel 服务模型):

```
seam/session 插件 ──provide "session"──▶ *session.Registry
memory/jsonl 插件 ──inject "session"──▶ Registry.Register(Provider)
消费者 ──inject "session"──▶ Registry.Open(name, meta) ──▶ Session
```

**为什么 seam 也是插件**:产品可以只装 seam 不装实现;没有 provider 时消费者拿到的是
`EUNIMPLEMENTED`(而不是「服务不存在」导致 fiber 永远 `pending`)。

## 2. 契约(已实现)

```go
type Session interface {
    ID() string
    Meta() Meta
    Append(events ...Event) ([]uint64, error)
    Events(from uint64, limit int) ([]Event, error)
    Subscribe(from uint64) (<-chan Event, kernel.Dispose)
    Turn() TurnInfo
    Fork(at uint64) (Session, kernel.Dispose, error)
    Compact(ctx context.Context, upTo uint64, summary Event) error
    Project(name string) (any, error)
}
```

- 事件类型常量与 `ModelVisible`(只有 `user/message`、`assistant/message`、`tool/result`、
  `compact/summary` 进入模型可见上下文)。
- `PluginEvent(id, kind)` 生成 `plugin:<id>/<kind>` 事件名;契约要求这类事件 `Ignorable=true`。
- `Registry` 的选择规则**与注册顺序无关**:默认取名字排序后的第一个 provider。
  「后注册覆盖先注册」会让装配顺序决定行为,这是装配层最忌讳的随机性。
- `Registry.Projections()` 持有投影注册表;provider 实现 `ProjectionBinder` 时,
  `Registry.Register` 会把投影表注入 provider,`Session.Project` 才能工作。
- `session.Resumer` 是 provider 的可选扩展:按既有 id 打开会话(`OpenID`)。
  `session-jsonl` 能恢复持久化会话,`session-memory` 只认识本进程仍打开的会话;
  未知 id 一律报错,不静默新建。控制层 `session.load` 在内存表缺失时经
  `agent.Resumer` 走这条路径。
- `session.WithSession` / `FromContext`:agent-loop 在 `Run` 开始时把当前会话放进
  ctx,供 Go 工具/策略追加控制面事件(`core/plan` 的计划事件)。作用域是一次 Run,
  不是跨会话索引;TS 工具拿不到它。

## 2.1 投影(projection)

```go
type Projection interface {
    Name() string
    Version() int
    Apply(state any, ev Event) (next any, changed bool)
    View(state any) any
}
```

- `Apply` 是纯函数、同步执行,不得有副作用;`changed=false` 表示状态不变。
- 投影缓存按 `(sessionID, projection)` 隔离,每次只重放上次 `seq` 之后的事件。
- `Version` 变化时缓存整体作废并重放。
- 投影未注册时 `Session.Project` 返回 `EUNIMPLEMENTED`;已注册但读事件失败则原样返回错误。
- `core/session-stats` 注册 `turn-stats`:轮次、消息、工具调用、token 估算、已压缩边界。
- 投影状态目前只在内存缓存,检查点持久化仍是下一刀。

## 3. session-memory

| 行为 | 约定 |
|---|---|
| `Open` | 每个会话独立;id 形如 `mem-1` |
| `Append` | 整批先校验再写入;seq 从 1 连续分配;补 `TS`/`SessionID`/`Source=core` |
| `Events` | `from` 是起始 seq(含);`limit<=0` 不限;返回副本 |
| `Subscribe` | 先回放既有事件再推新事件;缓冲 256,**慢订阅者丢事件**(日志才是权威来源) |
| `Turn` | 从 `turn/start` / `turn/end` 的载荷维护;`state` 缺省 `running`/`done` |
| `Fork` | 复制 `at` 为止的历史并**重新编号** seq;`Meta.Parent` 记录来源;父子互不影响 |
| `Compact` | 校验 `upTo` 范围后追加 `compact/summary` 事件,并记录已压缩边界 |

## 4. session-jsonl

一个会话一个文件,目录形如:

```
<dir>/<session-id>.jsonl
<dir>/<session-id>.meta.json
```

- `plugins/provider/session-jsonl` 的 `Store` / `Journal` 是持久化抽象:
  以后换 SQLite、bbolt 或内存后端时,只实现这两个接口,`Session` 与 `Provider` 的并发语义不变。
- `Store` 负责打开/创建/复制日志;`Journal` 负责单会话的 append、read、compact。
- **每会话 actor**:`Append` / `Compact` 进入该会话的命令队列,由同一个 goroutine 落盘;
  不同会话各有 actor 与文件,没有共享写锁,因此不会出现 SQLite 那样的全局写队列。
- 同一会话内仍保持单写者:seq 连续、append-only、不会交错;多个生产者可以并发调用
  `Append`,但由 actor 串行分配 seq。
- `Append` 默认 `sync=true`,每批落盘后 `fsync`;可配置 `sync=false` 换吞吐。
- 崩溃恢复:打开时逐行解析,最后一行不是完整 JSON 就截断到最后一条完整事件之后;
  完整但缺换行的尾行会补一个换行。
- `Fork` 保留 seq,把 `SessionID` 改为子会话 id,并写 `Meta.Parent`;
  `Compact` 只追加摘要,不重写历史。
- `OpenID` 是 provider 扩展(不属于 seam 最小接口),用于恢复已有会话:
  同一 id 已被打开时返回 `EINVALID`,避免两个写句柄。

配置:

```jsonc
{
  "id": "session-jsonl",
  "kind": "go",
  "inject": {"required": ["session"]},
  "config": {"dir": "examples/agent/sessions", "sync": true}
}
```

## 5. 与 seam-api §4 的偏差(有意,不是遗漏)

1. **`Project` 已实现,但检查点未持久化**:投影缓存只在内存;版本变化会重放,
   但跨进程恢复仍需 `seam/storage` 检查点。
2. **`Fork` 重新编号 seq**:子会话保持「seq 从 1 连续」,坐标来源由 `Meta.Parent` 表达。
3. **单写者由 actor 串行,不是拒绝**:session 契约的「单写者」是逻辑语义;
   `Append` 可以来自多个 goroutine,但会被 actor 串行化。provider 只拒绝同一 id 的第二个 `Open`。
4. **无多进程租约**:当前假设一个进程拥有会话文件;跨进程写需要租约(后续)。
7. **同进程多会话并发支持**:不同 session 各有独立事件日志/actor,同一 `Agent` 可并发 `Run` 不同 session(`agentloop.TestConcurrentSessionsInOneProcess`);**同一 session 的并发写仍由单写者契约约束**,调用方需自行串行(未加互斥拒绝)。
5. **`Compact` 不裁剪日志**:只追加摘要并记录边界,读路径仍能拿到全部历史。

## 6. 测试矩阵

| 场景 | 用例 |
|---|---|
| append 顺序 / seq 连续 / 分页 | `sessiontest.Run` → 「append 顺序」 |
| 空 type 拒绝(`EINVALID`) | 「空类型被拒绝」 |
| 未知插件事件可忽略 | 「未知事件可忽略」 |
| turn 状态 | 「turn 状态」 |
| 订阅回放 + 实时 + dispose 后不再推送 | 「订阅回放与推送」 |
| fork 隔离 | 「fork 隔离」 |
| compact 追加摘要 + 范围校验 | 「压缩追加摘要」 |
| jsonl 契约测试 | `jsonl.TestConformance` |
| 重新打开后事件/seq/meta 恢复 | `jsonl.TestPersistenceAcrossReopen` |
| 多会话并发写 | `jsonl.TestConcurrentSessions`(`-race`) |
| 同进程多会话并发 Run | `agentloop.TestConcurrentSessionsInOneProcess`(`-race`) |
| 同会话多生产者由 actor 串行 | `jsonl.TestSameSessionActorSerializes`(`-race`) |
| 半行崩溃恢复 | `jsonl.TestTornTailRecovery` |
| 路径穿越 id 拒绝 | `jsonl.TestRejectBadID` |
| 经内核装配 | `jsonl.TestPluginThroughKernel` |
| jsonl 投影 | `jsonl.TestProjectionWithJSONL` |
| 投影统计/增量重放 | `sessionstats.TestApplyStats`、`TestProjectionThroughKernel` |
| 未注册投影 | `sessionstats.TestProjectionUnregistered` |
| 压缩摘要 | `compaction.TestCompactAppendsSummary`、`TestCompactNoopWhenShort` |
| 模型摘要 | `compaction.TestCompactWithModelSummary` |
| memory provider 经内核装配 | `memory.TestSeamAndProviderThroughKernel` |
| 无 provider 时返回 `EUNIMPLEMENTED` | `memory.TestRegistryWithoutProvider` |

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/provider/session-jsonl ./plugins/provider/session-memory
go run ./cmd/ada plan --preset examples/agent/agent-jsonl.preset.json
go run ./cmd/ada agent --preset examples/agent/agent-jsonl.preset.json -message "读取 a.txt 并总结"
pwsh scripts/guard.ps1
```

## 8. 已知边界与下一步

1. 投影检查点持久化(`seam/storage`),冷启动只重放尾段。
2. `core/session`:把 session 包装成 agent core 插件(轮次生命周期、compaction 触发)。
3. `core/compaction-basic`:超预算时生成摘要并写 `compact/summary`。
4. 多进程租约与跨进程恢复。
5. 日志裁剪/归档策略(当前只追加摘要)。
6. TS 侧访问:bridge 提供 `ada:session`(身份、插件自有事件追加、`history`/`recent`
   有界读取)与 `ada:store`;没有无限量的事件导出接口(见 [bridge-session-store.md](bridge-session-store.md))。
7. 版本协商:消费者声明期望的 seam 版本,provider 版本不足时应在 `Registry.Open` 阶段拒绝
   (现在只比较 `Provider.Version()`,没有消费者侧期望)。
