# seam/rpc 与 RPC 通讯协议插件

> 状态:**P0–P3 已实现,P4 TLS 已实现(2026-11-20);`SO_PEERCRED` 待做,桌面接入原型已落地(`cmd/ada-rpc-agent`)**。已落地:`plugins/seam/rpc`(契约)、`plugins/provider/rpc-json`(帧/握手/调用/事件/取消)、`plugins/provider/rpc-net`(tcp/unix)、`plugins/provider/rpc-pipe`(Windows 命名管道,overlapped 流)、`plugins/policy/rpc-guard`(授权/地址/方法/对端/鉴权)、`plugins/policy/rpc-observe`(`rpc/observe`)、`plugins/provider/rpc-tls`(`tls://`)、`cmd/ada-rpc-agent`(把 agent preset 做成每机器人一个进程的 RPC 宿主)。
>
> 五层控制架构(RPC/ACP)见 [rpc-control-layers.md](rpc-control-layers.md)。
>
> 关联:[plugin-kernel-design.md](plugin-kernel-design.md)(§2 原则、§13 安全)、[seam-api.md](seam-api.md) §1.2(错误码)、[caps.md](caps.md)、[host-server.md](host-server.md)、[ADR 0008](adr/0008-desktop-webview-shell.md)、[ADR 0009](adr/0009-rpc-json-budget.md)、AGENTS §2–§5。

## 0. 结论摘要

**一句话**:把「进程间控制面调用」做成 seam/provider/policy 三件套——协议自带版本、鉴权、限流、取消与事件;底层传输由 `tcp` / `unix`(含 Windows AF_UNIX)/ `pipe`(Windows 命名管道)三个实现择一注册;业务方法由宿主或核心插件注册,RPC 插件本身不认识 agent。

| # | 决策 | 替代方案 | 理由 |
|---|---|---|---|
| D1 | **只做控制面**:方法调用、通知、事件;大对象走 URL/句柄 | 把文件/消息体塞进 RPC | P8;JSON 边界不承载 > 256 KiB 对象 |
| D2 | **自定义帧协议**(4 字节长度 + 1 字节 kind + JSON body) | gRPC、`net/rpc`(gob)、WebSocket、裸 JSON 行 | 无 protobuf/CGO/代码生成;显式版本/上限/取消;帧边界不受内容影响 |
| D3 | **一条连接多路复用**,`id` 关联请求与响应,单写者串行落帧 | 每请求一条连接 | 连接建立/鉴权成本高;单写者让顺序确定、无锁竞争 |
| D4 | **传输可替换**,地址用 scheme 表达 | 只支持 TCP;只支持 Unix socket | 同一协议在 Linux/macOS/Windows 各自用最合适的本地 IPC |
| D5 | **鉴权分两档**:`os`(文件权限/管道 ACL)与 `token`(HMAC 挑战应答) | 明文 Bearer token;无鉴权 | 本地 IPC 让内核对端身份;TCP 不把 token 放进明文帧 |
| D6 | **出站按 caps、入站默认拒绝** | 入站也走事件/无门禁 | AGENTS §0.3、P7;方法白名单必须是确定性的门禁 |
| D7 | **事件独立于调用**:订阅 + `seq` + gap 检测 | 把事件当返回值;事件做鉴权 | 事件是广播,不是门禁;订阅是显式动作 |
| D8 | **协议不做自动重连/重试**:at-most-once | 内置重试/幂等缓存 | 重试可能重复执行工具;重连策略属于宿主 |

**不做什么**:gRPC/protobuf、WebSocket、服务发现、负载均衡、跨机集群、RPC 传大文件、用 RPC 装载插件、v1 迁移。

---

## 1. 范围与归属

### 1.1 它是什么

| 场景 | 走 RPC | 不走 RPC |
|---|---|---|
| 桌面壳 ↔ Go 内核(两个进程) | 方法调用 + 事件 | WebView2 Bind/Electron IPC 仍由 host 适配器选择 |
| `ada` CLI/GUI 附加到常驻 agent 进程 | `attach` 后的控制面 | —— |
| 多个本地客户端共享一个 agent | 会话/审批/查询方法 | 业务逻辑仍在 core 插件 |
| 跨机远程客户端(未来) | `tls://` 上的同一协议 | 公网暴露需要网关/鉴权,不在本刀 |

### 1.2 归属(AGENTS §3 决策树)

| 部分 | 归属 | 判据 |
|---|---|---|
| `plugins/seam/rpc` | **seam**(接口/错误/事件名 + Chain/Registry 这类无 I/O 的编排) | 定义「进程间调用的接口」;删掉它产品仍可只用 HTTP/SSE |
| `plugins/provider/rpc-json` | **provider** | 实现 seam,只做一次调用的编解码/分发 |
| `plugins/provider/rpc-net` / `rpc-pipe` | **provider** | 实现传输,不含协议语义 |
| `plugins/policy/rpc-guard` | **policy**(装饰器) | 确定性阻断:地址/方法/对端/鉴权 |
| `platform/host/*` | **宿主适配器** | 只把具体方法映射到服务;不定义协议 |

**为什么不是 L2 基础设施**:RPC 不是「装载/编译/执行/授权」机制,而是一项可替换的通信能力;产品完全可以用 HTTP/SSE 代替(见 host/web)。因此它必须是插件,不能进 `platform/`。

