# Node 风格运行时设计（fs / net / IO / 异步 / 权限）

> 状态：**设计稿，部分实现**
> 范围：`ada_jsrt`（`compile` + `jsrt`）
> 目标：把当前「插件调用型 JS 执行层」升级为「类 Node.js 的脚本运行时」，并补齐文件、网络、IO、异步、事件循环与权限配置。

## 实现进度

| 阶段 | 状态 | 已落地 / 待办 |
| --- | --- | --- |
| **P0 权限骨架** | ✅ 完成 | `Permissions/FSPermission/NetPermission/ChildPermission/EnvPermission`、`Limits`、`LoopMode`、`ExitPolicy`、`Event`；路径/网络匹配器；`Permission()` 视图；见 `jsrt/permissions.go` |
| **P1 常驻循环** | ✅ 核心完成 | `LoopResident`、`Wait`、顶层 await、`AbortController`、`unhandledRejection`/`uncaughtException` 的 `OnEvent` 与 `process.on`、`ExitPolicy`/`process.exit`；`nextTick` 顺序仍为近似 |
| **P2 文件权限** | ✅ 核心完成 | 核心 `node:fs` 的 `Allow`、fsx 逐操作检查、只读/Allow/Deny/WriteAllow、符号链接解析、`fs.watch/watchFile` 轮询、`MaxFileBytes/MaxOpenFiles/MaxDirEntries`（fsx + 核心 fs 包装层）；核心 fs 瞬态 fd 仍由 Workers 限制 |
| **P3 网络客户端** | ✅ 核心完成 | Net Guard；DNS 异步；异步 `net.connect`（支持 AbortSignal）+ `readAsync` 泵；`tls.connect`；`MaxConnections`/请求响应字节配额；HTTP Resident 流式 / Scoped 整包 |
| **P4 网络服务端** | ◐ 部分完成 | `net.createServer/listen/accept`（Resident）；HTTP/1.1/HTTPS 服务端支持 keep-alive、chunked 请求/响应、header/body 超时与 `maxBodyBytes`；流式与完整慢连接防护待做 |
| **P5 IO/流** | ✅ 核心完成 | `_read/_write/_flush/_destroy`、字节/对象高水位、drain 背压、pipe/pipeline/finished、`Readable.from`、zlib `create*` 逐块流、`toWeb/fromWeb`、HTTP Resident 流式；完整 WHATWG 控制器仍是 C1 说明 |
| **P6 收口** | ✅ 核心完成 | 兼容矩阵、Node 风格端到端用例、Call/Stream 基准、`Artifact.SourceMap` 堆栈重映射、fuzz seeds；逐块 zlib/更多 fuzz 属后续增强 |

> 默认仍是 `LoopScoped`（一次调用一次循环）；`Mode: LoopResident` 已可用。FS、Net、Child、Env 权限均已强制；`process` 的 uncaughtException/unhandledRejection 事件已接入。

---

## 0. 结论

**可行，但不建议追求完整 Node.js 兼容。** 正确的目标是：

1. **引擎层复用现有的 moejs + eventloop**，不换引擎；
2. 增加 **常驻事件循环模式**，同时保留现有「一次调用一次循环」的插件模式；
3. 以 **Node 20/22 的常用接口形状 + 语义** 为目标，明确 C0–C3 兼容级别与非目标；
4. 把 **文件、网络、子进程、环境变量** 的权限做成 Go 侧 `Options` 的一等配置，默认拒绝，JS 侧只做形状；
5. 保留 `Host` 注入点作为嵌入式扩展，内置默认的 OS provider，权限检查先于任何 provider 执行。

建议分 6 个阶段落地，单人约 **12–15 周**，双人约 **7–9 周**；每个阶段都能独立合并、独立验收。

---

## 1. 现状盘点

### 1.1 引擎与事件循环

| 项 | 现状 |
| --- | --- |
| JS 引擎 | `moejs v0.1.0-alpha.11` |
| 编译 | `esbuild v0.28.2`，逐文件 TS→ES2022 |
| 循环 | `eventloop.New`；**一次宿主调用跑一次 `loop.Run`** |
| 并发 | 一个 Runtime 一个 worker goroutine；所有 JS 访问串行 |
| 定时器 | 调用结束时 `boot.js` 清理未到期定时器（`boot_timers.js`） |
| 取消 | `ctx` 取消 → `loop.Interrupt`；2 秒未收尾 → `ErrBroken` |
| 后台能力 | 无：定时器、异步 I/O 都必须在本次调用内结算 |
| 顶层 await | **不支持**：`Load` 返回 `ErrModulePending` 时直接报错 |
| 动态 `import()` | 未接入；模块图在装配期静态 `Link` |
| 模块形态 | ESM only；无 CommonJS / `require` / npm 解析 |

### 1.2 模块覆盖（当前）

| 模块 | 当前能力 | 主要缺口 |
| --- | --- | --- |
| `node:fs` / `fs/promises` | moejs 核心 19 个操作 + `fsx.js` 补齐（fd 族、`FileHandle`、`Dir`、流、`readv/writev`、`readLines` 等） | `watch` 明确不支持；`lchmod/lchown/lutimes/fdatasync/statfs/openAsBlob` 未实现；流背压不完整；无路径/网络级权限 |
| `node:net` | **异步 connect + `data` 泵 + `createServer/listen`**；`Socket.read(n)` 同步扩展 | 无 TLS/unix socket；背压/暂停语义简化；服务端需 `LoopResident` |
| `node:http` / `https` | **同步整包客户端 + 最小 HTTP/1.1 服务端**；`Agent` 只是占位 | 服务端无 keep-alive/chunked；客户端无流式请求/响应；无 `tls` 服务端 |
| `node:dns` | 同步 `lookup/resolve*`；宿主参考实现支持 `lookup/lookupAll/resolve4/6/Cname/Ns/Mx/Txt/Srv/reverse` | 无异步形态；无自定义 resolver；无权限 |
| `node:stream` | `Readable/Writable/Duplex/Transform/PassThrough`、`pipe`、`asyncIterator`、基础 `drain` | 完整 `_read`/`_write` 协商、`destroy`、`pipeline/finished` 边界、对象模式细节；fsx 流不跨块回压 |
| `node:timers` | eventloop 定时器 + `ref/unref`；`timers/promises` 部分 Promise 形态 | 受调用边界清理；`AbortController` 无全局 |
| `node:fs` 流 | `createReadStream/createWriteStream`（fsx） | 大文件可能整段进内存；无跨块暂停 |
| `node:zlib` | 同步 gzip/deflate/raw/unzip 7 个函数 | 无异步/流式；无 brotli/zip |
| `node:crypto` | hash/hmac/pbkdf2/random/timingSafeEqual/uuid | 无对称加密、非对称、X509、TLS |
| `node:child_process` | 仅 `execFile/execFileSync` | 无 `spawn/fork/exec`；无 stdio 流 |
| `node:process` | 受限子集：`cwd=/workspace`、`env={}`、`argv=[]`、`pid=0`、`exit` 抛错、`nextTick` 用 Promise 模拟 | 无真实 env/argv/exit/SIG*；`nextTick` 顺序不严格 |
| `node:events` | `EventEmitter` + `once/on/listenerCount` 等 | `captureRejections`、`error` 语义边界 |
| 其它 | `buffer/url/util/assert/os/path/querystring/string_decoder/readline/tty/perf_hooks` | 子集，需逐项对表 |
| 明确不做 | `worker_threads`、`inspector`、`async_hooks`、`trace_events`、`cluster`、`vm`、WASI | 无隔离/无线程，保持拒绝 |

