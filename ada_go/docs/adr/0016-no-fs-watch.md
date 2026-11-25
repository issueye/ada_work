# ADR 0016:不实现 `fs.watch` / `fs.watchFile` / `fs.unwatchFile`

> 迁移说明(2026-11-25):执行层的 `compile`/`jsrt` 已抽到同级独立库 **ada_jsrt**;下文路径中的 `platform/jsrt` 现位于该库的 `jsrt` 包,决策与行为不变。

- 状态:已接受
- 日期:2026-11-24
- 复议日期:2027-06-30
- 相关:[node-compat.md](../node-compat.md)、[event-loop.md](../event-loop.md)、[ADR 0014](0014-call-scoped-event-loop.md)、AGENTS §0 红线 6 与 §0.1

## 1. 背景

补 `node:fs` 时,`watch` 家族是唯一没法按 Node 语义实现的 API。它要同时满足两个条件:

1. **在 JS 侧收到事件**:监听器是插件里的 JS 函数;
2. **在没有调用时也能收到**:文件被外部修改的时刻,通常没有任何宿主调用在跑。

条件 2 与 [ADR 0014](0014-call-scoped-event-loop.md) 直接冲突:那个决定把事件循环限定在
**一次宿主调用**内,调用结束就 `Run` 返回,不做后台执行。原因也写在那里——后台回调没有
「当前调用」的 ctx,workspace 绑定、caps 授权、审批与审计都会失去调用点。

此外,宿主没有跨平台的文件通知后端:Linux 用 inotify、macOS 用 FSEvents、Windows 用
`ReadDirectoryChangesW`。Go 标准库不提供统一抽象,引入 `fsnotify` 属于
AGENTS §12 的「新的第三方依赖」,需要单独 ADR。

## 2. 决策

**不实现**:`fs.watch`、`fs.watchFile`、`fs.unwatchFile` 调用即抛
`ERR_NOT_SUPPORTED`,错误信息指向本 ADR 与替代路径。不做「静默无效的 watcher」,
也不做「只在下次调用时批量投递事件」的降级版本——后者会让「盯着文件变化等它触发」
的代码永远等不到,排查成本远高于一句明确的拒绝。

## 3. 替代路径(写进文档,而不是让作者自己猜)

| 需求 | 现在该用什么 |
|---|---|
| 监视文件变化并响应 | **Go 插件**:用 `fsnotify` 或轮询 + `ctx.emit("fs/changed", ...)` 发事件,JS 用 `ctx.on` 订阅 |
| 周期性检查(如每 5 秒看一次 mtime) | `platform/sched` 的周期任务 + Go 插件;不要在 JS 里靠定时器轮询 |
| 单次调用内的等待 | `await` 一个 Go 服务方法(例如"等待文件出现"),调用边界内可以阻塞 |

## 4. 替代方案(为什么没选)

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 引入 `fsnotify` + 后台投递 | 新依赖,事件在宿主 goroutine 里收,再想办法送进 JS | 需要打开后台事件循环(ADR 0014 已否决),或者接受「事件只在下次调用时投递」的降级语义;同时新增依赖未过 ADR |
| B. `fs.watch` 返回空 watcher,事件永不触发 | 形状对齐,行为静默 | 违反「按 Node 的方式失败」的既有原则;空 watcher 会让等待方永久挂起 |
| C. 轮询 + 只在调用内检查 | 每次调用开始时 stat 一次并对比 | 语义与 `watch` 完全不同(漏事件、错过瞬时变化),却共享同一个 API 名字,比不提供更误导 |

## 5. 影响面

- `node:fs` 的 `watch`/`watchFile`/`unwatchFile` 从 moejs 的「未实现桩」变成
  明确的 `ERR_NOT_SUPPORTED`(错误文案给出替代路径);
- 不引入依赖,不改变执行模型与二进制体积;
- 复议时若要做,先落「Go 插件 + 事件」的形态(它不需要后台 JS),再讨论是否值得为
  纯 JS 插件打开后台循环。

## 6. 回滚方式

无代码需要回滚(本 ADR 记录的是「不实现」)。将来若实现,删除本 ADR 的对应条目、
在 `platform/jsrt/nodemods/src/fsx.js` 用真实 watcher 替换 `watchUnsupported`,并补
契约测试。

## 7. 复议日期

2027-06-30。复议必须回答:

1. 「Go 插件 + `fs/changed` 事件」是否已经覆盖了真实需求?
2. 是否有产品需求必须让 JS 插件在没有调用时收到文件事件?若有,是否接受为此重开
   ADR 0014 的后台循环?
3. `fsnotify` 的跨平台行为与许可证是否可接受(需要单独 ADR)?
