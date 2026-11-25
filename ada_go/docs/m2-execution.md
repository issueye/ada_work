# M2 执行层(第一刀 + 双向 ctx 桥)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §10–§11 与 [ADR 0004](adr/0004-execution-layer-deps.md)。
> 本页描述**已实现**的部分与**已知边界**;未实现的部分明确列在 §5,不假装存在。
> 迁移说明:执行层已抽到同级独立库 **ada_jsrt**;下文路径 `ada_jsrt/*` 指该模块的包,仓库内只保留 `platform/bridge`。

## 1. 范围

M2 的目标是「TS 插件在进程内跑起来,且不需要 Node」。当前已打通**双向、含中间件**:

```
TS 源文件 ──compile.Build──▶ Artifact(每个文件的 JS,纯数据)
                                   │
                                   ▼
                    jsrt.New ──▶ moejs Runtime(每实例一个 goroutine)
                                   │
        ┌──────────────────────────┴──────────────────────────┐
        │ JS → 内核(暂存,回到内核协程提交)                  │ 内核 → JS(投递到 worker)
        ▼                                                      ▼
 apply(ctx, cfg)                                      ctx.on(event, handler)
   ctx.provide / ctx.emit ──▶ stager ──▶ 服务表/事件总线   内核事件 ──▶ CallFunction
   ctx.effect ──▶ 卸载时反向执行清理                      服务方法 ──▶ CallFunction
   ctx.waterfall ──▶ 可改写/否决的中间件                   waterfall ──▶ 同步 next 往返
```

**不在这一刀里**:装配(assemble)、插件清单与安装、权限/能力、运行期提交队列、`async apply`、池化、sourcemap 反查、预算/熔断。

## 2. 三个包

### 2.1 `compile`

| API | 语义 |
|---|---|
| `Transform(name, src, opts)` | 单文件降级到 ES2022 ESM;不解析路径、不打包、不落盘 |
| `Build(id, entry, files, opts)` | 逐文件转换并计算摘要,产出 `Artifact` |
| `Artifact.Resolve(referrer, spec)` | **只支持相对导入**;允许省略扩展名、`.js` → `.ts`、`/index` |
| `Artifact.Verify()` | 结构与摘要校验;从磁盘/网络拿到的产物必须先过这一关 |
| `ParseArtifact`/`JSON` | 落盘与跨进程传输用的纯数据格式 |

错误统一为 `*compile.Error`(带 `KindSyntax`/`KindInternal` 与逐条行列诊断)。
`Options.TsconfigRaw` 可透传 `experimentalDecorators` 等开关。

### 2.2 `jsrt`

| API | 语义 |
|---|---|
| `New(ctx, Options)` | 编译 Artifact → `moejs.Link` → `Load`;每实例一个 worker goroutine |
| `Call(ctx, export, args...)` | 参数/返回值只走 JSON |
| `CallWithHost(ctx, export, args...)` | 在参数最前插入 `ctx` 对象,供 `apply(ctx, cfg)` 使用 |
| `Options.Host` | `provide` / `emit` / `on` / `effect` / `waterfall` 的落地回调 |
| `Service` | JS 侧 `ctx.provide` 的产物;`Call(ctx, method, args...)` 转发到 worker |
| `Close(ctx)` | 停止 worker;可重复调用 |
| `ErrClosed` / `ErrBroken` / `ErrReentrant` | 关闭后调用 / 中断后无法收尾 / 同运行时嵌套调用 |

并发模型:Runtime 只在 worker goroutine 上使用,只有 `Interrupt`/`ClearInterrupt` 跨协程——这是 moejs 的硬约束(C1),包内用 `mu` 保护 `raw` 指针。`ctx.on`、`ctx.effect`、`ctx.waterfall` 注册的函数与 JS 服务方法共用 worker 独占的 `handlers` 表(函数 + `this`),调用一律经引导模块在 worker 上执行。

**调用 ctx 传播**:`enqueue` 在 worker 执行每个任务前把任务 ctx 存入 `callCtx`,结束后清空;`__adaHost.shell` 与 `__adaHost.fs.*` 使用 `currentContext()`,因此 JS 工具的原生 fs/shell 调用会继承调用级 `workspace.Binding`、取消与超时,而不是退回 `context.Background()`。事件/waterfall 回调用事件自己的 ctx。

### 2.2.1 事件循环

每次进入 JS(`Load`、`Call`、回调、服务方法)都在 `eventloop.Loop.Run` 里执行,循环排空到
「没有东西再让它活着」为止;入口的 Promise 由引导模块 `boot.js` await 后交回宿主。
循环由**调用边界驱动**,不后台常驻——理由与替代方案见
[event-loop.md](event-loop.md) 与 [ADR 0014](adr/0014-call-scoped-event-loop.md)。