### 1.3 权限现状

当前没有显式的权限模型，只有隐式能力开关：

- `Options.Host` 里某个函数为 `nil` → 对应能力不可用；
- `Options.Node.FS` 给 `Root` 才有 `node:fs`，`Write` 控制写；
- `node:fs` 的路径被 `os.Root` / `/workspace` 限制；
- 网络没有任何 allow/deny；是否可用完全由宿主 `Host.NetDial/HTTPDo/DNSLookup` 决定；
- 没有审计、没有按路径/主机的白名单、没有资源配额（除 `MemoryLimit`）；
- moejs 的 `FSOptions.Allow(op, path) error` 钩子已存在，但 `jsrt` 没有暴露。

### 1.4 关键差距

1. **循环模型**：Node 是常驻循环；现在是调用边界。`net.Server`、`fs.watch`、后台 I/O 都依赖常驻。
2. **网络客户端仍有同步 Host 调用**：`net` 已异步化，但 `http` 仍整包缓冲；没有服务端、TLS、流式 HTTP。
3. **流没有完整背压**：fsx 逐块读，但上层 `Readable.from` 会尽快拉取；网络/HTTP 也没有流式桥。
4. **权限不可配置**：只有「能力装/不装」，没有「能访问哪些路径/主机」。
5. **限制不足**：无 fd/socket/body/timer 配额；无审计。
6. **异步语义缺口**：`nextTick` 顺序、顶层 await、`unhandledRejection`、`AbortSignal`。

---

## 2. 目标与非目标

### 2.1 兼容级别

| 级别 | 含义 | 验收方式 |
| --- | --- | --- |
| **C0** | 名字存在；调用抛 `ERR_METHOD_NOT_IMPLEMENTED` 或明确拒绝 | 导出面对表 |
| **C1** | 常用路径可用；同步或基础异步语义 | 单模块契约测试 |
| **C2** | Node 的主要语义：异步时序、事件、错误码、背压 | 兼容性用例 + 事件顺序测试 |
| **C3** | 边界细节与性能接近 Node | 边缘用例 + benchmark |

**目标**：`fs`、`net`（客户端）、`http`（客户端/服务端基础）、`stream`、`events`、`timers`、`dns` 达到 **C2**；`tls`、`child_process`、`zlib`、`crypto`、`process` 达到 **C1**；其余明确 C0/不支持。

### 2.2 目标

- 一个 Runtime 可以像 Node 进程一样常驻：定时器、服务器、socket、watcher、Promise 延续持续运行；
- 支持 `import` `node:*` 常用模块，形状与错误码对齐 Node；
- 文件、网络、子进程、env 都有显式权限配置；
- 权限默认拒绝，且不可被 JS 侧绕过；
- 嵌入式用法（`ada_go` 的插件）继续可用，不被 Node 模式破坏。

### 2.3 非目标

- 完整 Node.js；不实现 CommonJS、`node_modules`、原生 addon、`worker_threads`、`cluster`、`inspector`、`vm`、WASI；
- 进程/OS 级隔离；本权限模型是**同进程 API 级白名单**，不是沙箱；
- CPU 抢占；事件循环被死循环卡住时无法恢复（scoped 模式可中断，resident 模式视为损坏）；
- `process.exit` 直接杀宿主进程；
- HTTP/2、QUIC、WebSocket、`fetch`（可作为后续扩展）；
- 性能与 Node 对齐。

---

## 3. 总体架构

```text
┌──────────────────────────────────────────────────────────────┐
│ Node 兼容层（JS，运行在 moejs 内）                            │
│  node:fs / net / http / https / dns / tls / stream / timers  │
│  process / events / buffer / zlib / child_process ...        │
└───────────────▲──────────────────────────────────────────────┘
                │ 形状、参数、错误码、事件
┌───────────────┴──────────────────────────────────────────────┐
│ JS 运行时（Go）                                               │
│  Runtime / Loop（Scoped | Resident） / 模块装载 / 生命周期     │
│  __adaHost / __adaEntry / 全局（AbortController 等）           │
└───────────────▲──────────────────────────────────────────────┘
                │ 原生调用（JSON / base64 / handle）
┌───────────────┴──────────────────────────────────────────────┐
│ 能力守卫（Permission Guard）                                  │
│  FS 路径+读写 / Net host+IP+port / Child / Env / 配额         │
│  —— 所有原生入口先过这里，再进 provider                        │
└───────────────▲──────────────────────────────────────────────┘
                │
┌───────────────┴──────────────────────────────────────────────┐
│ Providers（默认 OS 实现；Host 可替换）                        │
│  FS: os.Root + fd 表 + moejs node fs + fsx                    │
│  Net: net.Dialer / net.Listen / net.Resolver / crypto/tls     │
│  Child: os/exec；Env: process env 视图                        │
└──────────────────────────────────────────────────────────────┘
```

三条硬规则：

1. **权限只在 Go 侧判定**；JS 侧不得作为唯一检查点。
2. **Provider 可替换，Guard 不可绕过**；自定义 `Host` 也必须经过 Guard。
3. **Node 兼容层不持有策略**；它只做 Node 形状与错误码。

---

## 4. 事件循环与异步模型

### 4.1 双模式

新增 `Options.Mode`：

| 模式 | 语义 | 适用 |
| --- | --- | --- |
| `LoopScoped`（现有，默认） | 一次 `Call` 跑一次 `loop.Run`；调用结束清定时器；超时可中断 | `ada_go` 插件、工具调用 |
| `LoopResident`（新增） | `New` 后 `loop.Start()` 常驻；定时器/服务器/IO 持续；`Close` 优雅停止 | Node 风格脚本、长驻服务 |