**为什么不是内核**:内核不认识「方法」「对端」「连接」;这些词汇一旦进 `platform/kernel/`,就违反红线 1。

### 1.3 与现有宿主协议的关系

- `host/server` 是**只读 HTTP API**(GET + JSON),面向浏览器/脚本,不引入长连接、调用关联与事件订阅。
- `host/web`(UI 提案)的 SPA 通道是 **HTTP + SSE**;WebView2/Electron 用宿主自己的 Bind/IPC。RPC 不替代它们。
- RPC 解决的是「**进程边界、双向、长连接、有状态**」的控制面;三者可以共存:同一进程内 `host/web` 与 `host/desktop` 都可以把 `rpc` 服务当作下游。

---

## 2. 分层与插件清单

```
plugins/seam/rpc/             Address / Frame / Message / Stream / Transport / Peer / Server
                              Chain(出站装饰器) / Registry(传输注册) / 错误码 / 事件名
plugins/provider/rpc-json/    协议:帧编解码、握手、请求分发、事件、取消、心跳   ✅ P0
plugins/provider/rpc-net/     传输:tcp + unix(Windows 走 AF_UNIX)              ✅ P1
plugins/provider/rpc-pipe/    传输:Windows 命名管道(仅 windows 构建注册)         ✅ P2
plugins/policy/rpc-guard/     出站 caps/方法/地址白名单;入站 peer/方法白名单 + principal 映射  ✅ P3
plugins/policy/rpc-observe/   观察:rpc/observe(不阻断)                          ✅ P3
plugins/provider/rpc-tls/     传输:tls://(证书/CA,远程 TCP 需该传输)             ✅ P4(TLS)
```

服务名:

| 服务 | 类型 | 提供者 | 用途 |
|---|---|---|---|
| `rpc` | `*rpc.Chain` | `seam/rpc` 提供空链;`rpc-json` `Install` 底座 | 消费者 `Dial`/`Listen` |
| `rpc-transport` | `*rpc.Registry` | `seam/rpc` | `rpc-net`/`rpc-pipe`/`rpc-tls` 按 `Network` 注册 |

依赖方向:`rpc-json` **inject** 必需 `rpc-transport`(缺失时插件停在 `pending`,装配期即可发现);传输 provider **inject** `rpc-transport` 并 `Register`;`rpc-guard` **inject** `rpc`,`caps`、`secrets` 可选;`rpc-observe` **inject** `rpc`。

### 2.1 seam 接口草图(只类型,不实现)

```go
package rpc

type Network string

const (
    TCP  Network = "tcp"  // Linux/macOS/Windows(Winsock)
    Unix Network = "unix" // Linux/macOS/Windows AF_UNIX(1803+)
    Pipe Network = "pipe" // 仅 Windows 命名管道
)

type Address struct {
    Network Network
    Name    string // tcp: host:port;unix: 绝对路径;pipe: 名字(不含 \\.\pipe\)
}

func Parse(raw string) (Address, error) // "tcp://127.0.0.1:7331" / "unix:///run/ada/agent.sock" / "pipe://ada-agent"
func (a Address) String() string

// Stream 是传输层的双向字节流;SetDeadline 可返回 ErrUnsupported(pipe 同步句柄)。
type Stream interface {
    io.ReadWriteCloser
    SetDeadline(t time.Time) error
}

type Transport interface {
    Dial(ctx context.Context, addr Address) (Stream, error)
    Listen(ctx context.Context, addr Address, opts ListenOptions) (Listener, error)
}

type Listener interface {
    Accept(ctx context.Context) (Stream, error)
    Addr() Address
    Close() error
}

// Handler 是入站方法;params 是原始 JSON,由方法自己解码。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

type Event struct {
    Topic string
    Seq   uint64
    Data  json.RawMessage
}

type Peer interface {
    Call(ctx context.Context, method string, params, result any) error
    Notify(ctx context.Context, method string, params any) error // 无响应
    Subscribe(ctx context.Context, topics ...string) error
    Events() <-chan Event
    Remote() string
    Close() error
}

type Server interface {
    Handle(method string, h Handler) (kernel.Dispose, error)
    Publish(topic string, data any) error
    Serve(ctx context.Context) error // 阻塞到 Close/ctx 取消
    Stats() Stats
    Close() error
}

// Stats 是只读诊断计数(供 ada inspect / 宿主视图使用)。
type Stats struct {
    Listeners []Address
    Conns     int
    Inflight  int
    FramesIn  uint64
    FramesOut uint64
    EventsOut uint64
}

// Auth 是鉴权配置;Token 由调用方从 seam/secrets 解析后传入,provider 不读 secret。
type Auth struct {
    Mode          AuthMode // OS | Token | None
    Token         []byte   // Mode=Token 时必填
    AllowInsecure bool     // Mode=None 时必须显式打开
}

type AuthMode string

const (
    AuthOS    AuthMode = "os"    // 依赖 unix 0600 / 管道 ACL;用在 tcp 上直接 EINVALID
    AuthToken AuthMode = "token" // HMAC 挑战应答
    AuthNone  AuthMode = "none"  // 仅测试,必须 AllowInsecure
)

type Limits struct {
    MaxFrameBytes  int // 默认 16 MiB
    MaxParamsBytes int // 默认 1 MiB
    MaxInflight    int // 每连接,默认 32
    MaxEventQueue  int // 每连接帧数,默认 1024
    MaxConns       int // 默认 64
}

type DialOptions struct {
    Client  string        // 客户端名,进入握手
    Auth    Auth          // 鉴权
    Timeout time.Duration // 握手+连接,默认 10s
}

type ServerOptions struct {
    Auth    Auth
    Limits  Limits
    PeerIDs []string // 允许的客户端名;空 = 拒绝所有(必须显式配置)
}

// ListenOptions 是传输层选项,不含协议语义。
type ListenOptions struct {
    Perm        os.FileMode // unix socket 权限,默认 0600
    RemoveStale bool        // 仅删除属主为当前用户的 stale socket
    MaxConns    int         // 传输层接受上限,0 = 不限(协议层仍限)
}

// Service 是 rpc-json 实现的协议入口;Chain 包裹它。
type Service interface {
    Dial(ctx context.Context, addr Address, opts DialOptions) (Peer, error)
    Listen(ctx context.Context, addr Address, opts ServerOptions) (Server, error)
}
```