- 可用:`await`、`fs.promises`、`setTimeout`/`setInterval`/`setImmediate`(调用内到期);
- 不可用:跨调用的后台定时器、顶层 `await`、`waterfall` 中间件里 await(`next` 是同步的);
- 未 await 且入口结算时仍未到期的定时器会被清掉,不会活到下一次调用;
- 取消走 `Loop.Interrupt`,与 `Interrupt`/`ClearInterrupt` 的既有语义一致。


### 2.2.1 Node 内置模块

`jsrt` 同时安装一套 Node 形状的内置模块,降低脚本插件的心智负担:

- `node:fs` / `node:fs/promises` / `node:buffer` 来自 moejs 的 `node` 包;
- `node:path` / `node:os` / `node:events` / `node:util` / `node:assert` /
  `node:url` / `node:querystring` / `node:string_decoder` / `node:process`
  是 `ada_jsrt/jsrt/nodemods` 的宿主源码(embed);
- `node:child_process` / `node:http` / `node:https` / `node:dns` /
  `node:dns/promises` / `node:net` 也是宿主源码,经 `__adaHost` 调用宿主能力;
  网络模块直连标准库,仓库已无 `seam/net` 与 `net-guard`(ADR 0011/0012);
- `node:timers` / `timers/promises` / `stream` / `stream/promises` / `readline` /
  `readline/promises` / `tty` / `perf_hooks` 是宿主源码(纯 JS,建立在事件循环上的
  模块化形态);`tty` 只回答「不是终端」,`worker_threads`/`inspector`/`async_hooks`/
  `trace_events` 不实现(ADR 0015);
- `node:fs` 只在 fs/workspace 链上没有策略装饰器时安装,避免绕过
  fs-sandbox/caps/workspace-guard;
- `node:fs` 与 `node:fs/promises` 由宿主包一层:**补齐** `mkdtemp`/`symlink`/`link`/
  `chmod`/`chown`/`truncate`/`utimes` 的 sync / callback / promise 三形态
  (`ada_jsrt/jsrt/nodefsx.go` + `fsx.js`),路径与写权限沿用 node:fs 的根与开关;
- 旧的 `ada:fs` / `ada:shell` 仍保留,见 [node-compat.md](node-compat.md)。

### 2.3 `bridge`

`bridge.Plugin` 实现 `kernel.Plugin`,入口约定与 AGENTS §6.2 一致:**`apply(ctx, config)`**。

JS 侧 `ctx` 目前有六个能力:

| 能力 | 方向 | 行为 |
|---|---|---|
| `ctx.log(...)` | JS → 宿主 | 立即写宿主日志(线程安全) |
| `ctx.provide(name, value)` | JS → 内核 | 暂存;`apply` 返回后提交为 `kernel.Provide`。对象上的函数成员成为**服务方法** |
| `ctx.emit(event, payload)` | JS → 内核 | apply 期间暂存;运行期直接派发(广播) |
| `ctx.on(event, handler)` | 内核 → JS | 暂存注册;提交为内核监听器,事件触发时把载荷投递到 worker 执行 JS 函数 |
| `ctx.effect(label, cleanup)` | JS → 宿主 | 暂存;fiber 卸载时按逆序执行 JS 清理函数 |
| `ctx.waterfall(event, handler)` | 双向 | 暂存注册;提交为内核 waterfall 中间件,JS 可用 `next` 同步继续下游 |

**写方向为什么暂存**:内核的生命周期操作是单写者(M1 §1),而 JS 跑在自己的 goroutine 上。直接在 JS 线程里 `Provide`/`Emit` 会同时触碰服务表、事件总线与 fiber 表;加锁只能消除数据竞争,消除不了「半新半旧的依赖图」这类语义竞争。

**提交语义**:
- 顺序 = JS 调用顺序:先挂 `waterfall`/`on`,再 `provide`,最后 `emit`——否则 apply 里 emit 的事件打不到自己刚注册的处理器;
- 提交发生在 `apply` 返回之后、仍在调用 `Apply` 的内核协程上;
- `apply` 结束后再调用 `provide`/`on`/`effect`/`waterfall` 会抛 JS 异常(没有提交点的调用必须报错,不能静默丢失)。

**运行期写(只有广播)**:`ctx.emit` 在 apply 结束后仍然可用——事件是广播,总线自带锁、每个监听器有独立超时,不需要内核控制协程。其余四个能力会改服务表/监听器表/依赖图,仍只允许在 apply 期间提交。
反过来,若运行期 `emit` 触发的监听器又调回**同一个运行时**,`inHost` 守卫会返回 `ErrReentrant`,避免「worker 等待宿主、宿主又等待 worker」的死锁。