**迁移策略**：默认仍为 `LoopScoped`，避免破坏现有插件；Node 风格使用显式 `Mode: LoopResident`。长期可把新 API 的默认值切到 resident，但不在本设计内破坏性切换。

### 4.2 启动与关闭

Resident 的启动顺序：

1. 创建 `moejs.Runtime`、`eventloop.Loop`、fd/连接表、权限 Guard；
2. 注入 `__adaHost`、Node 模块、全局对象；
3. `loop.Start()`；
4. `Post(load)`：编译、`Link`、`Load` 入口；
5. 等待入口模块求值完成（含顶层 await）或 `StartupTimeout`；
6. 返回 `Runtime`，循环继续跑。

关闭顺序（`Close(ctx)`）：

1. 标记 `closing`，拒绝新的 `Call`；
2. 停止接收新连接（`net.Server` / `http.Server`）；
3. 关闭 watcher、socket、server、fd；
4. 清理 ref'd 定时器；拒绝/取消未结算的 Promise；
5. `loop.Stop()`；超过 `CloseTimeout` 则 `loop.Terminate()`；
6. 释放 runtime；返回错误（若有未收尾资源）。

`Wait(ctx)`（新增）：等待运行时自然退出（`ExitPolicy` 请求 exit 或 `Close`）。嵌入式场景通常不调用。

### 4.3 定时器

- `setTimeout/setInterval/setImmediate` 由 eventloop 提供，保留 `ref/unref`；
- Resident 模式不清理未到期定时器；`unref` 的定时器不阻止退出；
- Scoped 模式保留现有清理策略（`boot_timers.js`），避免调用间串扰；
- `timers/promises` 支持 `signal`；补 `AbortController/AbortSignal` 全局（见 §4.7）。

### 4.4 Promise / nextTick / 微任务

- `queueMicrotask` 使用引擎实现；
- `process.nextTick` 用一个 FIFO 队列 + 微任务冲刷实现。**已知偏差**：Node 的 nextTick 队列先于同一 tick 的 Promise 微任务，当前 moejs 没有暴露 job 队列钩子，只能做到「同一 checkpoint 内、按注册顺序」，无法保证 nextTick 绝对优先。列为 C1，待上游提供钩子后再升 C2。
- `unhandledRejection` 通过 `moejs.SetPromiseRejectionTracker` 接入，触发 `process.emit('unhandledRejection', reason, promise)`；默认记录日志，不退出（可配置）。
- `rejectionHandled` 同步接入。
- `uncaughtException` 通过 eventloop `OnError` 接入；有监听器时交给 JS；没有监听器时默认记录 + 标记损坏（resident）/ 中止调用（scoped）。宿主可通过 `OnEvent` 覆盖。

### 4.5 顶层 await（TLA）

moejs 本身支持：`Load` 返回 `ErrModulePending`，当依赖的 Promise 结算后继续求值。当前 `jsrt` 把它当错误。设计改为：

1. 在入口外再包一层「sentinel 模块」：

```js
import * as entry from "ada:entry";
globalThis.__adaEntry = entry;
globalThis.__adaReady?.();
```

2. `Load` 返回 `ErrModulePending` 时视为「加载中」，由 `__adaReady` 原生回调通知 Go；
3. Resident：循环继续跑，直到 ready 或 `StartupTimeout`；
4. Scoped：`loop.Run` 继续到 ready 或超时；
5. 超时/入口 Promise 永不结算 → `ErrUnstarted`，关闭运行时。

这要求启动期允许「尚未 ready 时存在后台任务」，与 resident 语义一致；scoped 模式也可以在启动阶段临时允许，加载完成后再进入调用边界模型。

### 4.6 `process.exit` 与退出码

新增 `Options.ExitPolicy`：

| 值 | 行为 |
| --- | --- |
| `ExitDeny`（默认） | `process.exit()` 抛 `ERR_ACCESS_DENIED`，不结束宿主 |
| `ExitRequest` | 记录退出码，走优雅关闭；`Wait` 返回 `ExitError{Code}`；不杀宿主进程 |

`process.exitCode` 可写；`beforeExit` 暂不支持（eventloop 没有 idle 回调），列为后续项。

### 4.7 AbortController / AbortSignal

moejs 未安装 `webapi`，当前没有全局 `AbortController`。在 boot 中提供一个最小 polyfill：

- `AbortController { signal, abort(reason) }`；
- `AbortSignal { aborted, reason, addEventListener/removeEventListener, throwIfAborted }`；
- 可选 `AbortSignal.timeout(ms)`、`AbortSignal.any([...])`；
- `http`、`fs`（支持 signal 的 API）、`timers/promises`、`stream` 使用它。

### 4.8 已知偏差（必须写进文档与测试）

| 项 | 偏差 |
| --- | --- |
| `nextTick` 顺序 | 不能保证先于 Promise 微任务 |
| `beforeExit` | 暂不触发 |
| 顶层 await | 新支持，但启动超时后不可恢复 |
| `process.exit` | 不杀宿主；默认拒绝 |
| 事件循环阻塞 | 无抢占；resident 模式只能关闭/重建 |
| `queueMicrotask` | 依赖引擎实现，顺序按引擎 |

---

## 5. 文件子系统

### 5.1 覆盖目标

在现有 moejs 核心 + fsx 的基础上补齐：

- 保留：`readFile/writeFile/appendFile/access/stat/lstat/readdir/mkdir/rm/rmdir/unlink/rename/copyFile/cp/glob/readlink/realpath`；
- 保留 fsx：fd 族、`FileHandle`、`Dir/opendir`、`readv/writev`、`createReadStream/createWriteStream`、`readLines`、`readableWebStream`；
- 新增：`fs.watch/watchFile/unwatchFile`（轮询后端，见 5.4）；
- 视平台补：`fdatasync`（`File.Sync`）、`statfs`（Unix `Statfs` / Windows API）、`openAsBlob`（依赖 Blob，后置）；
- 明确不补：`lchmod/lchown/lutimes`（平台差异大，先 C0 拒绝，文档写明）。

### 5.2 权限执行点

moejs 的 `FSOptions.Allow(op, path) error` 会在每个核心操作前被调用，`op` 是 `"read"|"write"`，`path` 是**已解析符号链接的虚拟绝对路径**。这正是权限钩子：

```go
opts := mojsnode.Options{
    Buffer: true,
    FS: &mojsnode.FSOptions{
        Root:  perm.Root,
        Mount: perm.Mount,
        Cwd:   perm.Cwd,
        Write: perm.Access == FSReadWrite,
        Workers: limits.FSWorkers,
        Allow: guard.FSAllow, // func(op, path string) error
    },
}
```

fsx 的每个操作在 `fsxDo` 入口调用同一个 `guard.CheckFS(op, path)`；核心 fs 与 fsx 共享一份策略，避免两套规则。