`Chain` 与 `seam/fs` 同构:`Install(base Service)`(重复安装报错)、`Decorate(fn, priority)`(越大越靠外)、`Get()`(无底座 → `EUNIMPLEMENTED`)。`Registry` 同理:每个 `Network` 只能注册一次,`Dispose` 反注册。

```go
type Registry struct{ /* 互斥表 */ }

func (r *Registry) Register(n Network, t Transport) (kernel.Dispose, error)
func (r *Registry) Lookup(n Network) (Transport, bool)
```

事件名与错误构造:`const EventObserve = "rpc/observe"`;`Invalid(msg)`/`Denied(msg)` 与其它 seam 一致地返回 `seamerr` 带码错误。

### 2.2 为什么协议与传输分成两个 provider

- 传输 provider 只依赖标准库的 `net` 与 `x/sys/windows`,可以被第二个协议实现复用(例如未来紧凑二进制编解码)。
- 协议 provider 不知道字节从哪来,因此可以用 `net.Pipe`/内存管道做契约测试,不需要真实端口。
- 两者都符合「一个插件只做一件事」;合在一起会变成 1000+ 行的混装插件,且 Windows 代码会污染所有平台。

---

## 3. 传输层

### 3.1 地址语法

| scheme | 规范形式 | 备注 |
|---|---|---|
| `tcp` | `tcp://127.0.0.1:7331` | IPv6 用 `tcp://[::1]:7331`;空 host 视为 `127.0.0.1`(仅监听) |
| `unix` | `unix:///run/ada/agent.sock` | 必须是绝对路径;不做相对路径解析 |
| `pipe` | `pipe://ada-agent` | 仅 Windows;规范化为 `\\.\pipe\ada-agent` |
| `stdio` | `stdio://local` | 单连接 stdin/stdout;由 `rpc-stdio` 提供,不注册为 builtin |

规则:未知 scheme / 空 Name / `..` 路径段 / NUL 字符 → `EINVALID`;`pipe` 名字不得含 `\`、`/`、`..`;`stdio` 只接受 `stdio://local`。

### 3.2 平台矩阵

| 传输 | Linux | macOS | Windows | 默认鉴权 | 说明 |
|---|---|---|---|---|---|
| `tcp` | ✅ | ✅ | ✅ Winsock | `token` | 默认只监听 loopback;远程需 TLS(P4) |
| `unix` | ✅ | ✅ | ✅ AF_UNIX(10.0.17063+) | `os` | Windows 只支持文件路径,无抽象命名空间;路径权限语义弱于 Linux |
| `pipe` | ❌ | ❌ | ✅ 命名管道 | `os` | 仅 windows 构建注册;非 Windows 返回 `EUNIMPLEMENTED` |
| `stdio` | ✅ | ✅ | ✅ | `none` | 单连接、由宿主拥有;ACP/CLI attach 使用 |

Windows 上首选 `pipe`(ACL + `PIPE_REJECT_REMOTE_CLIENTS`),`unix` 只用于与其它平台的测试/互操作。`tcp` 在 Windows 上即 Winsock;`pipe` 是命名管道;两者都保留。

### 3.3 传输契约

1. `Dial`/`Listen` 必须遵守传入 ctx;取消后返回 `ECANCELED`。
2. `Accept` 阻塞直到有连接或 ctx 取消;**禁止 sleep 轮询**。
3. `Close` 幂等;关闭后 `Accept` 返回 `EUNAVAILABLE`;未完成的读写以错误返回。
4. `Stream.SetDeadline` 尽力而为:命名管道的同步句柄返回 `ErrUnsupported`,协议层靠 ctx + `Close` 取消。
5. 监听器的 `Addr()` 返回实际地址(端口 0 时是系统分配后的端口),用于测试与日志。

### 3.4 Unix socket 规则

