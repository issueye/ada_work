# 控制层五层架构(ACP)

> 状态：**第 2 层控制契约 + Go/TS 实现、第 3 层 ACP 适配、第 4 层 jsonrpc2、第 5 层 stdio、宿主入口 `ada acp` 已落地**；ACP 的 permission、fs/terminal 反向能力与 `acp-helper` 已接，MCP 与会话级路由待做。
> RPC 侧已整体移除：`control-rpc` 适配器、`cmd/ada-rpc-agent` 宿主与整个 `seam/rpc` / `rpc-*` 通讯栈（ADR 0018）；ACP 是唯一控制面。
> 关联：[seam-rpc.md](seam-rpc.md)（已移除说明）、[seam-acp.md](seam-acp.md)、[AGENTS §2/§3](../AGENTS.md)。

## 0. 结论

采用五层，把 ACP 作为唯一协议适配面：

```
ada_go 能力（agent-loop / session / tools / fs / shell）
        ↑
控制层（seam/control：session / prompt / cancel / permission / update）
        ↑
协议适配层（control-acp）
        ↑
消息通道/编码（jsonrpc2）
        ↑
传输（stdio）
```

方向是“能力 ← 控制 ← 协议 ← 编码 ← 传输”。依赖只能向上：控制层不认 frame/stdio，
ACP 适配器不直接调 `agent-loop` 或具体 `fs` provider。

## 1. 各层职责

| 层 | 职责 | 不负责 |
|---|---|---|
| 能力层 | `Agent.Run`、`seam/session`、`seam/tools`、`seam/fs`、`seam/shell` | 协议、连接、鉴权 |
| 控制层 | 会话创建/加载、prompt、取消、权限请求、流式更新 | 帧、握手、stdio、JSON-RPC 字段 |
| 协议适配层 | 把 ACP 方法翻译成控制调用 | 具体 agent 逻辑、具体编码 |
| 消息通道/编码 | JSON-RPC 2.0 消息、id/取消/通知 | agent 语义 |
| 传输层 | 字节流与监听/连接 | 消息格式 |

## 2. 控制层：Go 与 TS 两种实现

控制层的 kernel 服务值是 **`control.Invoker`**，不是只能由 Go 实现的强类型接口：

```go
type Invoker interface {
    Invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
}
```

### Go 实现

- 实现 `control.Invoker`，通过 `ctx.Provide("control", impl)` 注册；
- 可以直接调用 `agent-loop` / `session` / `tools` 的 seam；
- 协议适配器通过 `control.New(invoker)` 得到类型化 `Client`。

### TS 插件实现

- 插件在 `apply` 期间：

```ts
ctx.provide("control", {
  async invoke(method, params) {
    switch (method) {
      case "session.create": return { id: "s1", title: params.title }
      case "session.prompt": return { id: "t1", text: "..." }
      default: throw new Error("unsupported: " + method)
    }
  }
})
```

- `platform/bridge` 检测服务名 `control`，校验它暴露 `invoke`，然后包装成
  `control.Invoker` 再提交给 kernel；
- **kernel 与 `ctx.provide` 不变**，其它服务不受影响；
- TS 的 `invoke` 可以返回 Promise（bridge 的 JS 调用路径会等待结算）。

### 同一份契约

Go 与 TS 必须实现同一组方法：

| 方法 | 入参 | 返回 |
|---|---|---|
| `session.create` | `CreateRequest` | `SessionInfo` |
| `session.load` | `{sessionId, cwd?}` | `SessionInfo` |
| `session.list` | `{}` | `SessionInfo[]` |
| `session.prompt` | `PromptRequest` | `TurnResult` |
| `session.history` | `{sessionId}` | `Update[]` |
| `session.cancel` | `{sessionId}` | `{}` |
| `session.permission` | `PermissionRequest` | `PermissionDecision` |
| `session.close` | `{sessionId}` | `{}` |

