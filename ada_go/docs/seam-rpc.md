# seam/rpc 与 RPC 通讯栈（已移除）

> 状态:**已移除(2026-11-27,ADR 0018)**。本页只保留历史入口与替代路径;协议细节见 git 历史。
> 原内容:`seam/rpc` 契约 + `provider/rpc-json`(帧/握手/调用/事件/取消)+ `rpc-net`/`rpc-pipe`/`rpc-stdio`/`rpc-tls` 传输 + `policy/rpc-guard`/`rpc-observe` 门禁与观察。

## 1. 为什么移除

- 宿主侧已改用 **ACP**:ada_chat 删掉了 `internal/rpc` 客户端与 `chat.send`/`chat.approve` 事件面,
  ada_go 删掉了 RPC 宿主入口 `cmd/ada-rpc-agent` 与控制面适配 `provider/control-rpc`。
- 删除后核查依赖:整个 RPC 栈只剩 `platform/host/builtins` 的注册表与自身测试引用;
  **没有任何 preset 启用 `rpc-*`,没有任何产品路径调用**。
- 保留成本真实:两套控制面长期并行、452 行协议文档、`rpc-json` 的预算例外(ADR 0009)与
  7 个内置插件;一个没有消费者的 seam 也会误导后来者沿用旧协议。

决策与备选方案见 [ADR 0018](adr/0018-remove-rpc-stack.md)。

## 2. 现在用什么

| 需求 | 现役实现 |
|---|---|
| 桌面宿主 ↔ agent 进程 | **ACP**:stdio 上的 JSON-RPC 2.0([rpc-control-layers.md](rpc-control-layers.md)、[seam-acp.md](seam-acp.md)) |
| 消息通道/编码 | `plugins/provider/jsonrpc2`(ACP 专用,保留) |
| 只读 HTTP 宿主 | `platform/host/server`、`platform/host/registry`(HTTP,与本页无关) |
| 进程内调用 | 内核服务 + 事件;不需要跨进程协议 |

需要新的跨进程通讯端口时:**先写 seam 契约与 ADR**,说明与 ACP 的边界,不得复活旧的
`rpc-json` 帧格式(见 ADR 0018 §6)。

## 3. 历史参考

- 协议与实现细节、P0–P4 验收矩阵、门禁配置示例:见删除前版本(git 历史,提交 `400acfc` 之前)。
- 相关 ADR:[ADR 0009](adr/0009-rpc-json-budget.md)(预算例外,已被 ADR 0018 撤销);
  [ADR 0012](adr/0012-remove-seam-net.md)(更早的网络 seam 移除,同类先例)。
