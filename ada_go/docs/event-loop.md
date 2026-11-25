# 执行层事件循环:调用边界驱动

> 代码:独立库 ada_jsrt 的 `jsrt/loop.go`、`jsrt/boot.js`、`jsrt/boot_timers.js`(模块 `github.com/issueye/ada_jsrt`)
> 决策:[ADR 0014](adr/0014-call-scoped-event-loop.md) · 上游能力:`moejs/eventloop`
> 相关契约:[m2-execution.md](m2-execution.md)、[node-compat.md](node-compat.md)

## 1. 要解决的问题

`ada_jsrt/jsrt` 的宿主 API 是同步的(`Call`/`CallWithHost`),而插件作者写的是现代 JS:
`async function apply`、`await fs.promises.readFile`、`await new Promise(r => setTimeout(r, 10))`。

如果宿主只做 `raw.Call(...)`,得到的是 Promise 对象,异步分支永远不会被驱动。
这个文档定义"一次调用什么时候算结束",以及异步在这个边界内的确切语义。

## 2. 模型

**一个 `jsrt.Runtime` = 一个 `eventloop.Loop`;默认由宿主调用驱动,不后台常驻。**

> 2026-11-26 修订:新增显式 `Options.Mode = LoopResident`。默认仍是 `LoopScoped`;
> Resident 只用于需要跨调用定时器、服务端或 `fs.watch` 的长期运行时，`Call` 串行，
> 权限/退出策略由宿主收紧。详见 [ADR 0014](adr/0014-call-scoped-event-loop.md) §7。

```text
宿主 goroutine                        worker goroutine(唯一执行 JS 的地方)
──────────────                        ──────────────────────────────────
rt.Call(ctx, "apply", cfg)
  └─ enqueue(job) ──────────────────▶ job()
                                       └─ loop.Run(fn)
                                            fn: __adaInvoke("apply", host, args, ok, fail)
                                            ├─ 定时器阶段 / 任务阶段 / 立即阶段
                                            ├─ 入口 Promise 结算 → ok(v) / fail(e)
                                            ├─ 清掉本次调用遗留的定时器
                                            └─ 无事可做 → Run 返回
  ◀────────────────────────────────── 结果(或错误)
```

要点:

1. 循环只在这条调用链里跑。**没有任何 JS 会在"没有调用"的时候执行**,
   `currentContext()` 因此永远有真实的调用 ctx(workspace 绑定、取消、配额都在)。
2. `loop.Run` 的返回条件是"没有东西再让它活着":没有 ref 的定时器 / 立即任务 /
   未结算的 `NewPromise` / 未跑的宿主任务 / 未跑完的 promise job。
3. 入口结算后,boot 模块会把本次调用里创建且仍未到期的定时器清掉(理由见 ADR 0014 §2),
   所以一个 `setTimeout(fn, 60_000)` 不会让 `apply` 卡住 60 秒。

## 3. 契约

### 3.1 可以被 await 的东西

| 形态 | 保证 |
|---|---|
| `await <同步值>` | 立即继续(一次微任务) |
| `await fs.promises.*`(moejs `node` 包) | 结算前 `Run` 不返回;错误按 Promise 拒绝传播 |
| `await new Promise(r => setTimeout(r, n))` | 定时器在调用内到期并执行 |
| `await` 一个永远不会结算的 Promise | `Run` 因无事可做而返回 → 宿主报「调用未结算」,不是挂死 |
| 顶层 `await`(模块求值) | **不支持**:`Runtime.Load` 返回 `ErrModulePending` 时装配失败并说明原因 |

### 3.2 定时器的作用域

| 行为 | 语义 |
|---|---|
| `setTimeout` / `setInterval` / `setImmediate` | 由 `eventloop` 安装,Node 形状(返回 Timeout/Immediate,可 `ref`/`unref`/`clear`) |
| 调用内到期并被 `await` | 执行,结果参与后续逻辑 |
| 入口结算时仍未到期 | 被清除,不跨调用;`Logf` 不报错(这是契约,不是异常) |
| 模块顶层(求值时)创建 | 与 `apply` 同一条规则:`Load` 结束时被清除 |
| 周期性工作 | 用 `plugins/sched` 的调度能力(Go 侧),不要靠 JS 定时器 |

### 3.3 错误与取消