- 目录必须存在且权限 ≤ 0700;不在 `NewListener` 里递归创建目录。
- 绑定后 `chmod 0600`(socket 文件);失败即关闭并删除。
- 已存在路径:默认拒绝(`EINVALID`,不自动删);`RemoveStale=true` 时,只有 `Lstat` 确认是 socket 且属主为当前用户才删除;符号链接/普通文件一律拒绝。
- 关闭监听器时删除 socket 文件;异常退出留下的文件由下次 `RemoveStale` 处理。
- Linux 可选用 `SO_PEERCRED`、macOS 用 `getpeereid` 校验对端 UID(P4 增强;P0–P3 依赖文件权限)。

### 3.5 Windows 命名管道规则

- 路径统一为 `\\.\pipe\<name>`;名字长度 ≤ 128。
- 第一个实例使用 `FILE_FLAG_FIRST_PIPE_INSTANCE`,防止同名管道劫持。
- 模式 `PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED`(首个实例再或上 `FILE_FLAG_FIRST_PIPE_INSTANCE`),类型 `PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS`。
- 安全描述符只授予**当前用户**与 `SYSTEM` 完全控制;默认拒绝远程客户端(即使 SMB 可达)。
- 每个连接一个管道实例;`Accept` 用 overlapped `ConnectNamedPipe`(每实例一个 EVENT),把句柄包成**自实现的 overlapped 流**:每次读/写各用独立 `OVERLAPPED` 与事件,完成后再 `GetOverlappedResult`;取消时 `CancelIoEx` + 关闭句柄,不留悬挂线程。
- **不要用同步句柄 + `os.File`**:同步句柄上挂起的读会让同一连接的写阻塞,而 RPC 客户端总是「读循环 + 调用写」并发,会直接死锁;`os.File` 的并发读写在 Windows 上还会争用内部偏移。P2 早期实现踩过这个坑,`TestRPCOverPipe` 是回归用例。
- 不使用 `go-winio`(未在依赖中);用已存在的 `golang.org/x/sys/windows` 自己实现 overlapped 流(见 §12 替代方案)。P2 已落地并用 `TestPipeEcho`/`TestPipeAcceptCancel`/`TestRPCOverPipe` 验证;连接建立后立即预建下一个实例,避免客户端看不到管道。

---

## 4. 帧与消息协议(Protocol v1)

### 4.1 帧

```
0        4        5
+--------+--------+------------------------+
| len u32| kind u8| body (len-1 bytes)     |
+--------+--------+------------------------+
```

- `len` 含 kind 与 body;`len ≥ 1`;上限 `maxFrameBytes`(默认 16 MiB,配置范围 4 KiB–64 MiB)。
- 超限/截断/未知 kind → 协议错误帧后关闭连接。**不做容错跳过**,避免把错位当合法数据。
- body 用 JSON(UTF-8)。解码**不启用** `DisallowUnknownFields`:版本演进允许新增字段;只有未知 kind/版本才是错误。

### 4.2 kind

| 值 | 名称 | 方向 | body |
|---|---|---|---|
| 0x01 | `challenge` | server → client | `{"v":1,"nonce":"<hex>","modes":["token"]}` |
| 0x02 | `auth` | client → server | `{"v":1,"client":"desktop","proof":"<hex>"}` |
| 0x03 | `ready` | server → client | `{"v":1,"peer":"desktop#1","features":["cancel","events","ping"]}` |
| 0x04 | `request` | client → server | `{"v":1,"id":"c-1","method":"...","params":{...},"noReply":false}` |
| 0x05 | `response` | server → client | `{"v":1,"id":"c-1","result":{...}}` 或 `{"v":1,"id":"c-1","error":{...}}` |
| 0x06 | `event` | server → client | `{"v":1,"topic":"...","seq":7,"data":{...}}` |
| 0x07 | `cancel` | client → server | `{"v":1,"id":"c-1","reason":"..."}` |
| 0x08 | `subscribe` | client → server | `{"v":1,"topics":["agent/*"]}` |
| 0x09 | `ping` | 双向 | `{"v":1,"ts":...}` |
| 0x0a | `pong` | 双向 | `{"v":1,"ts":...}` |
| 0x0b | `bye` | 双向 | `{"v":1,"code":"...","message":"..."}` |

请求必须发生在 `ready` 之后;握手阶段只接受 `auth`/`bye`。未知 kind → `bye` + `EINVALID` + 关闭。

### 4.3 握手(挑战应答)

```
client                                  server
  |  TCP/Unix/Pipe connect                 |
  |<--- challenge {v:1, nonce, modes} -----|
  |--- auth {v:1, client, proof} --------->|
  |<--- ready {v:1, peer, features} -------|
```

- `proof = hex(HMAC-SHA256(token, "rpc1:" + nonce + ":" + client))`;服务器用 `crypto/subtle.ConstantTimeCompare` 比较。
- `nonce` 每连接随机 32 字节,一次性;握手超时默认 5s;失败只允许一次,随后 `bye EAUTH` 并关闭。
- `auth:"os"` 时 `modes:["os"]`,客户端 `proof` 为空;服务器依赖文件权限/管道 ACL,不校验 proof。`auth=os` 配在 `tcp` 上属于配置错误(`EINVALID`),启动即失败,避免「以为有 ACL 其实没有」。
- `auth:"none"` 只在显式 `allowInsecure=true` 时可用(仅建议测试);provider 不写审计,生产配置应避免。
- 服务器校验客户端名:长度 ≤ 64、可打印 ASCII、匹配 `^[a-z0-9._-]+$`;不在 `peerAllowlist` 内 → 拒绝。
- token 不在日志/事件/错误文本里出现;错误只返回 `EAUTH` 与固定文案。