流式片段不放在 `session.prompt` 的返回值里，而是发 `control/update` 事件；
ACP 协议适配器把它映射成 `session/update`，RPC 适配器映射成事件订阅。
`core/agent-control` 在 `Run` 执行期间就转发事件，因此工具调用/中间消息能在
prompt 未返回时到达对端（不是结束后补发）。

## 3. 已落地与未落地

| 包 | 层 | 状态 |
|---|---|---|
| `plugins/seam/control` | 2 | ✅ 契约、`Client`、`Invoker`、Go/TS 契约测试 |
| `platform/bridge`（control 包装） | 2 适配 | ✅ TS `ctx.provide("control")` 自动包装 |
| `core/agent-control` | 2 实现 | ✅ `agent.runner` → `control.Invoker`；create/load/list/prompt/history/cancel/permission/close；token 级 `assistant/delta`；取消返回 `cancelled`；`load` 可按 id 恢复；同一会话单轮串行 |
| `core/plan` | 2 实现 | ✅ 会话级计划：`plan_set` / `plan_update` / `plan_show`；计划写成 `plugin:plan/updated` session 事件；ACP 映射成 `plan` |
| `seam/agent` | 能力契约 | ✅ `Runner`（Open/Run） |
| `provider/jsonrpc2` | 4 | ✅ 双向 JSON-RPC 2.0 连接、id/错误/通知、行上限、`null` 结果；连接关闭会取消 handler ctx |
| `provider/control-acp` | 3 | ✅ initialize/session/new/load/prompt/cancel；`session/load` 回放历史；`session/update` 实时转发（assistant 文本/推理 + tool_call/tool_call_update）；`session/request_permission` + `approval.Approver`；反向 fs/terminal 能力 |
| `seam/acp` | 2 契约 | ✅ client 能力契约 + `Registry`（单连接绑定） |
| `platform/host/cli`（`ada acp`） | 宿主入口 | ✅ 装配 preset → control 服务 → stdio 上的 ACP；日志走 stderr |

## 4. 为什么 TS 实现不破坏插件系统

`ctx.provide` 的语义不变：

- TS 仍然提供普通对象；
- `bridge` 只在提交 `control` 服务时做类型包装；
- 其它服务（`tools`、`fs`、`llm` 等）仍按原逻辑提供；
- 没有新增 kernel 类型，也没有要求 TS 实现 Go 接口；
- 卸载时随 fiber 一起回收，和普通服务一致。

代价是 `bridge` 对 `"control"` 有一个显式分支；这是协议适配层与语言适配层的边界，
不是业务特判。

## 5. Go 实现边界

`core/agent-control` 目前是同步实现：`session.prompt` 阻塞到一轮结束并返回最终结果；
但它会在 `Run` 执行期间并发转发本轮新增的 session 事件（`control/update`），
所以工具调用与中间消息能在 prompt 未返回时到达适配器。

token 级流式：`agent-loop` 在每次 `llm/stream` 回调后发 `agent/delta`
（`{sessionId,turnId,stepId,kind,text}`，`kind=text|reasoning`），`agent-control` 在
prompt 期间订阅并投影成 `control/update` 的 `assistant/delta` 与
`assistant/reasoning`。只有正文增量会抑制该 step 的 `assistant/message`，推理增量不会；
没有增量的 provider 仍发整段消息。`agent/delta` 不写进 session，也不参与 `render`。

ACP 适配器从会话事件里提取文本，而不是把整段 JSON 当文本：
`assistant/message` 的 `{"message":{"content":[{"kind":"text","text":...}]}}`
会被展开成 `agent_message_chunk.content.text`，同时兼容控制层直接发
`{"text":...}` 的简化形态。

工具事件也映射成 ACP 的对应形状（而不是 `data` 透传）：

| 会话事件 | ACP `session/update` | 关键字段 |
|---|---|---|
| `tool/call` | `tool_call` | `toolCallId`、`title`、`kind`（read/edit/execute/…）、`status=in_progress`、`rawInput` |
| `tool/result` | `tool_call_update` | `toolCallId`、`status=completed/failed`、`content`（文本块）、`rawOutput` |