| 情况 | 行为 |
|---|---|
| JS 同步抛错 | boot 捕获 → `onErr(堆栈)` → 宿主错误 `jsrt: JS 异常: <堆栈>` |
| Promise 拒绝 | 同上,拒绝值有 `stack` 时带堆栈 |
| 定时器回调抛错 | 交给 `eventloop.Options.OnError` → `Logf`;**不让当前调用失败**(一个后台刷新失败不该毁掉工具调用) |
| 调用 ctx 取消 | `Loop.Interrupt(ctx.Err())`;`Run` 返回 `*moejs.InterruptedError`,清中断后运行时继续可用 |
| 死循环 | 同取消路径,由调用 ctx 的 deadline 触发 |

### 3.4 与 `node:fs` 的关系

`node.InstallLoop(loop, opts)` 让 `node:fs` 的异步形状拿到真实的 `Hold`/`Post`:
promise 形状的读写在结算前不会让 `Run` 返回。是否安装 `node:fs` 仍只由
fs/workspace 装饰器决定([node-compat.md](node-compat.md) §3)。

## 4. 实现

### 4.1 `boot.js`(转接层)

```js
import "ada:boot";              // 先装定时器记账(在用户模块求值之前)
import * as entry from "ada:entry";
export * from "ada:entry";

export function __adaInvoke(name, host, argsJSON, onOK, onErr) { ... }
export function __adaInvokeFn(fn, thisArg, argsJSON, onOK, onErr) { ... }
```

- 调用插件函数、`await` 结果、结算时清定时器,再回调宿主;
- `onOK`/`onErr` 是宿主注入的 native 函数,把值/错误交回 worker goroutine;
- 参数仍走 JSON(跨边界不传引用,C3);只有 `ctx` 对象按 Value 传。

### 4.2 `boot_timers.js`(定时器记账)

包装 `setTimeout`/`setInterval`/`setImmediate`,把句柄记进
`globalThis.__adaPendingTimers`,并提供清除。为什么在 JS 侧做:宿主拿不到
`eventloop` 的定时器句柄(没有公开的清理接口),而"入口结算"这件事本身发生在 JS 里。

### 4.3 `loop.go`

| 函数 | 职责 |
|---|---|
| `newLoop` | `eventloop.New` + `OnError` 落到 `Logf` |
| `runCall` | 一次完整调用:`Run` → 等结算 → 映射错误 → 清理定时器 |
| `runOnLoop` | 不需要 await 的同步入口(waterfall 中间件) |
| `dropLoop` / `ensureLoop` | `Close` 时 `Terminate`;重建只发生在装配失败路径 |
| `interrupt` | 有循环走 `Loop.Interrupt`(能唤醒睡眠中的循环),否则退回 `Runtime.Interrupt` |

## 5. 已知边界

- 默认不支持后台/跨调用定时器;需要时显式使用 `LoopResident`(ADR 0014 §7);
- 顶层 `await` 支持(Scoped/Resident 均可用),但永久不结算的 Promise 仍报「调用未结算」;
- `waterfall` 中间件保持同步(`next` 是同步回落),不要在里面 await;
- `node:fs` 有 fs/workspace 装饰器时仍不可用(策略绕不过去,这是有意的);
- Resident 下 `fs.watch/watchFile` 是轮询实现,不是 fsnotify(ADR 0016 §8);
- `console`/`fetch`/`URL` 全局仍未安装(见 [node-compat.md](node-compat.md) §5.1)。

## 6. 测试

| 用例 | 覆盖 |
|---|---|
| `TestAwaitAsyncApply` | `async apply` 里的 `await` 在装配期完成 |
| `TestAwaitFSPromises` | `await fs.promises.writeFile/readFile` 真实落盘 |
| `TestSetTimeoutInsideCall` | `await new Promise(r => setTimeout(r, 1))` 在调用内执行 |
| `TestUnawaitedTimerIsCleared` | 入口结算时清定时器,不跨调用触发,调用不阻塞 |
| `TestAwaitNeverSettles` | 永不结算的 Promise 报「调用未结算」 |
| `TestAsyncRejectionSurfaces` | 异步拒绝带堆栈回到宿主 |
| `TestTimerErrorDoesNotFailCall` | 定时器回调抛错只记日志 |
| `TestLoopReuseAcrossCalls` | 同一运行时多次调用互不串味 |

```powershell
(cd ../ada_jsrt; go test ./jsrt -run 'Await|Timer|Loop' -count=1)
```
