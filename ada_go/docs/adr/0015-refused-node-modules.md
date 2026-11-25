# ADR 0015:不实现 worker_threads / inspector / async_hooks / trace_events

> 迁移说明(2026-11-25):执行层的 `compile`/`jsrt` 已抽到同级独立库 **ada_jsrt**;下文路径中的 `platform/jsrt` 现位于该库的 `jsrt` 包,决策与行为不变。

- 状态:已接受
- 日期:2026-11-24
- 复议日期:2027-06-30
- 相关:[node-compat.md](../node-compat.md)、[event-loop.md](../event-loop.md)、[m2-execution.md](../m2-execution.md)、AGENTS §0 红线 6 与 §0.1

## 1. 背景

补齐 Node 内置模块时,`worker_threads`、`inspector`、`async_hooks`、`trace_events`
这四个模块被点名要求。逐个核对执行层能力后,结论是:**它们不是「还没写」,而是
当前引擎(moejs)根本没有对应机制**;用 JS 近似实现会得到一个「看起来能用、
实际给出错误结论」的假能力。

## 2. 逐个判断

| 模块 | Node 的语义 | 当前引擎有什么 | 近似实现的后果 |
|---|---|---|---|
| `worker_threads` | 独立 isolate + 消息通道 + `SharedArrayBuffer`/transfer list + 可强制终止 | 一个 `Runtime` 一个 goroutine、一个 realm;没有第二个 isolate,也没有 `SharedArrayBuffer` | 用「同 realm 的假 Worker + JSON 消息」会让 `workerData`/`postMessage` 在**同一个堆**上跑:数据竞争不可见、`terminate()` 无法真正打断、`isMainThread` 恒 true 的代码路径分叉全部失效 |
| `inspector` | V8 Inspector 协议 + CDP 会话 + `inspector.Session` | moejs 没有暴露 Inspector 接口 | 只能返回空会话:`post()` 不报错但也不生效,调试器连不上却「看起来打开了」 |
| `async_hooks` | 引擎在 promise/timer/IO 上回调 init/before/after/destroy,promise 上下文传播是内核行为 | 引擎没有 async hook 钩子;我们的定时器与 Promise 都在宿主控制之外 | `AsyncLocalStorage` 会**静默丢失上下文**:跨 await 读到别的请求的值,比直接不存在危险得多 |
| `trace_events` | 写入 V8 的 trace buffer(供 `chrome://tracing` 分析) | 没有 trace buffer | 只能把事件丢进普通日志,却声称是 trace 文件,排查时得到错误证据 |

## 3. 决策

**不注册这四个模块**。`import "node:worker_threads"` 等仍走既有的链接期失败
(`No such built-in module: ...`),这是 Node 对缺失内置模块的原生行为,
而不是「import 成功、调用才炸」的桩。

替代路径(在文档里写清,而不是让作者自己猜):

| 需求 | 现在该用什么 |
|---|---|
| 并行/后台工作 | `platform/sched` 的任务与 `ctx.emit` 事件;重计算写 Go 插件(同进程、无跨边界) |
| 调试 | `ctx.log` + `ada inspect`(fiber/服务/effect/装饰器链快照);需要断点时用 `go test -race` 复现 |
| 请求上下文隔离 | 调用点 ctx 本身(`currentContext()`):每次调用有独立 ctx,天然不跨调用泄漏 |
| 追踪 | `policy/audit-log`(JSONL 审计)+ `node:perf_hooks` 的 mark/measure |

## 4. 替代方案(为什么没选)

| 方案 | 否决理由 |
|---|---|
| A. 假 Worker(同 realm + JSON 消息) | 与 Node 语义差得最远的地方正好是 `worker_threads` 的存在理由(真正的并行与隔离);假实现会让「搬过来就能跑」的承诺变成事故 |
| B. 每个 Worker 起一个新 moejs Runtime | 技术上可行,但需要一整套消息协议、生命周期、错误传播与配额;在事件循环刚落地、流/定时器还没稳定时做,等于同时改两层。列为复议项,不是永久拒绝 |
| C. `async_hooks` 用包装计时器/Promise 近似 | 无法覆盖 `await` 的上下文传播(引擎行为),近似只会制造「有时对有时错」的上下文,比没有更难查 |
| D. 四个模块都返回空实现 | 直接违反 AGENTS §0.1「能力先存在」与「区分能力与默认值」:空实现既不是能力,也遮住了缺失 |

## 5. 影响面

- `docs/node-compat.md` §5.1 列出这四个模块与原因;
- 插件的 `import` 在链接期失败,错误信息是 Node 原生文案;
- 不新增依赖、不改变二进制体积与执行模型;
- 复议时若要打开 `worker_threads`,先写传输协议与配额方案(见方案 B),再动代码。

## 6. 回滚方式

无代码可回滚(本 ADR 只记录「不实现」)。将来实现任意一个模块时,删除本 ADR 的
对应行、在 `platform/jsrt/nodemods` 注册实现并补契约测试即可。

## 7. 复议日期

2027-06-30。复议必须回答:

1. `platform/sched` + Go 插件是否已经覆盖了「并行工作」的真实需求?
2. 若确有 Worker 需求,消息协议用 JSON 还是结构化克隆,配额与终止语义如何定义?
3. 是否有上游(moejs)新增的 async hook / inspector 支持可以直接接?
