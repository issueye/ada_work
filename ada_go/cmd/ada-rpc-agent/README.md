# ada-rpc-agent

把一份 agent preset 装配成工作区级的 RPC 宿主，供 ada_chat 这类桌面壳调用：同一个工作目录
共享一个进程；进程的工作目录就是用户选择的工作目录，`fs-local` 的 `/workspace` 根在装配前被改成该目录。

```bash
go build -o bin/ada-rpc-agent.exe ./cmd/ada-rpc-agent
```

## 环境契约

宿主（ada_chat）启动本进程时设置环境变量，并把 `cmd.Dir` 设成工作目录：

| 变量 | 必填 | 说明 |
|---|---|---|
| `ADA_BOT_ADDR` | 是 | `pipe://name` / `unix:///abs/path` / `tcp://host:port`；token 鉴权 |
| `ADA_BOT_TOKEN` | 是 | RPC token；只走环境，不进行程命令行 |
| `ADA_GO_AGENT_PRESET` | 是 | agent preset 的绝对路径（例：`examples/agent/agent.preset.json`） |
| `ADA_BOT_NAME` | 否 | 机器人名；写入 `system-prompt` 的 identity 与 `bot.info` |
| `ADA_BOT_MODEL` | 否 | 只用于 `bot.info` 展示；实际模型由 preset 决定 |
| `ADA_BOT_INSTRUCTION` | 否 | 追加到默认 session 的 `system-prompt` identity；`chat.send` 带的指令按 session 覆盖 |
| `ADA_BOT_WORKSPACE` | 否 | 工作目录；缺省用进程 cwd |
| `ADA_BOT_CLIENT` | 否 | 允许的客户端名，默认 `ada-chat` |
| `ADA_GO_AGENT_BASE` | 否 | preset 相对路径的基目录；缺省从 preset 目录向上找 `go.mod`（源码）或 `.ada-base`（发布包） |
| `ADA_LLM_PROVIDER` | 否 | 覆盖 preset 的模型供应商：`openai` / `replay`；空表示跟随 preset |
| `ADA_LLM_ENDPOINT` | 否 | OpenAI 兼容的 `/v1` 地址（`openai` 时生效，空则用官方端点） |
| `ADA_LLM_MODEL` | 否 | 请求使用的模型 ID（`openai` 时生效，空则用 `gpt-4o-mini`） |
| `ADA_LLM_API_KEY` | 否 | OpenAI 兼容端点密钥（`llm-openai` 会优先从 secrets 取，缺省回落到该环境变量） |

### 供应商覆盖

`ADA_LLM_PROVIDER` 不为空时，宿主适配器会修改 preset 的已展开清单：

- `openai`：启用 `llm-openai`、禁用 `llm-replay`，把 endpoint/model/apiKeyEnv 写入 `llm-openai` 配置，
  并把 `agent-loop.model` 设为所选模型；
- `replay`：启用 `llm-replay`、禁用 `llm-openai`，`agent-loop.model` 取 replay preset 的默认模型。

因此桌面端可以在可视化设置里切换供应商，而不需要改 preset 文件。

## 协议方法

| 方法 | 参数 | 结果/事件 |
|---|---|---|
| `bot.info` | — | `{name, model, workspace, pid, preset, instruction}` |
| `chat.send` | `{turn, text, botId?, botName?, instruction?, sessionId?}` | 运行期推送 `chat/delta` / `chat/reasoning` / `chat/tool` / `chat/approval` / `chat/error` / `chat/done`，最后返回 `{turn, text, steps, toolCalls}` |
| `chat.approve` | `{id, allow, reason?}` | 回答一次 `chat/approval`；未知或已结束的 id 返回 `EINVALID` |

同一个工作区进程可服务多个机器人：`botId` 不同则各自建一个 agent session，共享同一个进程、
fs/workspace、preset、模型与 LLM provider；`chat.send` 串行执行。`ctx` 被 RPC
取消时透传给 `agent.Run`。

`sessionId` 是宿主侧的对话 id：同一个机器人可以有多条对话，每条对话对应独立的 agent session
（模型上下文互不影响）。不带 `sessionId` 的旧客户端仍按每 `botId` 一个 session 工作。

### 每个机器人的身份

`botName` 与 `instruction` 由宿主按轮传入，宿主适配器在 `llm/request` 上把系统提示里的
默认身份段替换成该 session 的身份（规则/工具段不动）。所以：

- 同一个进程里的第二个机器人看到的是自己的名字与指令，不是 `ADA_BOT_NAME/INSTRUCTION` 的默认值；
- 设置页改机器人名称/指令后，**下一轮**即生效，不必重启进程；
- 模型与 provider 仍是进程级：`ADA_LLM_*` 改了要新进程才生效。

### 安全边界

协议底座（seam/rpc + rpc-json/net/pipe）之外，本宿主固定挂载 `rpc-guard` 与 `rpc-observe`：

- `rpc-guard` 的 `peers`/`methods`/`listen` 白名单在启动时由 `ADA_BOT_CLIENT`、本进程实际监听地址
  与 `bot.info`/`chat.send`/`chat.approve` 三个方法拼出，门禁是装饰器，不靠事件；
- token 仍由 `ADA_BOT_TOKEN` 走环境，管道 ACL 由 `rpc-pipe` 负责；
- `rpc-observe` 只发 `rpc/observe` 事件（观察不是门禁），payload 不含 params/result/token。