### 4.4 请求与响应

```json
{"v":1,"id":"c-1","method":"agent.session.list","params":{"limit":20},"noReply":false}
{"v":1,"id":"c-1","result":{"sessions":[]}}
{"v":1,"id":"c-1","error":{"code":"EDENIED","message":"缺少能力 rpc.call.agent.session.list","retryable":false}}
```

- `id` 由客户端生成(建议 UUID/随机),非空且 ≤128 字节;服务器对**活跃 id** 重复请求返回 `EINVALID`(避免串线)。
- `method` 长度 ≤ 128;只允许 `[a-z0-9._-]`;`params` 原始 JSON ≤ `maxParamsBytes`(默认 1 MiB)。
- 结果/错误大小同样受帧上限约束;超大结果由方法自己改用句柄/URL(P8)。
- 错误码复用 `seamerr`: `EINVALID` / `EAUTH` / `EDENIED` / `EUNAVAILABLE` / `ETIMEOUT` / `ECANCELED` / `ESCHEMA` / `EUNIMPLEMENTED`;**提案新增 `EINTERNAL`(服务器未预期错误)与 `ELIMIT`(超限)**,P0 实现时同步 `seamerr` 与 [seam-api.md](seam-api.md) §1.2。`EINTERNAL` 只返回固定文案,细节进日志(避免泄漏内部结构)。
- 未知方法 → `EUNIMPLEMENTED`;方法存在但未授权 → `EDENIED`;两者不合并,便于诊断。
- `noReply=true`:服务器仍执行,但不回 `response`;失败只进审计/日志。调用方无法感知失败,只用于幂等通知。

### 4.5 事件与订阅

```json
{"v":1,"topic":"agent/session/event","seq":7,"data":{"session":"s1","type":"assistant/message"}}
```

- 订阅后服务器才推送;`topics` 支持 `*` 通配(仅末段或整体,不做正则);单次订阅最多 64 个 topic,每个 ≤128 字节。
- `seq` 每 (peer, topic) 单调递增;客户端检测缺口 → 重新订阅/全量同步(协议不做重放缓冲)。
- 事件队列每连接上限 `maxEventQueue`(默认 1024 帧/4 MiB);溢出 → `bye ELIMIT` + 关闭(宁可断开重连,也不静默丢事件)。
- 事件**不是**审批/授权通道;审批仍走 `seam/approval`。

### 4.6 取消、心跳、背压

| 机制 | 规则 |
|---|---|
| 取消 | 客户端 ctx 取消 → 发 `cancel`;服务器取消 handler ctx;handler 应返回 `ECANCELED`。连接断开也取消所有在飞请求 |
| 心跳 | 空闲 30s 发 `ping`;超过 2 个心跳周期没有收到任意入站帧 → `fail` 并关闭 |
| 写背压 | 每连接单写者 + 128 帧有界队列;写超时 5s;队列满 → `ELIMIT` 并关闭(不再阻塞 Publish) |
| 读并发 | 单读循环;请求分发到有界 worker(默认 32/连接);满则直接回 `ELIMIT`,不排队 |
| 连接上限 | 默认 64;握手前原子预占，含握手中的连接;超过 → 立即关闭新连接(不静默排队) |

### 4.7 版本与兼容

- 帧不带版本,body 的 `v` 是协议版本;`v=1`。
- 收到未知 `v` → `bye` + `EINVALID`,关闭;不回退猜测。
- 新增 kind 属于破坏性扩展(旧端会关闭),必须升 `v`;新增 body 字段是兼容扩展。
- `features` 只做能力宣告,不做协商降级;缺失 feature 时调用方不得使用对应 kind。

---

## 5. 生命周期与内核接线

```go
func Plugin() kernel.Plugin // 提供 "rpc"(空 Chain)与 "rpc-transport"(Registry)
```

- `rpc-json.Plugin()`:启动时 `chain.Install(newService(ctx))`,在 `ctx.Effect` 内注册;**不**自动监听。
- `rpc-net.Plugin()` / `rpc-pipe.Plugin()`:启动时 `registry.Register(network, transport)`,返回 `Dispose` 反注册。
- 监听由宿主/消费者显式调用 `Server`/`Listen`,并 `go server.Serve(ctx)`;`ctx` 取消 → `Close` → 等待 goroutine 退出。
- 每个连接:1 个读循环 + 每个请求一个 goroutine(受 `maxInflight` 限制);写路径用 mutex 串行。所有 goroutine 由 `Server.Close` 收敛,禁止泄漏。
- `Handle` 返回 `kernel.Dispose`;fiber 卸载时方法消失,后续调用 `EUNIMPLEMENTED`。
- `rpc-guard` 在 `Chain.Decorate` 里包裹 `Service`,对 `Dial`/`Listen` 两个入口都设门禁;入站 handler 执行前用 `kernel.WithPrincipal(ctx, "rpc:"+peer)` 改写身份,使下游 caps 检查按远端身份而不是宿主身份通过。
- `Listen` 只绑定并返回 `Server`;真正的 accept/分发由 `Serve` 跑。`Stats()` 暴露 `{listeners, conns, inflight, framesIn, framesOut, eventsOut}`,供 `ada inspect`/desktop 视图使用(只读,不触发副作用)。

