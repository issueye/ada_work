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

1. **一个 Runtime 一个 goroutine。** 所有 `Runtime` 访问都在 worker 上串行执行；只有 `Interrupt` / `Close` 可以跨协程。
2. **跨边界只传 JSON。** `Call`、`CallWithHost`、`Service.Call` 都会把参数与返回值做 JSON 编解码，不能传函数、Promise、生成器或循环引用。
3. **一次调用一次事件循环。** 事件循环不后台常驻：每次调用 `loop.Run`，入口 Promise 结算后返回。定时器、`node:fs/promises`、异步 HTTP 等都在这次调用内结算。
4. **取消是中断，不是抢占。** 调用 ctx 取消时向 JS 发 `Interrupt`；若 2 秒内仍不收尾，运行时会标记为损坏（`ErrBroken`），后续调用直接失败，因为继续复用可能半执行的堆更危险。
5. **字节码不落盘。** moejs 的字节码不可序列化；需要跨进程缓存时只能缓存 `Artifact`（JS 源码 + 可选 sourcemap）。

## 入口与调用

| 方法 | 用途 |
| --- | --- |
| `jsrt.New(ctx, Options)` | 编译 `Artifact`、`Link` 模块图、`Load` 入口并安装 Node 能力 |
| `(*Runtime).Call(ctx, export, args...)` | 调用入口模块导出的普通函数 |
| `(*Runtime).CallWithHost(ctx, export, args...)` | 调用前插入宿主 `ctx` 对象，约定写成 `apply(ctx, config)` |
| `(*Runtime).Exports()` | 返回入口模块的导出名（排序，去掉引导模块内部名），用于诊断 |
| `(*Runtime).Close(ctx)` | 停止 worker、关闭事件循环与连接；可重复调用 |
| `(*Service).Call(ctx, method, args...)` | 调用 JS 侧 `ctx.provide` 暴露的服务方法 |

## Options

| 字段 | 说明 |
| --- | --- |
| `Artifact` | 必填。`compile.Build` 的产物，装配前会做 `Verify`（入口存在、摘要匹配） |
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
| `Shell` / `ShellExec` | `ctx.shell` / `ada:shell` | 执行外部程序 |
| `Tool` | `ctx.tool(spec, fn)` | 注册工具 |
| `FSRead` / `FSWrite` / `FSEdit` / `FSSearch` / `FSList` | `ada:fs` | 宿主文件能力（是否沙箱化由宿主决定） |
| `HTTPDo` | `node:http` / `node:https` | 一次 HTTP(S) 请求，入参与返回值都是 JSON |
| `DNSLookup` | `node:dns` / `node:dns/promises` | 域名解析 |
| `NetDial` | `node:net` | 建立 TCP 连接，句柄由运行时登记、`Close` 时统一关闭 |
| `Secrets` | `ada:secrets` | 按名读取宿主 secret；没有服务时返回错误而不是空串 |

## Node 内置模块

- **moejs `node` 包直接提供**：`node:fs`、`node:fs/promises`、`node:buffer`（同时安装全局 `Buffer`）。
- **本库自建**（`jsrt/nodemods`）：`node:path`、`node:os`、`node:events`、`node:util`、`node:assert`、`node:url`、`node:querystring`、`node:string_decoder`、`node:process`、`node:child_process`、`node:http`、`node:https`、`node:dns`、`node:dns/promises`、`node:net`、`node:crypto`、`node:zlib`、`node:timers`、`node:timers/promises`、`node:stream`、`node:stream/promises`、`node:readline`、`node:readline/promises`、`node:tty`、`node:perf_hooks`。
- **`node:fs` 补齐层**（`fsx.js` + 宿主原生实现）：`mkdtemp`、`symlink`、`link`、`chmod`、`chown`、`truncate`、`utimes`、文件描述符族、`FileHandle`、`opendir`、文件流等。根固定为 `NodeFSOptions.Root`，脚本看到的是 `Mount`（默认 `/workspace`）；`fs.watch` 不提供。

进程级能力默认是关闭的：`process.exit` 会抛错而不是结束宿主进程，`env` 为空，没有 `worker_threads` / `inspector`。

## 错误

| 错误 | 含义 |
| --- | --- |
| `ErrClosed` | 运行时已关闭或尚未就绪 |
| `ErrBroken` | 中断后未能收尾，运行时不可复用 |
| `ErrUnsettled` | 入口的 Promise 永远不会结算：循环已无别的事可做 |
| `ErrReentrant` | 同一运行时在自己的回调链里被再次调用，必然死锁 |
| `*moejs.Exception` | JS 异常；`wrap` 会带上堆栈（指向编译后文本） |

## 测试

```sh
go test ./...
```

`compile` 覆盖 TS 降级与装饰器；`jsrt` 覆盖多模块、循环依赖、事件循环、`ctx.*`、服务、Node 内置模块与 fs 补齐。`-race` 也可直接跑。

## 来源

代码迁移自 `ada_go_v2/platform/compile` 与 `ada_go_v2/platform/jsrt`，行为不变，改动只有：

- Go 包路径：`github.com/issueye/ada_go_v2/platform/{compile,jsrt}` → `github.com/issueye/ada_jsrt/{compile,jsrt}`；
- 去掉对 `ada_go` 设计文档的注释引用，边界说明改为本 README 与包注释；
- 新增本 README 与可运行示例。