纯工具调用的 assistant 消息（没有文本）不产生空 `agent_message_chunk`；
未知工具状态按 `failed` 处理（fail-closed）。

## 6. 协议适配方向

### control-acp

只做 ACP 方法翻译：

```
initialize          -> capabilities
session/new         -> Client.Create(cwd 经 workspace.Locator 映射为工作区 ID)
session/load        -> Client.Load(cwd 可选,重新绑定工作区) + Client.History,回放 session/update 后返回 null
session/prompt      -> Client.Prompt
session/cancel      -> Client.Cancel
session/update      <- control/update 事件
  assistant/delta   -> agent_message_chunk
  assistant/reasoning -> agent_thought_chunk
  assistant/message -> agent_message_chunk
  tool/call         -> tool_call
  tool/result       -> tool_call_update
  plan/updated      -> plan
```

`session/cancel` 取消当前轮后，`session/prompt` 返回 `stopReason: "cancelled"`
（不是 JSON-RPC 错误）；其它失败才是错误。

`initialize` 只协商 ACP 协议版本 1：客户端传其它非零版本返回 `-32602`，
空版本（未声明）按 1 处理。

端点关闭（连接断开或 `ada acp` 退出）时会回收本连接通过 `session/new` /
`session/load` 登记的会话，调用控制层 `session.close`，避免长进程反复建立
连接后残留会话。

`session/update` 的顺序有保证：端点把所有更新放进同一条队列，`session/prompt`
在返回前插入 flush 屏障，等本轮已入队的更新全部写出后才写响应。慢客户端或
积压时更新会被丢弃（`Endpoint.Stats` 暴露 dropped/failed 计数）；单条消息超过
`jsonrpc2` 行宽只会计入 failed，不再终止后续事件流。

映射失败（畸形 `tool/call`、未知事件 kind、无法识别形状的 assistant 消息）
一律跳过，不发明非法 `sessionUpdate`，也不把内部 JSON 当文本透给客户端。

`session/new` 的 `cwd` 会经 `workspace.Locator`（可选扩展）映射到已配置工作区；
有 workspace provider 但 cwd 不属于任何工作区根时拒绝创建，避免会话悄悄编辑
另一个目录。没有 workspace 服务时保持 provider 默认根。`mcpServers` 非空时
显式返回 `EUNIMPLEMENTED`，不做静默忽略。

`session/prompt` 的内容块只接受 `text` 与 `resource_link`：后者渲染成
`[file] 名称 <uri> (mime)`，由 agent 用工具读取；`image`/`audio`/
`embeddedContext` 按 `initialize` 声明的能力拒绝，不做静默丢弃。控制层的
`EINVALID` 映射成 JSON-RPC `-32602`，其余错误映射成 `-32603`。

同一会话同时只允许一轮 prompt：第二个并发 `session/prompt` 返回 `EINVALID`，
避免事件、取消与历史互相踩踏。

`session/load` 的响应是 `null`：先按 `session.history` 回放 `session/update`，再返回。
回放把 `user/message` 映射成 `user_message_chunk` 并跳过 `turn/*`、`step/*` 等
控制面事件；实时转发不回显用户消息。恢复能力依赖 `session.Resumer`：
`session-jsonl` 可跨重启恢复，`session-memory` 只能恢复同进程仍打开的会话。

`session/load` 带 `cwd` 时，控制层用同一个 `workspace.Locator` 重新解析工作区，
覆盖本进程内该会话的 `Meta.Workspace`（底层 provider 的持久化 meta 不改）；
后续 prompt 立即按新工作区绑定。有 workspace provider 时，cwd 不属于任何
已配置工作区返回 `EINVALID`；没有 workspace provider 时保持会话原有 meta。
会话正在运行 prompt 时拒绝切换，避免运行中的工具与工作区不一致。