**必须注意**：`Allow` 返回的错误会被 moejs 映射成 `EACCES`（除非 `fs.ErrNotExist` 或 `syscall.Errno`）。权限错误返回 `syscall.EACCES` 或自定义 `fs.PathError`；需要携带 `resource` 时由 fsx 包装。

### 5.3 路径与符号链接

- JS 只能看到 `Mount`（默认 `/workspace`）下的虚拟路径；相对路径按 `Cwd` 解析；
- 宿主侧统一用 `os.Root` 打开，越界、`..` 逃逸、绝对路径越界一律拒绝；
- moejs 在调用 `Allow` 前已解析符号链接；`fsx` 同样使用 `os.Root`；
- **残余风险**：检查与使用之间的 TOCTOU（例如检查后被替换为链接）。缓解：所有操作走 `os.Root`，不把虚拟路径拼成宿主绝对路径；对高安全场景建议禁用符号链接（后续可加 `DenySymlinks`，需要在 `Allow` 之前遍历路径组件，当前不提供）。

### 5.4 `fs.watch`

- 权限：`FSPermission.Watch`，默认关闭；
- 后端：v1 用**轮询**（默认 500ms，可配），不引入新依赖；每个 watch 一个 goroutine + ticker，通过 `eventloop.Post` 投递 `change/rename` 事件，用 `Hold` 保持循环存活；
- 限制：`MaxWatches`；`unref` 支持；`fs.watchFile` 复用同一后端；
- 后续：可选 `fsnotify` 后端（新依赖需 ADR），接口不变。

### 5.5 流与 fd

- `FileHandle`、fd 表、`createReadStream/createWriteStream` 保留在 fsx；
- P5 重做流实现：`createReadStream` 用按需 `_read`（每次只读 highWaterMark），不预读整文件；`createWriteStream` 用写队列 + `drain`；
- `MaxOpenFiles` 在 fsx fd 表上强制；moejs 核心 fs 的瞬态 fd 由 `FSWorkers` 限制（文档写明不是精确 fd 配额）；
- `MaxFileBytes` 在 fsx 的 read/write/ftruncate、文件流与 `truncate` 上强制，核心 `readFile/writeFile/appendFile` 由 JS 包装层强制；
- `MaxDirEntries` 在 fsx `opendir/readdir` 与核心 `readdir` 包装层上强制。

### 5.6 权限错误

| 场景 | 错误 |
| --- | --- |
| 路径不在 root / mount | `EACCES`（moejs 核心行为）；fsx 当前是 `EPERM`，P2 统一为 `EACCES` |
| 路径被 `Allow/Deny` 拒绝 | `EACCES`，带 `path`、`syscall` |
| 只读工作区写 | `EROFS` |
| 文件超过 `MaxFileBytes` | `EFBIG`（自定义扩展，文档注明） |
| fd 超过 `MaxOpenFiles` | `EMFILE` |
| watch 未授权 | `ERR_ACCESS_DENIED` |

---

## 6. 网络子系统

### 6.1 Provider 与 Guard

新增 Go 侧 `Net` 能力（保留现有 `Host.NetDial/HTTPDo/DNSLookup` 兼容）：

```go
type NetProvider interface {
    Dial(ctx context.Context, network, address string, opts DialOptions) (Conn, error)
    Listen(ctx context.Context, network, address string, opts ListenOptions) (Listener, error)
    Resolve(ctx context.Context, host string, rrtype string) ([]Record, error)
}
```

（`Conn/Listener/Record/DialOptions/ListenOptions` 是示意类型，实现时再定签名；现有 `Host.NetDial/HTTPDo/DNSLookup` 继续作为兼容路径。）

`Guard` 在 provider 之前执行：

1. 解析目标（host/port/scheme）；
2. 匹配 `Allow` / `Deny`；
3. 解析 DNS；对解析出的每个 IP 再做 CIDR/私网检查（防 DNS rebinding）；
4. 检查协议、连接数、超时；
5. 通过后才调用 provider；
6. 记录审计事件。

### 6.2 `node:net`

- **客户端**：异步 `connect`（Promise / 回调 / 事件），`Socket` 支持 `data/end/close/error/timeout`、`pause/resume`、`write` 背压、`destroy`、`setNoDelay/setKeepAlive`；
- **服务端**：`createServer` / `listen` / `close` / `address`；Go `net.Listen` accept 循环，每个连接注册句柄，向 JS 投递 `connection`；
- **地址族**：TCP v4/v6；Unix socket / Windows named pipe 作为 P3 可选；
- **权限**：出站 `Allow`；入站 `Listen`；`MaxConnections`；`IdleTimeout`。

保留 `socket.read(n)` 作为扩展，但 Node 路径以事件/流为准。

### 6.3 `node:http` / `https`

- **客户端**：从「同步整包」升级为「异步 + 流式」：
  - `request/get` 返回 `ClientRequest`，`write/end` 支持分块；
  - 请求体通过 `io.Pipe` 桥接到 Go `net/http` Transport；
  - 响应体作为 `Readable` 回传，支持 `data/end/aborted/error`；
  - `Agent` 支持 keep-alive；`statusCode/headers/rawHeaders/trailers`；
  - **不自动跟随重定向**（与 Node `http` 一致）；高层 helper 如需要再显式实现；
- **服务端**：`http.createServer` / `listen`：
  - v1 用 Go `net/http` 解析与连接管理（成熟、限流/超时可靠），把 `request/response` 桥到 JS；
  - 强制 `MaxHeaderBytes`、body 上限、读写超时、慢连接保护；
  - `Upgrade/WebSocket` 不实现；
- **TLS**：`https` 与 `node:tls` 共用 Go `crypto/tls`；支持 `rejectUnauthorized/ca/servername/ALPN`；客户端证书后置；
- **权限**：URL host/port/scheme 逐跳检查；DNS 结果 IP 复查；重定向若由高层实现，必须逐跳复查。

### 6.4 `node:dns`

- 改为异步（goroutine + Promise），支持 `lookup/lookupAll/resolve4/resolve6/resolveCname/resolveNs/resolveMx/resolveTxt/resolveSrv/reverse`；
- `Resolver` 保留；自定义 DNS 服务器暂不支持；
- 权限：`Net.DNS` 控制模块可用性；解析结果仍受出站 IP 策略约束；
- 超时与取消：使用调用方 `ctx`。

### 6.5 `node:tls`