---

## 6. 安全

| 威胁 | 控制 |
|---|---|
| 同机其他用户连本地 socket/管道 | Unix 0600 + 目录 0700;命名管道 DACL 仅当前用户+SYSTEM;`pipe` 默认 `PIPE_REJECT_REMOTE_CLIENTS` |
| 远程网络连 TCP | 默认只监听 loopback;非 loopback 显式 `allowRemote` 且要求 TLS(P4),否则启动失败 |
| 未授权客户端 | `peerAllowlist` + `token` 挑战应答;失败一次即断开并审计 |
| 本地插件越权调用 | 出站 `rpc.call.<method>` caps;地址白名单;`rpc-guard` 装饰器 |
| 远端调用放大成本 | 每连接 `maxInflight`、方法超时、帧/参数上限、连接上限 |
| 远端触发高权限操作 | 入站 principal 改写为 `rpc:<peer>`;下游 caps 按该身份 fail-closed;宿主必须显式授予 |
| 方法枚举 | 不提供 discover;unknown → `EUNIMPLEMENTED`;错误文案不透露已注册方法表 |
| secret 泄漏 | token 只从 `seam/secrets` 按名取;不进 config/日志/事件/错误;`rpc/observe` 不含 params/result |
| 帧走私/错位 | 严格长度前缀 + 上限;未知 kind/版本关闭连接 |

拒绝路径:出站/入站都走 `caps.Broker` 或 `rpc-guard` 的确定性判断(非事件);拒绝写 `caps/deny`,由 `audit-log` 落盘。`rpc/observe` 只记录成功/失败,不阻断。

能力名(P3 同步 [caps.md](caps.md) §2/§3):

| 能力 | 覆盖目标 | 最低信任 | 说明 |
|---|---|---|---|
| `rpc.call` | `rpc.call.<method>` | third-party | 出站调用某个方法 |
| `rpc.listen` | `rpc.listen.<network>` | verified | 监听 tcp/unix/pipe;开一个入口比调一次更敏感 |

---

## 7. 配置与 preset

> 本节是 **P3 已实现形态**:`rpc-guard` 按 `listen`/`call` 白名单、`peers`、`methods`、`auth`/`tokenSecret`、`allowRemote` 设门;监听仍由宿主显式调用 `Service.Listen`。

```jsonc
{
  "plugins": [
    { "id": "seam-rpc" },
    { "id": "rpc-net" },
    { "id": "rpc-pipe" },          // 非 Windows 注册为空操作
    { "id": "rpc-json" },
    { "id": "rpc-guard", "config": {
        "peers": ["desktop", "cli"],
        "methods": ["agent.session.list", "agent.session.append", "app.inspect"],
        "topics":  ["agent/*"],
        "listen": ["unix:///run/ada/agent.sock", "pipe://ada-agent"],
        "call":    ["unix:///run/ada/orchestrator.sock"],
        "auth": "token",
        "tokenSecret": "ADA_RPC_TOKEN",
        "allowRemote": false,
        "allowInsecure": false
    }}
  ]
}
```

规则:

- **默认不监听**:`listen` 为空时 `rpc-json` 不创建任何 listener;没有「装了就开门」的默认。
- `tokenSecret` 是 `seam/secrets` 的键名,不是明文;`auth=token` 且 secret 缺失 → 首次 `Dial`/`Listen` 失败(`EAUTH`)。`rpc-guard` 解析后把字节交给 `rpc-json`,provider 不直接读 secret。
- `allowRemote=true` 才允许监听非 loopback;远程必须是 `tls://`(明文 TCP 拒绝),`allowInsecure=true` 只用于 `auth=none` 的测试场景。
- `listen`/`call` 的地址在 `rpc-guard` 里做白名单;`topics` 控制 `Subscribe` 可订阅的 topic(pattern 支持 `*` 与末段 `/*`);provider 本身不判断「该不该连」。
- preset 只影响「装什么 + 配什么」,协议行为不因产品名分叉。

---

## 8. 观测与诊断

`rpc/observe`(由 `policy/rpc-observe` 发出,观察不阻断):

| 字段 | 类型 | 说明 |
|---|---|---|
| `direction` | `in`/`out` | 入站/出站 |
| `transport` | `tcp`/`unix`/`pipe` | 传输 |
| `peer` | string | 对端标识(出站为服务端 peer 名,入站为客户端名) |
| `method` | string | 方法(事件为 topic) |
| `bytes` | int64 | 帧体字节数 |
| `durationMs` | int64 | 耗时 |
| `error` | string | 失败文本(已脱敏,无 token/params) |
| `plugin` | string | 本地调用者/服务端插件 id |

`ada inspect` 显示:监听地址、连接数、在飞请求、帧计数;拒绝仍看 `caps/deny` 审计。事件名与字段变更需同步本表。

---

## 9. 测试矩阵

