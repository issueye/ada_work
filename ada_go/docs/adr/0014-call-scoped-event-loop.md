# ADR 0014:执行层的事件循环按「调用边界」驱动

> 迁移说明(2026-11-25):执行层的 `compile`/`jsrt` 已抽到同级独立库 **ada_jsrt**;下文路径中的 `platform/jsrt` 现位于该库的 `jsrt` 包,决策与行为不变。

- 状态:已接受
- 日期:2026-11-24
- 复议日期:2027-03-31
- 相关:[event-loop.md](../event-loop.md)、[m2-execution.md](../m2-execution.md)、[node-compat.md](../node-compat.md)、AGENTS §0 红线 1/7、§2.2、§5.3

## 1. 背景

`platform/jsrt` 目前每次宿主调用都是一个同步函数调用:`raw.Call(hook, args...)` 或
`raw.CallFunction(fn, this, args...)`,调用返回即认为这次调用结束。由此产生三类缺口:

1. **`await` 不成立**:插件写 `export async function apply()` 或 `await fs.promises.readFile(...)`,
   宿主拿到的是 Promise 对象,既不会等它结算,也无法把结算值翻译成 Go 值。
2. **定时器不可用**:moejs 的裸 Runtime 没有 `setTimeout`(C4);`node:fs` 的异步 API
   因为没有事件循环,只能靠 `platform/jsrt` 里自造的 `nodeLoop`(一个空实现的 Hold)勉强跑通,
   真实异步路径(回调/promise 形状)并没有被驱动。
3. **`node:fs` 的 promise 形状只是"形似"**:`docs/node-compat.md` §3 已经写明
   「promise 完成不保证早于 apply 返回」——这是缺陷,不是契约。

moejs 已经提供了完整的 `eventloop` 包(`Loop.Run/Start/Interrupt/Terminate`、定时器三件套、
`node.InstallLoop`),缺的是宿主选择一种驱动方式。

## 2. 决策

**每个 `jsrt.Runtime` 拥有一个 `eventloop.Loop`,由宿主调用边界驱动(`Loop.Run`),而不是后台常驻运行。**

具体:

1. `load()` 里创建循环(`eventloop.New`),用 `node.InstallLoop(loop, opts)` 替换自造的
   `nodeLoop`;`Runtime.Load` 本身也在 `loop.Run` 内执行。
2. 每次进入 JS(装配 `apply`、工具/事件回调、服务方法、waterfall 中间件)都在
   `loop.Run(fn)` 内执行:`fn` 只负责把调用挂上去,循环一直排空到「没有东西再让它活着」。
3. 入口函数的返回值由 boot 模块(`platform/jsrt/boot.js`)统一 `await`:
   - 同步值 → 立即用 `onOK` 交回宿主;
   - thenable → 结算后交回;拒绝 → `onErr(带堆栈的文本)`;
   - 永远不结算 → `Run` 因无事可做而返回,宿主收到「调用未结算」的明确错误。
4. 调用内创建、但入口已结算时仍未到期的定时器,由 boot 模块在结算瞬间全部清掉
   (见 §3 理由)。因此 `Run` 不会为了一个 60 秒的 `setTimeout` 卡住调用。
5. 取消仍走 `Loop.Interrupt`:调用 ctx 取消时宿主中断循环,`Run` 返回
   `*moejs.InterruptedError`,清掉中断标志后运行时继续可用(与今天的行为一致)。

### 为什么定时器在结算时被清掉

入口函数的 Promise 一旦结算,说明它 **所有 `await` 都已完成**。此时还挂着的定时器,
按定义就是"发起者没有等它"的:
- 让它继续留在循环里,它会在**下一次宿主调用**的排空阶段触发,那时 `callCtx` 已换成
  另一次调用的 ctx(workspace 绑定、取消信号、配额都不同)——这是**授权面的错位**,
  比"定时器没跑"严重得多;
- 清掉它,语义就变成一句可检验的契约:**要异步,就 await 它;要周期任务,用 `sched`**。

## 3. 替代方案(为什么没选)

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 后台常驻循环 | `Loop.Start()` 让循环一直跑,定时器在调用之间继续触发 | 后台回调没有"当前调用",`currentContext()` 只能退到 `context.Background()`:workspace 绑定、caps 授权、审批、审计都失去调用点。等于在本项目最核心的红线(§0-3/§0-7)上开洞。要修就得给后台回调定义一整套"无调用授权模型",成本远高于收益。 |
| B. 不接事件循环,只补 `await` | 用 `PromiseResult()` 在返回后取值,不引入定时器 | 异步 fs 仍然没有驱动(`nodeLoop.Hold` 是空实现),`await` 一个真实 I/O 会立刻"未结算"失败;等于把缺陷从"不 await"改成"await 必失败"。 |
| C. 每次调用新建 Runtime | 用 `Loop.Run` 一次后 `Terminate` 并重建运行时 | 每调用一次就丢掉模块级状态与 `ctx.store`,`apply` 的副作用(provide/tool/effect)全部失效;`ctx.provide` 的服务会指向已死运行时。 |
| D. 引入 Node/libuv | 用真实 Node 进程 | 直接违反"单二进制、无 Node"(硬约束 C8)与 ADR 0004。 |

## 4. 影响面

| 方面 | 变化 |
|---|---|
| `platform/jsrt` | 新增 `loop.go`(循环接入)与两个 boot 模块;`Runtime` 增加 `loop`/`invoke` 字段;删除 `nodeLoop`;`Options` 无破坏性变化 |
| 调用契约 | 宿主 API 仍是同步的 `Call`/`CallWithHost`;变化只在"这次调用什么时候算结束"——现在等入口结算 |
| 插件作者 | 可以写 `async function apply`、`await fs.promises.*`、`await new Promise(r => setTimeout(r, n))`;不能靠"留下定时器"做后台工作(会被清掉且 `Logf` 记一条警告之外的错误) |
| `node:fs` | 异步/回调形状真正可用;`node:fs` 是否安装仍由 fs/workspace 装饰器决定(不变) |
| 预算 | `platform/jsrt` 无行数预算(AGENTS §5.3 只约束 kernel/host/plugin/seam);新增代码约 200 行 |
| 性能 | 每次调用多一次 `Loop.Run`(enter/exit + 队列检查);基准仍须满足 ≤20 µs/次(`scripts/bench.ps1`) |
| 兼容 | 顶层 `await` 不支持(模块求值返回 `ErrModulePending` 时给出明确错误),不是静默失败 |

## 5. 回滚方式

`loop.Run` 的接入集中在 `platform/jsrt/loop.go` 的三个入口函数(`runCall`、`runOnLoop`、
`newLoop`)。回滚只需把这三处改回直接调用 `raw.Call`/`raw.CallFunction`,并恢复
`nodeLoop`;boot 模块与 `eventloop` 依赖可一并删除,**不触及 kernel、seam、bridge 的契约**。

## 6. 复议日期

2027-03-31。复议时要回答的问题:

1. 是否有真实需求要求"调用之间继续跑"的定时器?若有,应先在 `platform/sched` 里加
   `onCron`/`onInterval` 形态的插件钩子,而不是打开后台循环;
2. `node:fs` 的 `FSOptions.Allow`(每次访问判定)是否可以接到 fs seam 的装饰器链上,
   从而消掉"有策略时不能用 node:fs"的限制;
3. 每次调用的 `Loop.Run` 开销是否仍在预算内(`scripts/bench.ps1`)。