- `tls.connect(options, cb)` → 包装 Go TLS 握手，返回 TLSSocket（Duplex）；
- `tls.createServer(options, cb)` 由 `Host.TLSListen` 完成监听与握手，需 `LoopResident`；
- 证书校验默认开启；`rejectUnauthorized:false` 需显式配置；
- 权限：`Net.Protocols` 包含 `tls`；出站仍走同一 host/IP 检查；
- 不实现：OCSP、客户端证书、会话恢复调优（文档注明）。

### 6.6 SSRF 与资源限制

- **DNS rebinding**：先按 hostname 检查 allow，再解析并逐个 IP 检查 deny/私网；默认拒绝私网/环回，除非 `AllowPrivateIPs` 或 `Allow` 是覆盖该 IP 的 CIDR；
- **重定向**：`http` 不自动跟随；若上层实现跟随，每一跳重新过 Guard；
- **URL 解析**：拒绝 userinfo 中的凭据、非 http(s) 协议（除非显式允许）、超长 URL；
- **限制**：`MaxConnections`、`MaxRequestBytes`、`MaxResponseBytes`、`DialTimeout`、`IdleTimeout`、`MaxHeaderBytes`；
- **审计**：每次 dial/listen/resolve 记录 `{scope, op, resource, allowed, rule}`。

---

## 7. IO / Streams

### 7.1 目标语义

补齐到 C2 的关键点：

- `Readable`：`_read` 按需、`push` 返回背压、`pause/resume`、`pipe`、`unpipe`、`asyncIterator`、`destroy`；
- `Writable`：`_write/_writev`、`highWaterMark`、`write` 返回值、`drain`、`cork/uncork`、`end/finish`；
- `Duplex/Transform/PassThrough`；`objectMode` 的基础语义；
- `pipeline` / `finished`（回调 + Promise）；
- 编码：`utf8/utf16le/latin1/base64/base64url/hex`；
- `Readable.toWeb/fromWeb`、`Writable.toWeb/fromWeb` 作为可选项（P6）。

### 7.2 与 fs/net/http 的集成

- fs 流：按块异步读/写，不预读整文件；
- socket：`Duplex`，读事件从 Go 连接投递到 loop，写走异步队列；
- HTTP：请求体 `Writable`、响应体 `Readable`，都用 `Hold` 保持循环；
- 所有跨边界数据仍走 JSON/base64；大块数据分片（建议 64 KiB），避免单次 JSON 过大。

### 7.3 zlib / readline / crypto 流

- `zlib`：增加异步与流式（`createGzip/createGunzip/...`），输出上限；
- `readline`：基于新的 `Readable` 重做 `Interface`（含 `crlfDelay`、`history` 可后置）；
- `crypto`：`Hash/Hmac` 支持流式 update/digest；对称加密后置。

### 7.4 Web Streams

- 当前 fsx 的 `readableWebStream` 是最小实现；
- P6 视需要补 `ReadableStream/WritableStream/TransformStream` 的适配；不冒充完整 WHATWG 语义。

---

## 8. 权限模型

### 8.1 原则

- **默认拒绝**：零值 `Permissions` 不授予任何文件/网络能力；纯 JS 模块仍可用；
- **显式开启**：`FS.Enabled` / `Net.Enabled` 才安装对应模块；
- **Deny 优先**：`Deny` 命中即拒绝，不管 `Allow`；
- **Go 侧强制**：所有原生入口先过 Guard；JS 只负责把错误包装成 Node 形状；
- **不可动态提权**（v1）：权限在 `New` 时固定；审批/临时提权作为后续扩展点；
- **可观测**：每次拒绝/允许可回调、可计数。

### 8.2 Go 类型

```go
// Permissions 是运行时的能力白名单；零值 = 全部拒绝。
type Permissions struct {
    FS    FSPermission
    Net   NetPermission
    Child ChildPermission
    Env   EnvPermission
}

type FSAccess uint8

const (
    FSReadOnly FSAccess = iota // 默认：只读
    FSReadWrite
)

type FSPermission struct {
    Enabled bool
    Root    string // 宿主真实目录；Enabled 时必填
    Mount   string // JS 看到的根，默认 "/workspace"
    Cwd     string // 相对路径基准，默认 Mount
    Access  FSAccess
    // Allow 是读/写都适用的虚拟路径 glob；空 = Root 下全部。
    Allow []string
    // WriteAllow 只约束写；空 = Access=FSReadWrite 时 Root 下全部可写。
    WriteAllow []string
    // Deny 优先于 Allow/WriteAllow。
    Deny []string
    // Watch 允许 fs.watch/watchFile。
    Watch bool
    // 配额。
    MaxFileBytes int64
    MaxOpenFiles int
    MaxDirEntries int
}

type NetPermission struct {
    Enabled  bool
    Outbound bool
    Inbound  bool
    // Allow 是出站目标白名单；空 = 拒绝全部；"*" = 全部。
    // 语法见 8.3：host、*.example.com、scheme://host:port、CIDR。
    Allow []string
    Deny  []string
    // Listen 是入站绑定白名单；空 = 不允许监听。
    Listen []string
    // DNS 允许 node:dns 直接解析；出站 dial 的域名解析不受此项限制，
    // 但解析结果仍要过 IP 检查。
    DNS bool
    // 默认拒绝私网/环回地址；如需访问，显式打开或用 CIDR 允许。
    AllowPrivateIPs bool
    // 空 = 允许的协议全集；可选 "http","https","tcp","tls"。
    Protocols []string
    MaxConnections int
    MaxRequestBytes int64
    MaxResponseBytes int64
    DialTimeout time.Duration
    IdleTimeout time.Duration
}

type ChildPermission struct {
    Enabled bool
    Allow   []string // 可执行文件 basename 或绝对路径 glob；空 = 拒绝
    PassEnv []string // 允许透传的环境变量名
    Shell   bool     // 默认 false：只允许 execFile 语义，不允许 shell 解释
    MaxOutputBytes int64
    Timeout time.Duration
}

type EnvPermission struct {
    Allow []string // 允许读取的环境变量名 glob；空 = 无
    Set   bool     // 允许写 process.env（仅进程内视图）
}
```

### 8.3 匹配规则

**路径**：

- 使用虚拟绝对路径（`/workspace/...`），`path.Clean` 后匹配；
- 模式：`*` 匹配单段、`**` 匹配任意段、`?` 单字符；
- Windows 上大小写不敏感，其它平台敏感；
- `Deny` 优先；`Allow` 为空表示根下全部（因为 `Root` 已是硬边界，且 `Access` 控制读写）；
- 符号链接在 `Allow` 之前已解析；所有操作被 `os.Root` 限制在 root 内。

**网络**：

