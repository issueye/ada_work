# Node 运行时兼容矩阵

本文件是 `node-runtime-design.md` 的配套速查，记录当前实际可用的模块、已知偏差与权限要求。  
状态定义：

- **C0**：不提供。
- **C1**：主要形状可用，语义有明确限制。
- **C2**：常用语义基本一致，边界差异已写入文档。
- **C3**：目标完整兼容（当前没有模块宣称 C3）。

## 模块矩阵

| 模块 | 状态 | 已支持 / 说明 |
| --- | --- | --- |
| `fs` | C2- | 核心读写、目录、元数据；`Allow` 权限；`fsx` 补齐 `chmod/chown/link/symlink/truncate/utimes/fd/FileHandle/opendir/文件流`；`watch/watchFile` 轮询；`MaxFileBytes/MaxOpenFiles/MaxDirEntries` |
| `fs/promises` | C2- | 同步模块的 Promise 形态；配额包装一致 |
| `net` | C1+ | 异步 `connect`、`data/end/close`、`createServer/listen`（Resident）；`read(n)` 是同步扩展；无 unix socket |
| `net`/`tls` 连接 | C1+ | `AbortSignal` 可中止连接；`Host.NetDial/TLSDial/TLSListen` 由宿主提供 |
| `tls` | C1 | `connect`、`secureConnect`、`createServer`（宿主握手）；无证书管理/会话复用细节 |
| `http` | C1+ | 客户端：Resident 流式（请求体增量、响应 data 流、CL/chunked），Scoped 整包；服务端 HTTP/1.1，支持 keep-alive、chunked 请求/响应、超时、`maxBodyBytes` |
| `https` | C1+ | 复用 `http` 服务端与 `tls` 监听；客户端仍整包 |
| `dns` | C2 | 回调与 `dns.promises` 走宿主异步解析；无回调调用保留同步便利返回 |
| `stream` | C1+ | `_read/_write/_flush/_destroy`、字节/对象水位、`drain` 背压、`pipe`、`pipeline/finished`、`Readable.from` 惰性、`toWeb/fromWeb` |
| `stream/promises` | C1 | `pipeline/finished` Promise 形态 |
| `stream/web` | C1 | 最小 `ReadableStream`/`WritableStream`；无完整 WHATWG 控制器/BYOB |
| `zlib` | C1+ | `*Sync` + `create*` 逐块流（压缩 Flush 输出，解压 pipe 增量；8MiB 输出上限） |
| `crypto` | C1 | 摘要/HMAC/PBKDF2/随机；无对称/非对称/证书 |
| `process` | C1 | `env` 白名单、`exit` 策略、`uncaughtException/unhandledRejection`、`nextTick/hrtime/uptime` 子集（时间基于 `Date.now`）；`nextTick` 顺序近似 |
| `child_process` | C1 | 仅 `execFile/execFileSync`；`ChildPermission` 白名单；无 spawn/fork/shell |
| `timers` / `timers/promises` | C1+ | 调用边界与 Resident 下均可用；`AbortSignal` 子集 |
| `events` | C1+ | `on/once/off/emit/removeListener` 等常用语义 |
| `buffer` | C2 | 由 moejs 提供；`Buffer` 全局按 `Node.Buffer` 安装 |
| `path` / `os` / `url` / `querystring` / `string_decoder` | C1+ | 常用 API；平台差异随宿主 |
| `util` / `assert` | C1 | 常用函数；`inspect` 为简化实现 |
| `readline` / `readline/promises` | C1 | 基于流；无完整终端能力 |
| `tty` / `perf_hooks` | C1 | 轻量实现 |
| `worker_threads` / `inspector` / `cluster` | C0 | 不提供 |

## 权限

默认全部拒绝；只有显式启用后对应 Host 入口才会工作：

| 权限 | 控制 |
| --- | --- |
| `FS` | 根 `Root`、`Mount`、`Access`、`Allow/WriteAllow/Deny`、`Watch`、`Max*` |
| `Net` | `Outbound/Inbound`、`Allow/Listen` 规则、`Protocols`、私网 IP 策略、连接/字节配额 |
| `Child` | `execFile` 程序名/路径白名单 |
| `Env` | `process.env` 键白名单与写入开关 |

## 模式

- **LoopScoped**（默认）：一次 `Call` 对应一次事件循环；服务端、`fs.watch/watchFile` 等需要跨调用保活的能力不可用。
- **LoopResident**：`loop.Start` + 永久 hold，支持定时器、服务端、watch、`Wait`；`Call` 串行。
- `ExitPolicy`：`ExitDeny`（默认）或 `ExitRequest`（`Wait` 返回 `ExitError`）。

## 已知差异

1. Scoped 下 HTTP 客户端整包缓冲；Resident 下为流式。
2. zlib 流式实现有 8MiB 输出上限，超过会报错；压缩级别不逐档开放。
3. Web Streams 只覆盖 `toWeb/fromWeb` 所需的 reader/writer，不是完整 WHATWG。
4. 核心 fs 的瞬态 fd 不精确计入 `MaxOpenFiles`（由 `FSWorkers` 限制）。
5. `process.nextTick` 使用微任务近似，不保证与 Promise 队列的 Node 级顺序。
6. 核心 fs 的 `watch` 为轮询，不是 fsnotify；延迟与事件合并不同。
7. HTTP 请求发出后无法 abort（宿主同步调用不可中断）。

## 验证

```powershell
cd ada_jsrt
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```

兼容层端到端用例见 `jsrt/compat_e2e_test.go`；基准见 `jsrt/bench_test.go`；fuzz 种子见 `jsrt/fuzz_test.go` 与 `compile/fuzz_test.go`。  
错误堆栈在 `compile.Options{SourceMap: true}` 时会通过 `Artifact.SourceMap` 映射回源文件。