权限是反方向的：agent 内的 `approval.Ask` 调用 ACP client：

```go
ep := controlacp.New(conn, control.New(invoker))
dispose, err := ep.BindApproval(ctx)   // 注册 approval.Approver("acp-client")
// 其它插件按名选择: control.PermissionRequest{Backend: controlacp.ApproverName}
```

`approval.Approver.Approve` 发 `session/request_permission`（服务端 → 客户端请求），
把返回的 `outcome.selected` 映射成 allow/reject；取消、未知选项、连接错误都按拒绝处理
（fail-closed），超时由 `approval.Ask` 的 30s 默认值兜底。审批请求的 `sessionId`
优先取 `Request.Details`，缺失时回退到调用 ctx 里的 `session.FromContext`，
因此 fs/shell/gate 不需要各自记得填 Details；`approval-gate` 会额外带上
`toolCallId`，让客户端的权限弹窗能对应到具体工具调用。

ACP 的文件/终端回调是另一组反向调用，由 [seam-acp.md](seam-acp.md) 承载：

```
fs/read_text_file   -> Client.ReadTextFile
fs/write_text_file  -> Client.WriteTextFile
terminal/create     -> Client.CreateTerminal
terminal/output     -> Client.TerminalOutput
terminal/wait_for_exit -> Client.WaitForExit
terminal/kill       -> Client.KillTerminal
terminal/release    -> Client.ReleaseTerminal
```

能力位来自 `initialize` 的 `clientCapabilities`；未声明时返回 `EUNIMPLEMENTED`
且不向 client 发请求。路径是客户端绝对路径，因此**不接入** `seam/fs` 链，
由需要的工具显式 `inject "acp"` 消费。`ada acp` 在连接建立后把端点绑定到
`acp` 服务，退出时解绑。

### 宿主入口：ada acp

```powershell
ada acp --preset examples/acp/acp.preset.json
```

`platform/host/cli` 的 `cmdACP` 只做宿主该做的事：装配 preset、取 `control` 服务、
用 stdin/stdout 构造 `jsonrpc2` 连接、交给 `control-acp`。stdout 只写协议消息，
日志与装配警告一律走 stderr（否则会污染 JSON-RPC 行）。

示例 preset 在 `examples/acp/`：llm-replay + session-memory + agent-loop +
agent-control + tool-helper，不依赖网络与磁盘会话；回放里带一次 `tool_find` 调用，
端到端测试同时验证 `agent_message_chunk`、`tool_call`、`tool_call_update` 三种 update。
它要求 preset 显式启用 `agent-control`——**CLI 不会偷偷加插件**，产品差异仍然只来自 preset。

## 7. 迁移顺序

1. 控制契约 + Go/TS 实现通路（已完成；Go 由 `core/agent-control`，TS 由 bridge 包装）；
2. `provider/jsonrpc2`（已完成）；
3. `provider/control-acp` 最小聊天闭环（已完成）；
4. 权限（已完成）：`session/request_permission` ↔ `approval.Approver`；
5. 宿主入口（已完成）：`ada acp` + `examples/acp`；
6. 会话恢复（已完成）：`session.history` + `session.Resumer` + `session/load` 回放；
7. 文件/终端反向能力（已完成契约、适配与 `acp-helper` 工具）；MCP 与会话级路由待做。
   RPC 侧的 `control-rpc` 适配器与 `cmd/ada-rpc-agent` 宿主已移除（ada_chat 改用 ACP）。

## 8. 验证

```powershell
go test ./plugins/seam/control ./plugins/provider/control-acp
go test -race ./plugins/seam/control ./plugins/provider/control-acp ./plugins/core/agent-control
go test -run TestACPServe -v ./platform/host/cli
```

Go 与 TS 的契约测试应共享同一组 JSON fixture；任何新增方法必须同时更新
[seam-acp.md](seam-acp.md) 与本文。