**清理语义**:
- `ctx.effect` 的清理函数**由 bridge 的 Apply disposer 执行**,在关闭运行时之前、按登记逆序执行——与内核 effect 栈同语义;
- 为什么不直接挂到内核 effect 栈:内核在 Apply 返回后才把 `apply` disposer 入栈,而它是最先被执行的(LIFO);若清理函数在内核栈里,运行时已经先被关掉了;
- 清理失败会聚合进 `Fiber.Dispose` 的返回值,不会被吞掉;
- 装配失败时,已登记的清理函数也会被兜底执行一遍。

**waterfall 语义**:
- JS 处理器签名 `(input, next) => any`;调用 `next(v)` 会**同步**回到监听器所在的 goroutine 继续下游,拿到下游结果后返回给 JS;
- 不调用 `next` 即短路(否决),其返回值成为整条链的结果——与内核 waterfall 一致;
- **类型还原**:输入是「指向结构体的指针」(如 `*llm.Request`、`*prompts.Assembly`、
  `*tools.CallInfo`)时,`next` 参数与 JS 返回值会按 JSON 解码回同一类型;
  否则下游的 `v.(*T)` 断言失败,JS 改写会被静默丢弃。其它形态(标量/map/切片)原样透传;
- 重复调用 `next` 抛错,避免重复执行下游副作用;
- **同一运行时不允许嵌套调用**(`ErrReentrant`):worker 正阻塞等待宿主响应 `next` 时再次投递任务必然死锁;跨运行时的链是安全的;
- 取消/超时沿用 `Interrupt`;若 worker 正阻塞在 `next` 上,宿主会送一条错误结果把它放出来,避免 goroutine 泄漏。

**读方向**:
- 事件监听器在内核协程上被调用,把载荷 JSON 投递到 worker 并同步等待;返回值按 `emit` 语义忽略,抛错记为监听器失败;
- 服务方法由 Go 消费者显式调用:`svc.Call(ctx, "render", args...)`;
- `this` 绑定到服务对象本身,所以方法里读 `this.text` 能拿到兄弟成员。

**Go 侧拿到的服务值**:

```go
svc, err := kernel.Service[*jsrt.Service](ctx, "ts.greeting")
text := svc.Data()["text"]                 // 非函数成员(JSON)
out, err := svc.Call(ctx, "render")        // 方法调用转发到 JS worker
```

非对象值(标量/数组)放在 `svc.Value()` 里;`svc.Methods()` 给出方法名(排序)。

## 3. 约束落点

| 约束 | 落点 |
|---|---|
| C1 单 goroutine | `jsrt.worker` 是唯一接触 `moejs.Runtime` 的 goroutine |
| C2 字节码不可序列化 | Artifact 只存 JS 源文本;不缓存字节码 |
| C3 跨边界只传 JSON | 调用参数、事件载荷、方法参数、waterfall 的输入与 `next` 返回值都经 JSON 编解码;结构体指针输入在返回时按 JSON 还原类型 |
| C4 裸 Runtime 无环境能力 | 只有 `ctx.log/provide/emit/on/effect/waterfall`;默认零权限 |
| P5 注册有主 | JS 的 provide/on/effect/waterfall 都经暂存提交,卸载时按序回收 |
| P7 门禁确定 | waterfall 的 `next` 往返是同步的,超时/中断会确定性阻断;不允许重复 `next` |
| P8 数据面留在 Go | JS 只处理控制面/小对象;大对象留后续句柄化 |
| 单写者(写方向) | JS 侧写请求一律经 stager,回到内核协程提交 |
| 运行期写 | 只允许广播(`emit`);改服务表/监听器表/依赖图的操作仍限 apply 期间 |
| 自回调防死锁 | `inHost` 守卫:worker 正在回调宿主时,宿主不得再要求同一运行时执行回调 |
| 反向调用(读方向) | 内核 → worker → `CallFunction`;只读不写内核 |
| 内核无知 | `platform/kernel/` 不 import `compile`/`jsrt`/`bridge`(guard 的 `go list -deps` 强制) |

## 4. 测试矩阵(M2 DoD 的 12 类 fixture)