| 模式 | 含义 |
| --- | --- |
| `*` | 任意目标（仍需 IP 私网检查） |
| `api.example.com` | 精确主机，任意端口 |
| `*.example.com:443` | 任意子域，端口 443 |
| `https://api.example.com:443` | 协议 + 主机 + 端口 |
| `tcp://10.0.0.0/8:443` | CIDR + 端口 |
| `127.0.0.1:8080` | IP + 端口 |

- `Deny` 优先；
- 出站按 hostname 检查 `Allow`；解析后的每个 IP 再按 CIDR/`Deny`/私网规则检查；
- `AllowPrivateIPs=false` 时，未经 CIDR 明确允许的私网/环回地址一律拒绝；
- 入站 `Listen` 必须匹配；默认建议只允许 `127.0.0.1:0` 这类回环临时端口。

### 8.4 默认值

| 项 | 默认 |
| --- | --- |
| `Permissions` | 零值，全部拒绝 |
| `Mode` | `LoopScoped`（兼容） |
| `FSPermission.Access` | `FSReadOnly` |
| `FSPermission.Allow` | 空 = root 下全部（读/写均由 `Access` 与 `WriteAllow` 约束） |
| `FSPermission.Watch` | false |
| `NetPermission.Allow` | 空 = 拒绝全部；`"*"` 才是放开 |
| `NetPermission.Listen` | 空 = 不允许监听 |
| `NetPermission.DNS` | false |
| `NetPermission.AllowPrivateIPs` | false |
| `ExitPolicy` | `ExitDeny` |
| `Limits` | 零值 = 不额外限制（内存上限沿用 `MemoryLimit`） |

### 8.5 错误映射

| 场景 | `err.code` | 附加字段 |
| --- | --- | --- |
| fs 路径被拒 | `EACCES` | `path`, `syscall` |
| fs 越界 | `EACCES`（目标统一；fsx 当前 `EPERM`） | `path` |
| fs 只读 | `EROFS` | `path` |
| net 出站被拒 | `ERR_ACCESS_DENIED` | `syscall: "connect"`, `resource` |
| net 入站被拒 | `ERR_ACCESS_DENIED` | `syscall: "listen"`, `resource` |
| dns 被拒 | `ERR_ACCESS_DENIED` | `syscall: "lookup"`, `hostname` |
| child 被拒 | `ERR_ACCESS_DENIED` | `syscall: "spawn"`, `resource` |
| env 写入被拒 | `ERR_ACCESS_DENIED` | `resource: name` |
| watchdog/内存 | `ERR_MEMORY_LIMIT` / `ERR_ADA_TIMEOUT` | 自定义码，文档注明 |

`process.permission.has(scope, resource)` 返回布尔值，只读镜像当前权限，不做提权。

### 8.6 审计

- `Options.OnEvent` 接收 `permission` 事件：`{scope, op, resource, allowed, rule}`；
- 可选计数器：`runtime.Stats().PermissionDenied`；
- 日志脱敏：不记录文件内容、URL 中的凭据、环境变量值。

### 8.7 威胁模型（必须写清楚）

**能防**：

- 脚本使用 Node API 访问未授权的路径、主机、子进程、env；
- 通过 `..`、绝对路径、DNS 解析/重定向绕过静态 allowlist（配合 os.Root 与 IP 检查）；
- 通过未声明的内置模块获得能力（模块不安装）。

**不能防**：

- moejs 引擎漏洞、Go 标准库漏洞、原生代码执行；
- CPU 死循环（无抢占；只能 close/recreate）；
- 内存耗尽之外的系统级资源滥用（需 OS 配额）；
- 权限检查与使用之间的 TOCTOU 残余（已用 os.Root 缓解）；
- 恶意宿主自定义 `Host` provider 绕过 Guard（宿主代码即信任边界）。

**结论**：这是「API 级白名单」，不是沙箱；对不可信代码应使用子进程 + OS 隔离。

---

## 9. Options 设计

### 9.1 完整 API（草案）

```go
type LoopMode uint8

const (
    LoopScoped LoopMode = iota // 兼容现有插件：一次调用一次循环
    LoopResident               // Node 风格：常驻循环
)

type ExitPolicy uint8

const (
    ExitDeny ExitPolicy = iota
    ExitRequest
)

type Limits struct {
    MemoryBytes    int64
    StartupTimeout time.Duration
    CallTimeout    time.Duration // Scoped：中断；Resident：视为损坏并关闭
    CloseTimeout   time.Duration
    MaxTimers      int
    MaxListeners   int
    MaxSockets     int
    MaxWatches     int
    MaxHTTPHeaderBytes int
    MaxHTTPBodyBytes   int64
    FSWorkers      int // 透传 moejs FSOptions.Workers
}

type Event struct {
    Kind     string // "permission" | "uncaught_exception" | "unhandled_rejection" | "exit" | "warning"
    Err      error
    Scope    string
    Op       string
    Resource string
    Allowed  bool
    Code     int
}

type Options struct {
    Artifact *compile.Artifact

    Mode        LoopMode
    Permissions Permissions
    Limits      Limits
    ExitPolicy  ExitPolicy

    Logf    func(format string, args ...any)
    OnEvent func(Event)

    // 嵌入式扩展：为 nil 时使用内置 OS provider。
    // 权限 Guard 仍然先于这些函数执行。
    Host Host
    // 额外模块 / 全局，保留现有能力。
    Modules map[string]*moejs.Module
    Globals map[string]any

    // Deprecated: 用 Permissions.FS 替代；仅为兼容旧调用保留。
    Node *NodeOptions
}
```

新增导出：

```go
func (r *Runtime) Wait(ctx context.Context) error
func (r *Runtime) Permission() PermissionView
type PermissionView interface {
    Has(scope, resource string) bool
}
```

### 9.2 使用示例

```go
// 省略 import：context / time / ada_jsrt/compile / ada_jsrt/jsrt
art, _ := compile.Build("app", "main.ts", map[string]string{
    "main.ts": `
        import { readFile } from "node:fs/promises";
        import { request } from "node:https";
        // ...
    `,
}, compile.Options{})

rt, err := jsrt.New(ctx, jsrt.Options{
    Artifact: art,
    Mode:     jsrt.LoopResident,
    Permissions: jsrt.Permissions{
        FS: jsrt.FSPermission{
            Enabled: true,
            Root:    "/srv/app/workspace",
            Access:  jsrt.FSReadWrite,
            Allow:   []string{"/workspace/**"},
            Deny:    []string{"/workspace/secrets/**"},
            WriteAllow: []string{"/workspace/out/**"},
            MaxFileBytes: 8 << 20,
            MaxOpenFiles: 64,
        },
        Net: jsrt.NetPermission{
            Enabled:  true,
            Outbound: true,
            DNS:      true,
            Allow:    []string{"https://api.example.com:443"},
            MaxConnections: 8,
            DialTimeout: 5 * time.Second,
            MaxResponseBytes: 16 << 20,
        },
        Env: jsrt.EnvPermission{Allow: []string{"ADA_*"}},
    },
    Limits: jsrt.Limits{
        MemoryBytes:    256 << 20,
        StartupTimeout: 10 * time.Second,
        CloseTimeout:   5 * time.Second,
    },
    ExitPolicy: jsrt.ExitRequest,
})
if err != nil {
    return err
}
defer rt.Close(context.Background())
return rt.Wait(ctx)
```

