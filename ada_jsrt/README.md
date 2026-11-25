# ada_jsrt

`ada_jsrt` 是一个**不依赖 Node、不依赖 CGO** 的脚本执行层：用 esbuild 把 TypeScript 降级成 ES2022，用 [moejs](https://github.com/MoeForge/moejs) 在 Go 进程内执行，并按 Node 的常用形状补齐内置模块与文件系统能力。

本库从 `ada_go_v2/platform/{compile,jsrt}` 抽出，作为可被多个宿主复用的独立 Go 模块。它只依赖 `moejs`、`esbuild` 与标准库，不认识任何 agent/内核概念。

## 包结构

| 包 | 职责 |
| --- | --- |
| `compile` | 逐文件 TS/TSX → ES2022 ESM；产出可落盘、可摘要校验的 `Artifact` |
| `jsrt` | 运行时：每个实例一个 moejs Runtime、一个专属 goroutine、一个事件循环与宿主能力桥 |
| `jsrt/nodemods` | 自建的 Node 内置模块（`embed` 的 ESM 源码） |

## 接入

本地开发时在调用方 `go.mod` 里加：

```go
require github.com/issueye/ada_jsrt v0.0.0

replace github.com/issueye/ada_jsrt => ../ada_jsrt
```

发布后改成真实版本号并删除 `replace`。

## 快速开始

```go
package main

import (
	"context"
	"fmt"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func main() {
	// 1. 把 TS 源文件逐个降级成 ES2022 模块，产物是纯数据。
	art, err := compile.Build("hello", "plugin.ts", map[string]string{
		"plugin.ts": `export function greet(name: string): string { return "hello, " + name }`,
	}, compile.Options{})
	if err != nil {
		panic(err)
	}

	// 2. 每个 Runtime 是一个独立的 JS 世界（单 goroutine、单模块图）。
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		panic(err)
	}
	defer rt.Close(context.Background())

	// 3. 调用入口模块的导出函数；参数与返回值只走 JSON。
	got, err := rt.Call(context.Background(), "greet", "ada")
	if err != nil {
		panic(err)
	}
	fmt.Println(got) // hello, ada
}
```

需要把宿主 `ctx` 对象传给脚本时，用 `CallWithHost`：它会在参数最前面插入 ctx，约定入口写成 `apply(ctx, config)`。

```go
_, err = rt.CallWithHost(ctx, "apply", map[string]any{"name": "demo"})
```

## 线程模型与硬边界

这些是 moejs 的硬约束，本库把它们固化在实现里：

1. **一个 Runtime 一个所有者 goroutine。** Scoped 模式下是 worker；Resident 模式下是常驻 loop goroutine。外部调用都投递到所有者上串行执行。
2. **跨边界只传 JSON。** `Call`、`CallWithHost`、`Service.Call` 都会把参数与返回值做 JSON 编解码，不能传函数、Promise、生成器或循环引用。
3. **两种事件循环模式。** 默认 `LoopScoped`：一次调用跑一次 `loop.Run`，调用结束清理定时器；`Mode: LoopResident`：`loop.Start()` 常驻，定时器、异步 I/O 与后台任务跨调用存活，用 `Wait` / `Close` 管理生命周期。
4. **取消是中断，不是抢占。** 调用 ctx 取消时向 JS 发 `Interrupt`；若 2 秒内仍不收尾，运行时会标记为损坏（`ErrBroken`）。Scoped 下清中断后可继续复用；Resident 下循环已停，只能关闭/重建。
5. **字节码不落盘。** moejs 的字节码不可序列化；需要跨进程缓存时只能缓存 `Artifact`（JS 源码 + 可选 sourcemap）。

## 入口与调用

| 方法 | 用途 |
| --- | --- |
| `jsrt.New(ctx, Options)` | 编译 `Artifact`、`Link` 模块图、`Load` 入口并安装 Node 能力 |
| `(*Runtime).Call(ctx, export, args...)` | 调用入口模块导出的普通函数 |
| `(*Runtime).CallWithHost(ctx, export, args...)` | 调用前插入宿主 `ctx` 对象，约定写成 `apply(ctx, config)` |
| `(*Runtime).Exports()` | 返回入口模块的导出名（排序，去掉引导模块内部名），用于诊断 |
| `(*Runtime).Close(ctx)` | 停止 worker、关闭事件循环与连接；可重复调用 |
| `(*Runtime).Wait(ctx)` | 仅 `LoopResident`：等待 `Close`；当前没有 beforeExit |
| `(*Service).Call(ctx, method, args...)` | 调用 JS 侧 `ctx.provide` 暴露的服务方法 |

## Options

| 字段 | 说明 |
| --- | --- |
| `Artifact` | 必填。`compile.Build` 的产物，装配前会做 `Verify`（入口存在、摘要匹配） |
| `Mode` | `LoopScoped`（默认）或 `LoopResident` |
| `Permissions` | 文件/网络/子进程/env 白名单；四种作用域均已接入执行入口 |
| `Limits` | 启动/调用/关闭超时与配额；当前内存上限、FSWorkers、启动超时生效 |
| `ExitPolicy` | `ExitDeny`（默认，`process.exit` 抛错）或 `ExitRequest`（请求关闭，`Wait` 返回 `ExitError`） |
| `OnEvent` | 权限拒绝、未处理拒绝等事件回调 |
| `MemoryLimit` | JS 堆上限（字节），0 表示不限 |
| `DisableDynamicCode` | 关闭 `eval` / `Function` 构造器 |
| `Logf` | 接收 JS 侧 `ctx.log` 与运行时诊断；nil 丢弃 |
| `Host` | 宿主能力实现，见下表；为 nil 的能力在 JS 侧调用即抛错 |
| `Modules` | 宿主注入的 moejs 模块（如 `ada:shell`），`Link` 时优先于相对路径 |
| `Globals` | 加载前注入的全局变量 |
| `Node` | Node 内置模块配置；nil 表示只装默认模块，不提供 `node:fs` 根 |

## 宿主能力（Host）

回调在 JS worker goroutine 上执行，实现方必须自己保证线程安全；回调返回的 error 会变成 JS 异常。

| 字段 | JS 侧入口 | 说明 |
| --- | --- | --- |
| `Provide` | `ctx.provide(name, value)` | 对象上的函数成员变成可调用方法，其余成员按 JSON 暴露 |
| `Emit` | `ctx.emit(event, payload)` | 广播事件 |
| `On` | `ctx.on(event, fn)` | 注册「宿主 → JS」回调；`invoke` 可在任意 goroutine 调用 |
| `Effect` | `ctx.effect(label, fn)` | 登记清理函数，由宿主决定何时按逆序执行 |
| `Waterfall` | `ctx.waterfall(event, fn)` | 注册可改写/否决的中间件，JS 侧同步调用 `next` |
| `Shell` / `ShellExec` | `ctx.shell` / `ada:shell` | 执行外部程序；`Permissions.Child` 生效时先检查程序白名单 |
| `Tool` | `ctx.tool(spec, fn)` | 注册工具 |
| `FSRead` / `FSWrite` / `FSEdit` / `FSSearch` / `FSList` | `ada:fs` | 宿主文件能力（是否沙箱化由宿主决定） |
| `HTTPDo` | `node:http` / `node:https` | 一次 HTTP(S) 请求，入参与返回值都是 JSON |
| `DNSLookup` | `node:dns` / `node:dns/promises` | 域名解析 |
| `NetDial` | `node:net` | 建立 TCP 连接，句柄由运行时登记、`Close` 时统一关闭 |
| `TLSDial` | `node:tls` | 建立 TLS 连接；`ServerName`/`Insecure`/`ALPN` 来自 JS options |
| `TLSListen` | `node:tls` / `node:https` | TLS 监听与握手；返回 `NetListener`，服务端需 `LoopResident` |
| `Secrets` | `ada:secrets` | 按名读取宿主 secret；没有服务时返回错误而不是空串 |

## Node 内置模块

- **moejs `node` 包直接提供**：`node:fs`、`node:fs/promises`、`node:buffer`（同时安装全局 `Buffer`）。
- **本库自建**（`jsrt/nodemods`）：`node:path`、`node:os`、`node:events`、`node:util`、`node:assert`、`node:url`、`node:querystring`、`node:string_decoder`、`node:process`、`node:child_process`、`node:http`、`node:https`、`node:dns`、`node:dns/promises`、`node:net`、`node:tls`、`node:crypto`、`node:zlib`、`node:timers`、`node:timers/promises`、`node:stream`、`node:stream/promises`、`node:stream/web`、`node:readline`、`node:readline/promises`、`node:tty`、`node:perf_hooks`。
- **`node:fs` 补齐层**（`fsx.js` + 宿主原生实现）：`mkdtemp`、`symlink`、`link`、`chmod`、`chown`、`truncate`、`utimes`、文件描述符族、`FileHandle`、`opendir`、文件流等。根固定为 `NodeFSOptions.Root`，脚本看到的是 `Mount`（默认 `/workspace`）；`fs.watch/watchFile` 在 `LoopResident` + `FSPermission.Watch` 下提供轮询实现，`unwatchFile` 可停止 `watchFile`。

进程级能力默认是关闭的：`process.exit` 默认抛错（`ExitRequest` 才请求关闭），`env` 只暴露 `EnvPermission` 白名单，没有 `worker_threads` / `inspector`。

## 错误

| 错误 | 含义 |
| --- | --- |
| `ErrClosed` | 运行时已关闭或尚未就绪 |
| `ErrBroken` | 中断后未能收尾，运行时不可复用 |
| `ErrUnsettled` | 入口的 Promise 永远不会结算：循环已无别的事可做 |
| `ErrReentrant` | 同一运行时在自己的回调链里被再次调用，必然死锁 |
| `*moejs.Exception` | JS 异常；`wrap` 会带上堆栈（`Artifact.SourceMap` 存在时映射回源文件） |

## 测试

```sh
go test ./...
```

`compile` 覆盖 TS 降级与装饰器；`jsrt` 覆盖多模块、循环依赖、事件循环、`ctx.*`、服务、Node 内置模块与 fs 补齐。`-race` 也可直接跑。

## 权限（部分实现）

`Options.Permissions` 是静态白名单；零值表示全部拒绝。当前 **FS 与 Net（Host 路径）已强制**：

```go
rt, err := jsrt.New(ctx, jsrt.Options{
    Artifact: art,
    Permissions: jsrt.Permissions{
        FS: jsrt.FSPermission{
            Enabled: true,
            Root:    "/srv/app/workspace",
            Access:  jsrt.FSReadOnly,
            Allow:   []string{"/workspace/**"},
            Deny:    []string{"/workspace/secret/**"},
        },
        Net: jsrt.NetPermission{
            Enabled:         true,
            Outbound:        true,
            DNS:             true,
            AllowPrivateIPs: false,
            Allow:           []string{"https://api.example.com:443", "tcp://10.0.0.0/8:9000"},
            Deny:            []string{"*.internal"},
        },
    },
    OnEvent: func(ev jsrt.Event) { /* 权限拒绝等事件 */ },
})
```

- 默认拒绝、`Deny` 优先；`Access=FSReadWrite` 才允许写，写还可由 `WriteAllow` 收窄；
- 核心 `node:fs` 走 moejs 的 `FSOptions.Allow`，fsx 的每个操作也会检查；fsx 先解析符号链接再判定，避免「允许的链接目录指向被拒绝目录」；
- `fs.watch` / `fs.watchFile` 已提供轮询实现（需 `LoopResident` 与 `FSPermission.Watch`），默认 500ms，可传 `{ interval }`；`fs.unwatchFile` 可停止 `watchFile`；
- `Host.NetDial/HTTPDo/DNSLookup` 前会检查 host/port/scheme、DNS 规则，并解析域名后逐个检查 IP（默认拒绝私网/环回，防 DNS rebinding）；
- `node:net` 的 `connect` 现在是异步的（成功后 `connect` 事件，数据经 `readAsync` 泵以 `data` 事件到达，连接前写入会排队），支持 `signal` 中止；`read(n)` 保留为同步扩展；
- `net.createServer/listen` 可用（需要 `Mode: LoopResident` 与 `NetPermission.Inbound + Listen`）；`http.createServer` 提供 HTTP/1.1 服务端，支持 keep-alive、chunked 请求/响应、`headersTimeout`/`requestTimeout` 与 `maxBodyBytes`；
- `node:http`/`https` 客户端在 `LoopResident` 下为流式（请求体增量写入、响应 `data` 按到达顺序派发，支持 Content-Length/chunked）；Scoped 下仍是同步整包；`signal` 在请求真正发出前中止，服务端支持 keep-alive/chunked；
- `node:tls` 的 `connect` 复用 `net.Socket`，由宿主 `Host.TLSDial` 完成握手，并补 `secureConnect`；`tls.createServer` 通过 `Host.TLSListen` 提供 TLS 服务端（需 `LoopResident`），`https.createServer` 复用同一监听器；
- `NetPermission.MaxConnections` / `MaxRequestBytes` / `MaxResponseBytes` 已接入连接预留与 HTTP 请求/响应体限制；
- `FSPermission.MaxFileBytes` / `MaxOpenFiles` / `MaxDirEntries` 已在 fsx 的读写、fd、opendir/readdir 上强制，核心 `readFile/writeFile/appendFile/readdir` 的 sync/callback/promises 形态也加了配额包装；`MaxOpenFiles` 的核心 fs 瞬态 fd 仍由 Workers 限制；
- `node:dns` 的回调形态与 `node:dns/promises` 已走宿主异步解析（`dnsLookupAsync`）；无回调调用仍保留同步便利返回。
- `node:stream` 支持 `_read/_write/_flush/_destroy`、按字节或对象的高水位、`drain` 背压、`pipe`、`pipeline/finished` 与 `Readable.from` 惰性拉取；`Readable/Writable.toWeb/fromWeb` 提供最小 Web Streams 互操作（`node:stream/web`）；完整 WHATWG 语义仍待补。
- `node:zlib` 新增 `createGzip/createGunzip/createDeflate/createInflate/...`，使用宿主流式句柄：压缩每次 Flush 产生可用字节，解压通过 pipe 增量读取；仍受 `maxOutputLength`/8MiB 输出上限保护。
- `Permissions.Child` 已接到 `child_process.execFile/execFileSync`（按程序名/路径白名单）；
- `Permissions.Env` 已接到 `process.env`（只暴露白名单键；`Set` 控制写入）；
- `process.on("uncaughtException" | "unhandledRejection")` 已接入宿主事件；`ExitPolicy` 已接线；
- `process.hrtime/uptime` 基于 `Date.now`（提供 `hrtime.bigint`），不是严格单调时钟；

## 设计文档

- [Node 风格运行时设计（fs / net / IO / 异步 / 权限）](docs/node-runtime-design.md)：现状差距、双循环模式、文件与网络权限 `Options` 草案、分阶段实施计划。
- [Node 运行时兼容矩阵](docs/node-runtime-compat.md)：按模块记录 C1/C2 状态、权限要求、运行模式与已知差异。**核心阶段已落地，兼容矩阵持续更新。**

## 来源

代码迁移自 `ada_go_v2/platform/compile` 与 `ada_go_v2/platform/jsrt`，行为不变，改动只有：

- Go 包路径：`github.com/issueye/ada_go_v2/platform/{compile,jsrt}` → `github.com/issueye/ada_jsrt/{compile,jsrt}`；
- 去掉对 `ada_go` 设计文档的注释引用，边界说明改为本 README 与包注释；
- 新增本 README 与可运行示例。
