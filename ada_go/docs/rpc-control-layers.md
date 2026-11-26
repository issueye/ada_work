# RPC/ACP 五层控制架构

> 状态：**第 2 层控制契约 + Go/TS 实现、第 3 层 control-rpc/ACP 适配、第 4 层 jsonrpc2、第 5 层 stdio、宿主入口 `ada acp` 已落地**；ACP 的 permission 已接，文件/终端回调待做。
> 关联：[seam-rpc.md](seam-rpc.md)、[AGENTS §2/§3](../AGENTS.md)。

## 0. 结论

采用五层，而不是把 ACP 塞进现有 RPC：

```
ada_go 能力（agent-loop / session / tools / fs / shell）
        ↑
控制层（seam/control：session / prompt / cancel / permission / update）
        ↑
协议适配层（control-rpc-v1 | control-acp）
        ↑
消息通道/编码（rpc-v1-frame | jsonrpc2）
        ↑
传输（tcp | unix | pipe | tls | stdio）
```

方向是“能力 ← 控制 ← 协议 ← 编码 ← 传输”。依赖只能向上：控制层不认 frame/stdio，
ACP 适配器不直接调 `agent-loop` 或具体 `fs` provider。

## 1. 各层职责

| 层 | 职责 | 不负责 |
|---|---|---|
| 能力层 | `Agent.Run`、`seam/session`、`seam/tools`、`seam/fs`、`seam/shell` | 协议、连接、鉴权 |
| 控制层 | 会话创建/加载、prompt、取消、权限请求、流式更新 | 帧、握手、stdio、JSON-RPC 字段 |
| 协议适配层 | 把 RPC 方法或 ACP 方法翻译成控制调用 | 具体 agent 逻辑、具体编码 |
| 消息通道/编码 | 长度前缀帧或 JSON-RPC 2.0 消息、id/取消/通知 | agent 语义 |
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
| `session.load` | `{sessionId}` | `SessionInfo` |
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
| `core/agent-control` | 2 实现 | ✅ `agent.runner` → `control.Invoker`；create/load/list/prompt/history/cancel/permission/close；token 级 `assistant/delta`；取消返回 `cancelled`；`load` 可按 id 恢复 |
| `seam/agent` | 能力契约 | ✅ `Runner`（Open/Run） |
| `provider/rpc-json` | 3+4 | ◐ 现有 v1 协议；尚未拆成 control 适配 + frame 通道 |
| `provider/control-rpc` | 3 | ✅ v1 方法 ↔ `control.Invoker`（Client + Register）；已用真实 rpc-json + tcp 跑通端到端 |
| `provider/jsonrpc2` | 4 | ✅ 双向 JSON-RPC 2.0 连接、id/错误/通知、行上限、`null` 结果；连接关闭会取消 handler ctx |
| `provider/control-acp` | 3 | ✅ initialize/session/new/load/prompt/cancel；`session/load` 回放历史；`session/update` 实时转发（assistant 文本/推理 + tool_call/tool_call_update）；`session/request_permission` + `approval.Approver`；反向 fs/terminal 能力 |
| `seam/acp` | 2 契约 | ✅ client 能力契约 + `Registry`（单连接绑定） |
| `platform/host/cli`（`ada acp`） | 宿主入口 | ✅ 装配 preset → control 服务 → stdio 上的 ACP；日志走 stderr |
| `provider/rpc-stdio` | 5 | ✅ stdio 单连接 Transport + Listener |

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

### control-rpc-v1

把现有方法映射到 `control.Client`：

```
control.session.create  -> Client.Create
control.session.prompt  -> Client.Prompt
control.session.cancel  -> Client.Cancel
control.event           -> EventUpdate
```

现有 `agent.session.*` 消费者通过这一层过渡；wire 格式不变。

### control-acp

只做 ACP 方法翻译：

```
initialize          -> capabilities
session/new         -> Client.Create
session/load        -> Client.Load + Client.History,回放 session/update 后返回 null
session/prompt      -> Client.Prompt
session/cancel      -> Client.Cancel
session/update      <- control/update 事件
  assistant/delta   -> agent_message_chunk
  assistant/reasoning -> agent_thought_chunk
  assistant/message -> agent_message_chunk
  tool/call         -> tool_call
  tool/result       -> tool_call_update
```

`session/cancel` 取消当前轮后，`session/prompt` 返回 `stopReason: "cancelled"`
（不是 JSON-RPC 错误）；其它失败才是错误。

`session/load` 的响应是 `null`：先按 `session.history` 回放 `session/update`，再返回。
回放把 `user/message` 映射成 `user_message_chunk` 并跳过 `turn/*`、`step/*` 等
控制面事件；实时转发不回显用户消息。恢复能力依赖 `session.Resumer`：
`session-jsonl` 可跨重启恢复，`session-memory` 只能恢复同进程仍打开的会话。

权限是反方向的：agent 内的 `approval.Ask` 调用 ACP client：

```go
ep := controlacp.New(conn, control.New(invoker))
dispose, err := ep.BindApproval(ctx)   // 注册 approval.Approver("acp-client")
// 其它插件按名选择: control.PermissionRequest{Backend: controlacp.ApproverName}
```

`approval.Approver.Approve` 发 `session/request_permission`（服务端 → 客户端请求），
把返回的 `outcome.selected` 映射成 allow/reject；取消、未知选项、连接错误都按拒绝处理
（fail-closed），超时由 `approval.Ask` 的 30s 默认值兜底。

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
2. `provider/jsonrpc2` + `provider/rpc-stdio`（已完成）；
3. `provider/control-rpc`，把现有 v1 消费者切到控制层（适配器 + 真实 rpc-json/tcp 端到端已完成，消费者迁移待做）；
4. `provider/control-acp` 最小聊天闭环（已完成）；
5. 权限（已完成）：`session/request_permission` ↔ `approval.Approver`；
6. 宿主入口（已完成）：`ada acp` + `examples/acp`；
7. 会话恢复（已完成）：`session.history` + `session.Resumer` + `session/load` 回放；
8. 文件/终端反向能力（已完成契约与适配，接入工具待做）；MCP 与会话级路由待做。

## 8. 验证

```powershell
go test ./plugins/seam/control ./plugins/provider/control-rpc ./plugins/provider/control-acp
go test -race ./plugins/seam/control ./plugins/provider/control-rpc ./plugins/provider/control-acp ./plugins/core/agent-control
go test -run TestControlOverRPCJSON -v ./plugins/provider/control-rpc
go test -run TestACPServe -v ./platform/host/cli
```

Go 与 TS 的契约测试应共享同一组 JSON fixture；任何新增方法必须同时更新
[seam-rpc.md](seam-rpc.md) 与本文。