| fixture | 状态 | 用例 |
|---|---|---|
| 正常调用 | ✅ | `jsrt.TestCallMultiModule`、`bridge.TestJSPluginRunsInKernel` |
| JS 抛错 | ✅ | `jsrt.TestCallReportsJSException` |
| 超时/死循环 | ✅ | `jsrt.TestTimeoutInterruptsAndRuntimeRecovers`(中断后运行时仍可用) |
| 多文件相对导入 | ✅ | `jsrt.TestCallMultiModule`、`compile.TestBuildResolveAndDigest` |
| 循环依赖 | ✅ | `jsrt.TestCircularImports` |
| TS enum / 参数属性 | ✅ | `compile.TestTransformLowersTSRuntimeSemantics` |
| 装饰器(现代 + 实验) | ✅ | `compile.TestTransformDecorator` |
| TSX | ✅ | `compile.TestTransformTSX` |
| 缺模块 | ✅ | `jsrt.TestErrorsAreExplicit` |
| 缺导出 | ✅ | `jsrt.TestErrorsAreExplicit` |
| JS 提供服务并由 Go 消费 | ✅ | `bridge.TestJSPluginRunsInKernel` |
| JS 派发事件并由 Go 监听 | ✅ | `bridge.TestJSPluginRunsInKernel` |
| 内核事件 → JS 回调 | ✅ | `jsrt.TestHostOnInvokesJSHandler`、`bridge.TestJSPluginRunsInKernel` |
| Go 调用 JS 服务方法 | ✅ | `jsrt.TestServiceMethods`、`bridge.TestJSPluginRunsInKernel` |
| JS effect 逆序清理 + 失败上报 | ✅ | `jsrt.TestHostEffect`、`bridge.TestJSEffectErrorSurfacesOnDispose` |
| 运行期 emit(处理器里再 emit) | ✅ | `bridge.TestJSPluginRunsInKernel`、`jsrt.TestRuntimeEmitReentrantGuard` |
| JS waterfall 改写/否决 + 重复 next + 嵌套防护 | ✅ | `jsrt.TestInvokeWaterfall`、`jsrt.TestWaterfallDoubleNextFails`、`jsrt.TestWaterfallReentrantFails`、`bridge.TestJSPluginRunsInKernel` |
| JS waterfall 改写结构体指针输入后类型还原 | ✅ | `jsrt.TestWaterfallCoercesStructPointer`、`bridge.TestJSMemoryAutoRecall` |
| ctx 能力未启用/参数非法时报错 | ✅ | `jsrt.TestHostCallbackErrorsSurfaceInJS`、`jsrt.TestHostOnRejectsNonFunction` |
| JS 工具的原生 fs 继承调用 ctx(含 workspace 绑定) | ✅ | `bridge.TestJSNativeFSInheritsCallBinding` |
| CJS(`require`/`module.exports`) | ❌ | design §10 规定走离线 Bundle,M2 未做 |
| OOM / 内存上限 | ❌ | `jsrt.Options.MemoryLimit` 已透传,未写拒绝用例 |
| 卡死(不响应中断) | ❌ | `ErrBroken` 路径已实现,未写用例(需要不可中断的宿主函数) |

## 5. 已知边界与下一步

1. **运行期只有广播**:`ctx.emit` 可用,但 `provide`/`on`/`effect`/`waterfall` 在回调里仍会被拒绝——它们会改依赖图或注册表,必须回到内核控制协程。要支持,需要一个真正的内核命令队列(内核目前没有事件循环)。
2. **`apply` 必须同步返回**:`async apply` 的 Promise 尚未 await;落地时用 `moejs.PromiseResult`。
3. **同运行时不能嵌套 waterfall**:同一插件在同一事件上注册多个 waterfall 中间件会触发 `ErrReentrant`;需要「同一运行时串行排队」或把多个处理器合并成一次 JS 调用才能支持。
4. **清理只到 bridge 一层**:JS effect 由 bridge 的 Apply disposer 执行,不进入内核 effect 栈,因此 `ada inspect` 看不到它们。
5. **无装配层**:Artifact 目前由代码直接构造;插件清单、安装、签名、capability 属于 M3/M5。
6. **sourcemap 未接**:`Transform` 支持产出 map,但错误堆栈还没有反查(design §10.6 / R3)。
7. **池化与预算**:`pool: N`、内存/时间预算、熔断未做。
8. **`__adaLog` 兼容别名**:上一版的全局函数仍保留,等 `ctx` 稳定后删除。

## 6. 验证命令

```powershell
go test -race -count=1 ./...          # 内核 + schema + compile + jsrt + bridge
go run ./cmd/ada-demo                 # M1:纯 Go 插件装配
go run ./cmd/ada-ts-demo              # M2:TS 插件编译→加载→双向 ctx(服务/事件/回调/方法/清理/waterfall)
pwsh scripts/guard.ps1                # 红线门禁(含 kernel 不依赖执行层的检查)
```