JS 侧：

```js
import { readFile } from "node:fs/promises";
import { request } from "node:https";

const text = await readFile("/workspace/input.txt", "utf8");

await new Promise((resolve, reject) => {
  const req = request("https://api.example.com/data", (res) => {
    let body = "";
    res.on("data", (c) => (body += c));
    res.on("end", resolve);
  });
  req.on("error", reject);
  req.end();
});
```

### 9.3 兼容迁移

| 现有写法 | 迁移 |
| --- | --- |
| `Options.Node.FS.Root/Mount/Cwd/Write` | `Permissions.FS.Root/Mount/Cwd/Access` |
| `Options.Node.Buffer` | 保留；`node:buffer` 默认安装 |
| `Options.Host.NetDial/HTTPDo/DNSLookup` | 继续可用；若 `Permissions.Net.Enabled`，Guard 先检查再调用 Host |
| `Options.MemoryLimit` | 保留；`Limits.MemoryBytes` 为同义新名（二选一） |
| `Options.DisableDynamicCode` | 保留 |
| `Options.Host` 其余能力 | 保留；与 Node 模块并存，互不替代 |

`Node` 字段保留一个版本，文档标注 Deprecated；两者同时设置时以 `Permissions` 为准并记录 warning。

---

## 10. 资源限制

| 限制 | 执行点 | 生效阶段 | 说明 |
| --- | --- | --- | --- |
| `MemoryBytes` | moejs `Options.MemoryLimit` | P1 | Resident 的预算覆盖 Start→Stop |
| `StartupTimeout` | 启动等待（TLA/加载） | P1 | 超时关闭 |
| `CallTimeout` | Scoped：`loop.Interrupt` + 宽限；Resident：标记损坏 | P1 | Resident 无抢占 |
| `CloseTimeout` | `Close` | P1 | 超时 `Terminate` |
| `MaxTimers` | JS 包装层计数 | P1 | 防定时器炸弹 |
| `MaxListeners` | `events` 模块 | P5 | 默认 10，0 关闭 |
| `MaxSockets` | Net Guard | P3 | 出入站合计 |
| `MaxWatches` | fs watch 后端 | P2 | 防 watch 炸弹 |
| `MaxHTTPHeaderBytes` | HTTP 解析/服务端 | P4 | 防 header 膨胀 |
| `MaxHTTPBodyBytes` | HTTP client/server | P4 | 流式计数，超限中止 |
| `MaxFileBytes` | fsx + 核心 fs JS 包装层 | P2 | 已接 |
| `MaxOpenFiles` | fsx fd 表 | P2 | 已接；核心 fs 瞬态 fd 由 `FSWorkers` 限制，不是精确 fd 配额 |
| `MaxDirEntries` | fsx + 核心 readdir JS 包装层 | P2 | 已接 |
| `MaxOutputBytes` | child_process/zlib | P3/P5 | 输出截断/报错 |

---

## 11. 错误与诊断

- 统一 Node 风格 `code/syscall/path/resource`；
- 权限错误见 §8.5；Node 兼容错误沿用 `ENOENT/EACCES/ECONNREFUSED/ETIMEDOUT/...`；
- 非 Node 条件用 `ERR_ADA_*`，文档集中列出；
- JS 异常堆栈：利用 `compile.Artifact.SourceMap` 做「编译后位置 → 源文件位置」重映射（moejs 不支持 sourcemap，需宿主侧映射）；
- `uncaughtException/unhandledRejection/warning` 通过 `Options.OnEvent` 与 `process` 事件双通道暴露；
- `Close` 返回错误时聚合所有未收尾资源（`errors.Join`）。

---

## 12. 测试与验收

### 12.1 层次

| 层 | 内容 |
| --- | --- |
| 单元 | 权限匹配器、路径归一、URL/CIDR 解析、错误映射 |
| 模块契约 | 每个 `node:*` 模块的常用 API、错误码、回调/Promise 形态 |
| 事件循环 | timer/microtask/immediate 顺序、ref/unref、Close 时挂起任务、TLA、unhandledRejection |
| 流 | 背压、pipe、pipeline、asyncIterator、destroy、编码 |
| 权限矩阵 | 读/写/拒绝/只读/越界/符号链接/私网/DNS rebinding/监听 |
| 安全 | 路径穿越、SSRF、重定向、header 注入、慢连接、body 炸弹、fd 耗尽 |
| 兼容 | 与 Node 文档行为对表（不依赖 Node 运行） |
| 泄漏 | Close 后 goroutine/fd/socket/timer 归零 |
| 竞态 | `-race` |
| 性能 | 调用开销、fs 吞吐、HTTP RPS、流内存峰值 |

### 12.2 权限矩阵（示例）

| 用例 | 期望 |
| --- | --- |
| `Allow=/workspace/**`，读 `/workspace/a` | 允许 |
| `Deny=/workspace/secrets/**`，读该路径 | `EACCES` |
| `Access=FSReadOnly`，写 | `EROFS` |
| `WriteAllow=/workspace/out/**`，写 `/workspace/out/x` | 允许 |
| 同上，写 `/workspace/src/x` | `EACCES` |
| `/workspace/../../etc/passwd` | `EPERM` |
| 符号链接指向 root 外 | `EPERM` |
| `Net.Allow=api.example.com`，连接该主机 | 允许 |
| 同上，解析到私网 IP | `ERR_ACCESS_DENIED`（除非 `AllowPrivateIPs`/CIDR） |
| `Listen` 为空，`server.listen` | `ERR_ACCESS_DENIED` |
| `DNS=false`，`dns.lookup` | `ERR_ACCESS_DENIED` |

### 12.3 事件循环验收

- 入口 `await` 后返回，`data/end` 顺序正确；
- 未 `unref` 的定时器阻止 `Wait`；`unref` 后允许退出；
- `Close` 与挂起 I/O 并发：不挂起、不 panic、资源关闭；
- 顶层 await 成功/超时/拒绝三种路径；
- `unhandledRejection` 可被监听；无监听时按策略记录；
- 死循环：scoped 中断返回；resident 标记损坏并可重建。

### 12.4 安全验收

