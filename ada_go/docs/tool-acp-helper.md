# tool-acp-helper — ACP client 的文件与终端工具

> 契约：[seam-acp.md](seam-acp.md)；通道：[rpc-control-layers.md](rpc-control-layers.md)。
> 插件 id `acp-helper`，Go 实现，`inject: tools + acp`。

## 1. 它解决什么

ACP client 可以在 `initialize` 里声明 `fs/read_text_file`、`fs/write_text_file` 与
`terminal/*`。`acp-helper` 把这些能力暴露成模型工具，让 agent 在需要时使用
**编辑器自己的文件视图/终端**，而不是宿主的 `seam/fs`、`seam/shell`。

它不是第二条不受控的 fs/shell 通道：

- 路径与命令由 ACP client 自己的策略裁决（工作区、只读、审批）；
- 宿主没有 `acp` 连接、client 未声明能力、或缺少会话上下文时，工具**拒绝**，
  不回退到本地 fs/shell；
- 高风险的 `acp_run` 仍可被 `policy/approval-gate` 之类的工具闸门拦截。

## 2. 工具

| 工具 | 参数 | 返回 |
|---|---|---|
| `acp_read_file` | `path`（客户端绝对路径）、`line?`、`limit?` | 文件文本 |
| `acp_write_file` | `path`、`content` | `已写入 <path>(N 字节)` 或失败原因 |
| `acp_run` | `command`、`args?`、`cwd?`、`timeoutSeconds?`、`outputByteLimit?` | 终端输出、退出码或超时 |

`acp_run` 的默认超时 120s，上限 600s；超时会 `terminal/kill` 后返回 `StatusTimeout`。
无论成功、失败还是取消，都会 `terminal/release`（用脱离取消的 ctx 清理）。

## 3. 失败语义（fail-closed）

| 情况 | 结果 |
|---|---|
| 没有 `acp` 连接 | `StatusDenied`，文案指明 ACP client 不可用 |
| client 未声明对应能力 | `StatusDenied`，文案指明缺哪个能力 |
| 缺少 `call.SessionID` | `StatusDenied`（ACP 的文件/终端都按会话归属） |
| 参数非法 / 传输错误 | `StatusError` |
| 终端超时 | `StatusTimeout`，且已 kill + release |
| 非零退出码 / 被信号终止 | `StatusError`，输出仍返回 |

模型能看到的状态与 ACP 的 `ToolCallStatus` 一致（成功/失败/超时），
控制层会把它们映射成 `tool_call_update`。

## 4. 安全与限制

- 路径是客户端视角的绝对路径，不经过 `seam/workspace` 的虚拟路径解析；
  宿主的 workspace 授权不会自动作用于 ACP client 的文件。
- 不要把 token/密码写进 `command`/`args`：它们会进入客户端终端历史与日志。
- 输出在适配器侧按 rune 截断到 32K，避免一次工具结果撑爆模型上下文；
  客户端自己的 `outputByteLimit` 仍然生效。
- 终端输出不是流式的：`acp_run` 等命令退出后一次性返回；需要持续观察的场景
  仍然要用 `seam/shell` 或等后续的终端订阅能力。

## 5. 配置

无配置项。启用方式：preset 同时包含 `seam-acp`、`acp-helper`，并在宿主里由
`ada acp` 把连接绑定到 `acp` 服务（`platform/host/acp` 已负责）。

## 6. 测试

```powershell
go test ./plugins/tool/acp-helper
go test -race ./plugins/tool/acp-helper
```

覆盖：读写参数与结果、未声明能力拒绝且不发请求、缺少 sessionId、没有连接、
终端 create/wait/output/release 顺序、非零退出码、超时 kill + release。

## 7. 边界

- 没有持久终端/交互式 stdin；
- 没有 `terminal/output` 的增量订阅；
- 没有把 ACP client 的文件能力接入 `seam/fs`（见 [seam-acp.md](seam-acp.md) §3）。
