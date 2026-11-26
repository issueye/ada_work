# seam/acp — ACP client 反向能力

> 契约来源：[rpc-control-layers.md](rpc-control-layers.md)、ACP（Agent Client Protocol）。
> 本页记录**已实现子集**、与 `seam/fs` / `seam/shell` 的边界、以及安全约束。

## 1. 它是什么

ACP 是双向的：client（编辑器）在 `initialize` 里声明自己能不能代做文件与终端，
agent 再通过反向请求使用这些能力：

| 方法 | 方向 | 能力位 |
|---|---|---|
| `fs/read_text_file` | agent → client | `clientCapabilities.fs.readTextFile` |
| `fs/write_text_file` | agent → client | `clientCapabilities.fs.writeTextFile` |
| `terminal/create` / `output` / `wait_for_exit` / `kill` / `release` | agent → client | `clientCapabilities.terminal` |

`seam/acp` 把这组能力收敛成语言中立的 Go 契约，`provider/control-acp` 的
`Endpoint` 实现它；宿主（`ada acp`）在连接建立后把实现绑定到 `acp` 服务。

## 2. 契约

```go
type Capabilities struct {
    ReadTextFile  bool
    WriteTextFile bool
    Terminal      bool
}

type Client interface {
    Capabilities() Capabilities

    ReadTextFile(ctx, sessionID, path string, line, limit int) (string, error)
    WriteTextFile(ctx, sessionID, path, content string) error

    CreateTerminal(ctx, sessionID string, req TerminalRequest) (terminalID string, err error)
    TerminalOutput(ctx, sessionID, terminalID string) (TerminalOutput, error)
    WaitForExit(ctx, sessionID, terminalID string) (ExitStatus, error)
    KillTerminal(ctx, sessionID, terminalID string) error
    ReleaseTerminal(ctx, sessionID, terminalID string) error
}
```

- **能力是连接级**，不是会话级：`initialize` 之后由 client 声明，端点在
  整个连接生命周期内保持；`ada acp` 一个进程只有一个连接。
- **所有方法都要求 `sessionID`**：ACP 用会话限定文件与终端的归属。
- **路径是客户端视角的绝对路径**，不是 `seam/fs` 的虚拟路径。
- `Registry` 负责「当前连接」：`Set` 绑定、`Dispose` 解绑；没有连接时
  `Get` 返回 `EUNIMPLEMENTED`。

## 3. 为什么不接入 seam/fs、seam/shell

这是有意的取舍，不是遗漏：

1. **路径语义不同**：`seam/fs` 用 `/workspace/...` 虚拟路径，真实宿主根由
   workspace/fs provider 内部解析且不对外暴露；ACP 要求传绝对路径。
   把虚拟路径硬转成绝对路径等于绕过 workspace 的授权与解析。
2. **能力是可选且可能不存在**：`seam/fs` 必须有且只有一个底座实现；
   ACP 的客户端文件能力可能完全没有，不能作为底座。
3. **终端的策略在 client**：`terminal/*` 的沙箱/权限由编辑器负责；把它伪装成
   `seam/shell` 会形成第二条不受本仓库 `shell-guard` 控制的执行路径。

因此本契约**显式消费**：需要编辑器能力的工具/插件 `inject "acp"`，按
`Capabilities` 判断后调用；没有能力时回退到本地 `fs`/`shell` 或直接报错，
由调用方决定，而不是框架偷偷替换。

## 4. 错误语义（fail-closed）

| 情况 | 返回 |
|---|---|
| client 未声明对应能力 | `EUNIMPLEMENTED`（调用方应回退或禁用该工具能力） |
| 缺少 `sessionID` / `path` / `terminalID` / `command` | `EINVALID` |
| 连接不可用、传输错误、client 未返回 `terminalId` | `EUNAVAILABLE` |

能力位在调用前检查，未声明时不会向 client 发出请求（否则会得到
`MethodNotFound`，白白浪费一次往返）。

## 5. 已落地

| 组件 | 状态 |
|---|---|
| `plugins/seam/acp` | ✅ 契约 + `Registry` + 插件 |
| `control-acp` 实现 | ✅ capability 记录 + 7 个反向方法 |
| `ada acp` 绑定 | ✅ 有 `acp` 服务时 `Registry.Set(endpoint)`，退出时解绑 |
| `tool/acp-helper` | ✅ 消费 `acp` 服务：`acp_read_file` / `acp_write_file` / `acp_run` |
| 单元测试 | ✅ 能力门禁、参数映射、terminal 全流程、Registry 生命周期 |

## 6. 安全边界

- 路径与命令由 agent 生成，client 负责最终授权；agent 侧仍应遵守自己的
  workspace/审批策略，不能因为「client 会拦」就跳过本地策略。
- `terminal/create` 的 `env` 可能带 secret：调用方应只传必要变量；
  不要把 token 写进命令行（会进 client 的终端历史/日志）。
- 输出可能很大：`OutputByteLimit` 是 client 侧上限，调用方仍需按工具输出上限截断。

## 7. 尚未做

1. 没有按会话选择「编辑器文件 or 本地文件」的路由策略；
2. `session/new` / `session/load` 的 `mcpServers` 尚未实现：非空时显式返回
   `EUNIMPLEMENTED`，不再静默忽略客户端配置；
3. 终端输出没有接入 `control/update`（工具结果仍走 `tool/result`）。

## 8. 验证

```powershell
go test ./plugins/seam/acp ./plugins/provider/control-acp
go test -race ./plugins/seam/acp ./plugins/provider/control-acp
go run ./cmd/ada plan --preset examples/acp/acp.preset.json
```