- 路径穿越与 Windows 8.3/大小写；
- DNS rebinding、IPv4/IPv6 混合、CIDR 边界；
- 重定向逐跳检查（若实现高层 helper）；
- HTTP header/body 限制、慢连接、gzip 炸弹；
- 权限错误不泄露被拒路径的内容（目录遍历拒绝时不要先列目录）。

### 12.5 性能目标

- 单次 `Call` 开销不显著劣化（现有预算 ≤20 µs）；
- fs 读写吞吐 ≥ 现有 fsx 的 80%；
- HTTP 客户端/服务端 RPS 达到同机 Go `net/http` 的合理比例（记录基线，不承诺 Node 对齐）；
- 大文件流内存峰值 ≤ `highWaterMark × 常数`。

---

## 13. 分阶段实施

| 阶段 | 内容 | 验收 | 估算 |
| --- | --- | --- | --- |
| **P0 权限骨架** | `Permissions/Limits/Event/Options` 类型；路径/网络匹配器；Guard 接口；审计钩子；不改行为 | 匹配器单测；旧测试全绿 | 1 周 |
| **P1 常驻循环** | `LoopResident`；启动/关闭；TLA；`Wait`；`unhandledRejection`；`AbortController`；exit 策略 | 事件循环用例；TLA；关闭泄漏测试 | 2 周 |
| **P2 文件权限** | `FSOptions.Allow` 接入；fsx 统一检查；只读/Allow/Deny；`fs.watch` 轮询；fd/大小配额 | 权限矩阵；越界/只读/符号链接；fs 回归 | 2 周 |
| **P3 网络客户端** | 异步 `net.Socket`；`dns` 异步；`tls.connect`；Net Guard；DNS rebinding | 客户端用例；SSRF 用例；超时/取消 | 2–3 周 |
| **P4 网络服务端** | `net.Server`；`http.Server`；流式 client/server；header/body/连接限制 | HTTP 并发/流式/慢连接；服务端权限 | 3 周 |
| **P5 IO/流** | 背压 `Readable/Writable`；fs/socket/http 流；zlib/readline | 流语义/背压/pipeline；内存峰值 | 2–3 周 |
| **P6 收口** | 兼容矩阵、错误码、sourcemap、web streams 可选、文档/bench/race/fuzz | 全量矩阵；`-race`；基准 | 2 周 |

依赖关系：P0 → P1 → P2 → P3 → P4 → P5 → P6；P2 可与 P3 部分并行，P5 可在 P4 前先做基础背压。

---

## 14. 设计决策与开放问题

### 决策

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | 保留双循环模式，默认 `LoopScoped` | 不破坏 `ada_go` 插件；Node 风格显式开启 |
| D2 | 保留 moejs 核心 fs + `Allow` 钩子 + fsx，不整体重写 | moejs fs 已覆盖大量操作；重写成本高、风险大 |
| D3 | 权限只在 Go Guard 判定 | JS 检查可被绕过；Provider 不可信 |
| D4 | HTTP 服务端用 Go `net/http` 桥接 | 解析/超时/安全成熟；自定义解析器风险高 |
| D5 | `fs.watch` 先轮询，后可选 fsnotify | 不新增依赖；语义可接受 |
| D6 | 默认拒绝；显式开启 | 插件运行时优先安全 |
| D7 | 不做完整 Node | 避免承诺无法维持的兼容面 |

### 开放问题

1. **默认模式**：未来是否把新 API 默认切到 `LoopResident`？需要一次版本化迁移。
2. **nextTick 顺序**：是否向上游 moejs 提 job-queue 钩子？没有钩子就只能 C1。
3. **HTTP 客户端**：Go `net/http` 桥接 vs JS 自写 HTTP/1.1；前者稳，后者更像 Node。建议先桥接。
4. **HTTP 服务端**：`net/http` 的 `Request/ResponseWriter` 与 Node 的 `IncomingMessage/ServerResponse` 映射成本；是否需要支持 `Upgrade`？
5. **TLS**：是否支持客户端证书、自定义 CA、`rejectUnauthorized` 的默认值。
6. **child_process**：是否开放 `spawn`（stdio 流）？建议先只做 `execFile` + 严格权限。
7. **Web API**：`fetch`、`WebSocket`、`Blob/File`、`TextEncoder` 是否纳入下一阶段。
8. **权限审批**：是否需要动态审批/临时提权（类似 `ada_go` 的 approval seam）？
9. **与 `jscore` 的关系**：本设计先在 `jsrt` 内实现；若后续抽 `jscore`，权限 Guard 与 Provider 应留在适配层还是核心层？

---

## 15. 附录：当前模块矩阵（速查）

| 模块 | 现状 | 目标 |
| --- | --- | --- |
| `fs` / `fs/promises` | C1 + 补齐 + watch 轮询 | C2（watch 轮询、流背压、权限） |
| `net` | C1 异步客户端 + C1 服务端 | C2 异步客户端 + C1 服务端 |
| `http`/`https` | C1 客户端 + C1 服务端（keep-alive/chunked/timeouts） | C2 流式客户端 + C1 服务端 |
| `dns`/`dns/promises` | C2 异步 | C2 异步 |
| `tls` | C1 客户端 + C1 服务端（宿主握手） | C1 客户端 + 服务端 |
| `stream` | C1+ 背压/管道/finished/_destroy + toWeb/fromWeb | C2 背压/管道 |
| `events` | C1 子集 | C2 常用语义 |
| `timers`/`timers/promises` | C1 调用边界 | C2 常驻 + signal |
| `buffer` | C2 大部分 | C2 |
| `zlib` | C1 同步 + C1 缓冲式流 | C1 异步/流 |
| `crypto` | C1 摘要/HMAC/PBKDF2/随机 | C1（对称/非对称后置） |
| `child_process` | C1 execFile | C1 + 权限 + 限额 |
| `process` | C1 受限（env 白名单、exit 策略、process.on） | C1 + process 事件 |
| `worker_threads`/`cluster`/`inspector`/`vm` | 不支持 | 保持不支持 |
| `fetch`/`WebSocket` | 无 | 后续可选 |

### 参考

- 当前实现：`jsrt/runtime.go`、`jsrt/loop.go`、`jsrt/boot.js`、`jsrt/fsx.js`、`jsrt/nodemods/src/*`
- 上游：moejs `node` 包文档与 `FSOptions.Allow`、`eventloop` 文档
- Node 权限模型：`--permission`、`--allow-fs-read`、`--allow-fs-write`、`--allow-net`
- 现有项目文档：`ada_go/docs/node-compat.md`、`ada_go/docs/event-loop.md`、`ada_go/docs/adr/0014`、`ada_go/docs/adr/0015`、`ada_go/docs/adr/0016`