| 场景 | 用例 |
|---|---|
| 帧编解码:合法/超限/截断/未知 kind | `rpcjson.TestFrame*` |
| 握手:token 正确/错误/超时/重复 auth/未握手发请求 | `rpcjson.TestHandshake*` |
| 调用:成功/方法不存在/参数非法/结果超限 | `rpcjson.TestCall*` |
| 取消:客户端取消 → handler ctx 取消 | `rpcjson.TestCancel` |
| 并发:100 并发调用、id 不串线 | `rpcjson.TestConcurrent` |
| 背压:worker 满返回 ELIMIT;写队列满关闭 | `rpcjson.TestBackpressure` |
| 连接上限:并发/握手中连接预占 | `rpcjson.TestMaxConnsReservation` |
| 守卫:Notify/Subscribe 不能绕过 | `rpcguard.TestDial` |
| 事件:订阅/seq 递增/队列溢出断开 | `rpcjson.TestEvents` |
| 传输:tcp loopback(端口 0)、unix 临时目录 | `rpcnet.TestDialListen*` |
| unix 权限/stale/符号链接拒绝 | `rpcnet.TestPerm`、`TestStale` |
| Windows 命名管道(仅 windows) | `rpcpipe.TestPipe*` |
| 非 Windows 上 pipe 返回 `EUNIMPLEMENTED` | `rpcpipe.TestUnsupported` |
| 鉴权:peer 白名单/token 缺失/allowInsecure | `rpcguard.TestAuthRequiresSecret`、`rpcguard.TestListen` |
| caps:出站 `rpc.call.<method>` 允许/拒绝;入站 principal | `rpcguard.TestDial`、`TestListen` |
| 拒绝矩阵:地址×方法×对端×鉴权 | `rpcguard.TestDial`、`TestListenDenied` |
| 内核集成:seam+provider+consumer+Dispose | `rpcjson.TestKernelIntegration` |
| 重载 100 次无泄漏 | `rpcjson.TestReload` |
| 交叉编译 linux/darwin/windows(CGO=0) | `guard.ps1` |
| 帧模糊测试 | `FuzzFrameDecode` |
| 观察事件:出站/入站/错误 | `rpcobserve.TestObserveOutbound`、`TestObserveInbound` |
| TLS 往返/未知 CA/协议集成 | `rpctls.TestTLSRoundTrip`、`TestTLSRejectsUnknownCA`、`TestTLSRPCIntegration` |

约束:测试不依赖真实外网、不 sleep 轮询、不写固定端口;Unix/pipe 用 `t.TempDir()` 与随机名字;Windows 测试用 `t.Skip` 保护无 AF_UNIX 的环境。

**已落地(P0–P4 TLS)的用例**:

| 包 | 用例 |
|---|---|
| `seam/rpc` | `TestParse`、`TestChainOrder`、`TestRegistry` |
| `rpc-json` | `TestCallAndNotify`、`TestUnknownMethod`、`TestBadTokenAndPeer`、`TestCancelPropagates`、`TestEvents`、`TestConcurrentCalls`、`TestFrameLimits`、`TestKernelIntegration` |
| `rpc-net` | `TestTCP`、`TestUnix`、`TestUnixStale`、`TestAcceptCancel`、`TestUnsupported`、`TestKernelStack` |
| `rpc-pipe` | `TestPipeEcho`、`TestPipeAcceptCancel`、`TestRPCOverPipe`(Windows);`TestPipeUnsupported`(非 Windows) |
| `rpc-guard` | `TestDial`、`TestListen`、`TestListenDenied`、`TestAuthRequiresSecret` |
| `rpc-observe` | `TestObserveOutbound`、`TestObserveInbound` |
| `rpc-tls` | `TestTLSRoundTrip`、`TestTLSRejectsUnknownCA`、`TestTLSRPCIntegration` |

---

## 10. 兼容与演进

1. 协议版本 `v=1` 冻结后,新增字段兼容,新增 kind 破坏性(升 `v`)。
2. 不使用 protobuf/IDL;方法表由宿主注册,协议不内建 schema。
3. `pipe` 只在 Windows 构建注册;其它平台的 `pipe://` 地址返回 `EUNIMPLEMENTED` 而不是静默失败。
4. 未来若需 TLS:新增 `tls` scheme 或 `DialOptions.TLS`,不改帧格式。
5. 若需流式大结果:新增 `chunk` kind + `more` 标记,必须升协议版本;仍禁止把 > 256 KiB 塞进单帧。

---

## 11. 里程碑