### 事件

`chat.send` 运行期推送以下事件，宿主按 `turn`（工具卡片再按 `id`）合并：

| 事件 | 载荷 | 说明 |
|---|---|---|
| `chat/delta` | `{turn, text}` | 正文增量；非流式 provider 没有增量时，退回整段回放 |
| `chat/reasoning` | `{turn, text}` | 思考过程增量，宿主可做成可折叠区域 |
| `chat/tool` | `{turn, id, name, args, status}`，结果事件另有 `text`、`durationMs` | 工具开始为 `status="running"`，结果按 `ok`/`denied`/`error`/`timeout` 区分；`text` 截断到 8 KiB |
| `chat/plan` | `{turn, title, steps:[{content,status,priority}]}` | 会话计划更新；来自 session 的 `plugin:plan/updated`，Run 期间实时转发，结束后补发最新一份 |
| `chat/approval` | `{turn, id, source, op, target, reason, timeoutMs}` | 升级审批请求；宿主用 `chat.approve` 回答，超时/取消按拒绝 |
| `chat/error` | `{turn, reason}` | 本轮失败的可读原因（RPC 响应仍是 `EINTERNAL`，细节只走事件） |
| `chat/done` | `{turn}` | 本轮结束 |

工具事件来自 `tools/pre-execute` 与 `tools/result`，只做转发，不参与策略判定（事件不是门禁）。
计划事件来自会话日志（`plugin:plan/updated`），不是总线事件；`chat.send` 在 Run 前订阅、
Run 后 drain 并补发最新计划，保证任务面板最终状态不丢。

### 审批（按 preset 启用）

只有 preset 里装了 `seam-approval`（以及 `approval-gate`、`fs-sandbox` 这类发起方）时，本宿主才注册
名为 `desktop-rpc` 的审批人，把升级请求转给桌面宿主：

1. agent 发 `chat/approval`，带 `turn`/`id`/发起方/目标/原因/超时；
2. 宿主显示审批界面，用 `chat.approve {id, allow, reason}` 回答；
3. 超时、取消、没有宿主答复一律按拒绝（fail-closed）。

示例 preset 见 `examples/agent/approval.preset.json`（`approval-gate.backend` 固定为 `desktop-rpc`，
把 `read_file` 设为需要审批，配合 replay 脚本即可演示）。
发布包里的 preset 默认不启用审批；需要审批时把 `seam-approval` + `approval-gate` 加进自己的 preset，
并把 gate 的 `backend` 指向 `desktop-rpc`（否则按名字排序可能选中别的审批人）。

## 示例

```powershell
$ws = "$env:TEMP\ada-agent-workspace"
New-Item -ItemType Directory -Force $ws | Out-Null
Set-Content "$ws\a.txt" "hello"

$env:ADA_BOT_ADDR = "pipe://ada-agent-demo"
$env:ADA_BOT_TOKEN = "demo-token"
$env:ADA_BOT_NAME = "代码助手"
$env:ADA_BOT_WORKSPACE = $ws
$env:ADA_GO_AGENT_PRESET = (Resolve-Path "examples\agent\agent.preset.json").Path
$agent = (Resolve-Path "bin\ada-rpc-agent.exe").Path
Push-Location $ws
& $agent
```

ada_chat 侧只设置宿主变量即可（它不再内置 `--bot` 演示运行时）：

```powershell
$env:ADA_BOT_RUNTIME = "E:\codes\go_projects\ada_work\ada_go\bin\ada-rpc-agent.exe"
$env:ADA_GO_AGENT_PRESET = "E:\codes\go_projects\ada_work\ada_go\examples\agent\agent.preset.json"
# 在 ada_chat 仓库里：
go run ./cmd/ada_chat
```

发布包也能直接当 runtime 用：`scripts/package.ps1` 会在包根写一个 `.ada-base` 锚点，
把 presets 里的 `data/…`、`workspace/…` 相对路径落到包根；所以 `Agent preset` 可以指向
`dist/<pkg>/presets/agent-release.preset.json`，不必再从包根启动。

## 已知边界

- **模型与 provider 仍是进程级**：同一进程里的多个 bot 共享 preset、模型与 LLM provider；
  身份/指令按 session 隔离，但项目群组式的「每 agent 独立模型」仍需要 ada_go_v2 引入显式 agent 实例。
- replay provider 的游标是进程级的；多个 bot 共用 replay preset 时会耗尽脚本。真实 LLM 不受此限。
- 审批默认不在发布 preset 里；启用后审批人只有一个（`desktop-rpc`），并发工具同时审批时宿主按队列逐个回答。
- 工具结果只回传文本块（截断 8 KiB），图片/附件等非文本内容不进事件；完整结果仍在 `chat.send` 的返回值里。
- `chat.send` 串行：同一进程里的其他 bot 要等当前轮结束，进度事件也因此只有一条轮次线。
- preset 里的相对文件路径按装配基目录解析：源码是仓库根（`go.mod`），发布包是包根（`.ada-base`）；
  可用 `ADA_GO_AGENT_BASE` 覆盖。
- 身份替换发生在提示预算裁剪之后：超长指令由宿主自己约束，本进程不额外裁剪。
- 仍由宿主负责重启/重连策略；本进程不自动重连。
