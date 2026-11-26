# RPC/ACP 五层控制架构

> 状态：**第 2 层契约 + Go/TS 实现通路已落地**；第 4/5 层与 ACP 适配待接。
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
| `session.cancel` | `{sessionId}` | `{}` |
| `session.permission` | `PermissionRequest` | `PermissionDecision` |
| `session.close` | `{}` | `{}` |

流式片段不放在 `session.prompt` 的返回值里，而是发 `control/update` 事件；
ACP 协议适配器把它映射成 `session/update`，RPC 适配器映射成事件订阅。

## 3. 已落地与未落地

| 包 | 层 | 状态 |
|---|---|---|
| `plugins/seam/control` | 2 | ✅ 契约、`Client`、`Invoker`、Go/TS 契约测试 |
| `platform/bridge`（control 包装） | 2 适配 | ✅ TS `ctx.provide("control")` 自动包装 |
| `core/agent-control` | 2 实现 | ✅ `agent.runner` → `control.Invoker`；create/load/list/prompt/cancel/permission/close |
| `seam/agent` | 能力契约 | ✅ `Runner`（Open/Run） |
| `provider/rpc-json` | 3+4 | ◐ 现有 v1 协议；尚未拆成 control 适配 + frame 通道 |
| `provider/control-rpc-v1` | 3 | ⏳ 待建：v1 方法 → `control.Client` |
| `provider/jsonrpc2` | 4 | ⏳ 待建：JSON-RPC 2.0 消息通道 |
| `provider/control-acp` | 3 | ⏳ 待建：ACP 方法 → `control.Client` |
| `provider/rpc-stdio` | 5 | ⏳ 待建 |

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
它会在 `Run` 返回后把本轮新增的 session 事件投影成 `control/update`，
形状已通，但不是逐 token 实时。ACP 的流式 `session/update` 可先消费这些事件，
后续再改成边跑边发。

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
session/load        -> Client.Load
session/prompt      -> Client.Prompt
session/cancel      -> Client.Cancel
session/update      <- control/update 事件
session/request_permission -> Client.RequestPermission
```

ACP 的文件/终端回调应在控制层实现“按会话路由”，不能直接复用全局 `seam/fs`/`seam/shell`
链；这是一期最大的设计风险。

## 7. 迁移顺序

1. 控制契约 + Go/TS 实现通路（已完成；Go 由 `core/agent-control`，TS 由 bridge 包装）；
2. `provider/jsonrpc2` + `provider/rpc-stdio`；
3. `provider/control-rpc-v1`，把现有 v1 消费者切到控制层；
4. `provider/control-acp` 的最小聊天闭环；
5. 再补权限、文件、终端、MCP 与会话级路由。

## 8. 验证

```powershell
go test ./plugins/seam/control ./platform/bridge
go test -race ./plugins/seam/control ./platform/bridge
```

Go 与 TS 的契约测试应共享同一组 JSON fixture；任何新增方法必须同时更新
[seam-rpc.md](seam-rpc.md) 与本文。