| 阶段 | 交付 | 验收 |
|---|---|---|
| **P0** | `seam/rpc` 类型 + `Registry`/`Chain`;内存传输;`rpc-json` 帧/握手/调用/事件/取消;契约测试 | ✅ `go test -race ./plugins/seam/rpc ./plugins/provider/rpc-json` |
| **P1** | `rpc-net`(tcp loopback + unix;Windows AF_UNIX);权限/stale/Addr;集成测试 | ✅ Linux/macOS/Windows 全绿;`unix` 权限测试在支持平台通过 |
| **P2** | `rpc-pipe`(Windows 命名管道 + ACL + 拒绝远程客户端);非 Windows 空实现 | ✅ Windows CI/本机 `go test ./plugins/provider/rpc-pipe`;`GOOS=linux` 构建通过 |
| **P3** | `rpc-guard` + `rpc-observe`;caps 语法 `rpc.call`/`rpc.listen`;audit 接入;principal 改写 | ✅ `go test -race ./plugins/policy/rpc-guard ./plugins/policy/rpc-observe`;caps 拒绝接 `caps/deny` |
| **P4** | TLS/远程 TCP、`SO_PEERCRED`、宿主接入(`ada serve --rpc` 或 desktop Transport 可选) | ⚠️ TLS + 远程 TCP 已落地(`rpc-tls`);`SO_PEERCRED` 待做;宿主接入原型 `cmd/ada-rpc-agent` 已落地(每机器人一个进程、workspace 为工作目录、`bot.info`/`chat.send`,并按运行时地址/客户端名挂 `rpc-guard` + `rpc-observe`;见该目录 README) |

预算(超了先拆或写 ADR):seam ≤ 400 行;`rpc-json` ≤ 1200(ADR 0009);`rpc-net` ≤ 400;`rpc-pipe` ≤ 500(仅 windows 文件);`rpc-tls` ≤ 300;`rpc-guard` ≤ 400;`rpc-observe` ≤ 200。

---

## 12. 替代方案与否决理由

| 方案 | 否决理由 |
|---|---|
| gRPC / protobuf | 引入 protobuf 工具链与依赖;HTTP/2 栈过重;方法 schema 与现有插件模型重复;CGO/跨平台成本高 |
| 标准库 `net/rpc`(gob) | gob 跨语言不可用;无显式帧上限/版本/取消/心跳;错误码无法映射 seamerr |
| WebSocket | 需要 HTTP Upgrade 与握手;与 host/web 的 HTTP/SSE 重叠;本地 IPC 上没有收益 |
| 裸 JSON 行(NDJSON) | 内容里的换行/大对象会破坏边界;无法区分控制帧 |
| 只用 Unix socket | Windows 无原生等价物;跨机不可用 |
| 只用 TCP | 本地多用户环境下端口暴露面大;令牌明文风险 |
| `go-winio` | 未在依赖中;引入需 ADR 与新版依赖。当前用 `x/sys/windows` 的 `FILE_FLAG_OVERLAPPED` + 每操作 EVENT 自行实现流,足以覆盖并发读写与取消;若后续遇到超时/关闭缺陷再评估 ADR |
| 把 RPC 放进 `platform/`(L2) | RPC 是可替换能力,不是装载/编译机制;产品可用 HTTP/SSE 替代 |

---

## 13. 偏差与不做

### 13.1 已实现子集(P0–P4 TLS)的偏差

1. **授权已由 `rpc-guard` 提供**:出站地址/`Call`+`Notify` 方法/`rpc.call.<method>`/可选 `topics`,入站 `rpc.listen.<network>`/peer 白名单/`tokenSecret`;未装 guard 时仍由宿主自行控制监听地址。
2. **写路径已改为每连接独立写协程 + 128 帧有界队列**;队列满/写超时按 `ELIMIT`/关闭处理。队列上限暂未做成按字节配置。
3. **心跳已做存活回收**:客户端超过 2 个心跳周期没有收到任意入站帧就关闭;服务端仍主要依赖连接关闭/TCP 错误。
4. **明文 TCP 仍不允许远程**:`rpc-net` 只做明文 tcp/unix;远程必须用 `rpc-tls` 的 `tls://`,`rpc-guard` 默认拒绝非 loopback 明文 TCP。
5. **`noReply` 失败不可观测**:通知执行失败只丢在服务端,调用方无从知道;只适合幂等通知。
6. **`allowInsecure` 与 `auth=none` 是测试通道**:生产配置不应使用,且当前不写审计;`auth=os` 在 unix 上只依赖 0600 权限,不校验对端 UID(`SO_PEERCRED` 在 P4)。
7. **`rpc-json` 约 1220 行**,超过单插件 800 行预算,例外见 [ADR 0009](adr/0009-rpc-json-budget.md)(2026-11-26 修订上限 1300);拆包方向不变。

### 13.2 不做(本提案明确)

1. **不做认证之外的授权发现**:没有方法枚举、没有 capability 协商;方法表只由宿主知道。
2. **不做自动重连/重试/幂等缓存**:协议 at-most-once;重连与重放由宿主负责。
3. **不做事件重放**:只检测 gap,由客户端重新同步。
4. **不做压缩/分片**:大对象走句柄/URL;单帧硬上限。
5. **不把 RPC 当插件装载通道**:插件安装仍走 `install` 与签名。
6. **不替代 host/web 的 HTTP/SSE**:两条通道各自服务不同场景。
7. **远程只支持 TLS**:明文 TCP 只允许 loopback;非 loopback 由 `rpc-guard` 拒绝,必须改用 `rpc-tls`。

---

## 14. 验证命令(实现后)

```powershell
go test -race -count=1 ./plugins/seam/rpc ./plugins/provider/rpc-json ./plugins/provider/rpc-net ./plugins/provider/rpc-pipe
$env:CGO_ENABLED='0'; $env:GOOS='linux'; go build ./...
go test -race -count=1 ./...
pwsh scripts/guard.ps1
pwsh scripts/bench.ps1
```
