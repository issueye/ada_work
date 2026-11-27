# ada_chat × ada_go_v2：两条产品方向与职责分析

> 本文基于当前代码状态：ada_chat 是「工作区进程 + ACP 客户端」，删除了预置对话、固定回答、测试和快照；
> ada_go_v2 的 `ada acp` 把 agent preset 做成可被桌面宿主启动的 ACP agent（stdio 上的 JSON-RPC 2.0）。
>
> **历史说明**：早期 ada_chat 曾实现 seam RPC v1 客户端并与 `cmd/ada-rpc-agent` 通讯；该 RPC 路径
> 已整体移除（`internal/rpc`、私有 pipe/token 端点、`chat.send`/`chat.approve` 事件面），只保留 ACP。
> 下文提到 RPC 的地方都是历史基线或 ada_go_v2 侧仍保留的能力，不代表 ada_chat 现状。

## 0. 结论摘要

| 问题 | 结论 |
|---|---|
| 谁管理进程 | **ada_chat**。它按**工作目录**（工作区）管理进程实例，相同目录只保留一个进程和一条 ACP 连接。 |
| 谁运行 agent | **ada_go_v2**。kernel、agent loop、tools、LLM、session、workspace/policy 都在它这一侧。 |
| 两者边界 | ada_chat 负责「人、工作区、进程、对话、持久化、操作系统集成」；ada_go_v2 负责「agent 执行、模型、工具、会话、权限与编排」。ACP 是契约，ada_chat 是客户端，ada_go_v2 是服务端。 |
| 个人助手方向 | 现有原型可跑；最先缺的是 runtime 配置 UI、持久化、进程面板、工具/审批事件回传。 |
| 项目群组方向 | 进程/工作区底座已经有了，但缺少「项目」实体、群聊/任务路由、多 agent 运行时与权限模型。 |

---

## 1. 当前基线（代码事实）

### 1.1 ada_chat 现在负责什么

- 原生桌面窗口：侧栏、机器人、对话、输入框、设置（MyGo + Lobe）。
- **工作区进程管理**：`internal/bot.Supervisor` 以规范化后的工作目录为 key（绝对路径 + 解析符号链接 +
  Windows 小写）；同一个目录（包括带尾斜杠/大小写不同的写法）的多个机器人共享一个进程、一条 ACP 连接。
- 进程生命周期：添加机器人或点击机器人时确保该工作区进程运行；最后一个机器人离开时停止进程；
  应用关闭时统一停止所有实例。
- 每轮消息的路由：每个 `sessionId` 绑定 `botID/turn`，共享进程返回的 `session/update` 仍更新正确机器人的会话。
- 状态回写：进程状态（启动中/运行中/失败）、pid 与会话工作目录会广播给共享该进程的所有机器人。
- 外部运行时：设置页选择 `Agent 路径`/`Agent preset`/模型供应商（文件选择器、Endpoint/模型/API Key），
  保存到本机 `config.json`；留空项回落到 `ADA_BOT_RUNTIME` / `ADA_GO_AGENT_PRESET` /
  `ADA_LLM_PROVIDER` / `ADA_LLM_ENDPOINT` / `ADA_LLM_MODEL`。不再内置固定回答的测试机器人。
- 供应商：设置页可在「跟随 preset / OpenAI 兼容 / Replay」之间切换；agent 在装配 preset 时
  启用对应 provider、禁用另一个，并改写 `agent-loop.model`。供应商设置是**进程级**的，新进程生效。
- 空工作台：没有预置对话；添加机器人后自动打开它的第一条对话。

### 1.2 ada_go_v2 现在负责什么

- kernel/plugin/preset 装配，agent loop、tools、LLM provider、session、prompts、workspace、fs/shell、approval、caps 等业务插件。
- `ada acp` 宿主适配器（`platform/host/acp` + `platform/host/cli`）：
  - 按宿主给的 preset 装配 agent，把工作区根绑到 agent 进程的 cwd；
  - 在 stdin/stdout 上说 ACP：`initialize` / `session/new|load|prompt|cancel` / `session/update` /
    `session/request_permission`；
  - 每个 ada_chat 对话一个 agent session（同进程共享 agent 配置、模型与 preset）。
- `cmd/ada-rpc-agent` 与 seam RPC 栈仍留在 ada_go_v2（供其它客户端/测试使用），但**不再是 ada_chat 的运行时**。

### 1.3 已实际验证

- 两个机器人指向同一个临时工作目录：只启动 **1 个** ACP agent 进程，`InstanceInfo.Bots == 2`。
- ACP 互操作（`TestACPInterop`）：真实 `ada acp` 进程 → 文本/工具/计划更新落到会话。
- 移除第一个机器人后进程仍在（`Bots == 1`）；移除最后一个后实例为 0。
- 设置页保存的 `Agent 路径`/`Agent preset` 写入配置文件；重启后自动加载，新进程使用保存的路径启动
  （实测在无 `ADA_BOT_RUNTIME` 环境下仍能启动并返回真实回答）。
- 设置 OpenAI 兼容供应商后，agent 启用 `llm-openai`、禁用 `llm-replay`；经 mock OpenAI SSE
  端点实测，请求带保存的 Bearer key，回复来自该供应商。
- 审批往返已跑通（`TestACPInteropPermission`）：preset 叠加 `approval-gate` 后，agent 发
  `session/request_permission`，宿主允许一次，工具继续执行；超时/拒绝按 fail-closed。
- 发布包风格 preset（`data/…` 相对包根 + `.ada-base` 锚点）可以经 `ada acp` 装配成功。
- 机器人、对话与消息持久化到 `%AppData%\ada_chat\state.json`：添加/编辑、发送、轮次结束、
  改名/删除与关窗都会落盘；重启后侧栏机器人与历史消息恢复，进程按「未启动」恢复，
  正在生成的消息标记为未完成、运行中的工具记为中断（`internal/app/state_test.go`）。
- 自动化测试：ACP 端到端互操作（`internal/bot/acp_interop_test.go`）与审批队列单测
  （`internal/app/approval_test.go`）；互操作测试默认跳过，配置 `ADA_ACP_INTEROP_*` 才拉起真实 agent。

### 1.4 当前原型暴露的边界

- 同一进程内的多个 bot 共享 **agent preset、模型、LLM provider 和供应商设置**；session 与
  system prompt 里的身份段按 bot 分开（名称/指令下一轮生效），模型/provider 仍要重启进程。
- replay provider 的游标是进程级的，第二个 bot 的 replay 会话会「脚本已用完」；真实 LLM 不受此限，
  但「同工作区多机器人」在 ada_go_v2 侧仍缺 per-agent provider 隔离。
- 进程只有「启动/停止」两个隐式动作，没有 UI 面板、健康检查、重启策略、日志入口。
- 审批只在 preset 装了 `seam-approval` 时可用；发布预设默认不装，因此默认仍是 fail-closed。
- 偏好（名称、Enter、agent 路径/preset、供应商设置）写进 `config.json`；机器人、对话与消息写进
  `state.json`，重启后恢复。进程状态、审批进行中、窗口尺寸仍不落盘。API Key 目前明文存在
  `config.json`（0600），后续需要 keychain/DPAPI。
- 一个机器人可以有多条对话：右键「新建会话」会带新的 `sessionId`，agent 为每条对话建独立
  session，新对话不继承旧对话的模型上下文；进程/preset/模型仍按工作区共享。

---

## 2. 方向一：个人机器人助手

### 2.1 场景

一个用户在本机拥有若干机器人（例如「代码助手」「写作助手」），每个机器人绑定自己的工作目录；
用户从侧栏选择一个机器人开始对话，机器人能读写该目录、调用工具、使用模型。重点在**单人、少机器人、
低协作、强本地隐私**。

### 2.2 ada_chat 的角色

| 职责 | 现状 | 需要补充 |
|---|---|---|
| 机器人注册表 | 内存 + `state.json` 持久化 | 编辑/复制/删除机器人、重命名工作区别名 |
| 工作区进程管理 | 已有（同目录共享） | 进程面板：状态、pid、日志、启动/停止/重启；健康检查与退避重启 |
| 对话体验 | 有基本对话、流式回写与本地持久化 | 搜索、导出；附件/文件引用；消息重试与取消的 UI |
| Runtime / 供应商配置 | 设置页已有 Agent 路径、preset、供应商/Endpoint/模型/API Key（持久化） | 再加运行时诊断、密钥安全存储（keychain）、per-agent 供应商覆盖 |
| 审批/工具 | 工具卡片 + 审批弹窗已接入（`session/request_permission`，超时 fail-closed） | 把审批按项目/角色配置；审批历史与审计视图 |
| 身份与密钥 | 无 | 用户显示名；模型密钥进 OS keychain/秘密存储，不写日志 |
| 系统集成 | 有窗口 | 单实例锁、托盘/开机启动、通知、关闭窗口时的进程策略 |
| 安全边界 | ACP stdio + 子进程 | 进程权限、可执行文件签名/来源、工作目录授权提示 |

### 2.3 ada_go_v2 的角色

| 职责 | 现状 | 需要补充 |
|---|---|---|
| agent 执行 | `agent-loop` 已可用 | 面向宿主的稳定生命周期 API（初始化/健康/关闭） |
| 模型与会话 | LLM provider + session | per-session/per-agent 模型与 prompt 配置；会话持久化（JSONL/内存可选） |
| 工具与文件 | tools、fs、shell、workspace 已有 | 工具/审批事件通过 ACP 暴露；大对象走句柄而不是 JSON |
| 工作区 | workspace-local、fs-local；动态根由宿主 patch | 正式 `workspace.configure`/`workspace.info` 方法，不用装配期改 manifest |
| 安全 | caps/guard/approval、TLS、RPC guard 已有 | 与桌面宿主的信任边界：签名、密钥来源、审计出口 |
| 进程模型 | 每个工作区一个宿主进程（原型） | 进程内多 session；进程级健康/关闭；标准输入输出/日志约定 |

### 2.4 契约需要补的方法（个人助手）

- `runtime.hello`：版本、features、工作区、可用 agent 列表。
- `agent.configure`/`agent.list`：每个机器人的名称、instruction、模型、工具集。
- `session.create`/`session.list`：宿主显式创建与恢复会话。
- `chat.send` 明确 `agentId`/`sessionId`，返回可取消的 turn id。
- `tool.*` 事件（开始/参数/结果/失败）、`approval.request`/`response`。
- `process.health`/`process.shutdown`：宿主重启与优雅退出。
- `workspace.info`/`workspace.watch`：工作目录、文件变更、只读状态。

---

## 3. 方向二：项目群组

### 3.1 场景

一个「项目」对应一个工作目录或工作区集合，多个机器人承担不同角色（规划、实现、评审、测试），
用户可以建群聊、@某个机器人、派任务、查看共享文件与活动记录。重点在**多 agent 协作、共享上下文、
权限与可观测性**。

### 3.2 ada_chat 的角色

| 职责 | 现状 | 需要补充 |
|---|---|---|
| 项目实体 | 无 | 项目 = 工作区 + 成员机器人 + 权限 + 绑定进程；项目列表/创建/归档 |
| 进程管理 | 按工作目录一个进程 | 项目级进程面板；项目空闲后的保温/回收策略；项目级重启 |
| 群聊 | 只有单 bot 会话 | 群聊/线程；@机器人、消息路由、多轮任务；成员状态与在线状态 |
| 成员与角色 | 机器人只有名称/图标/指令 | 角色（规划/实现/评审）、能力标签、工具/模型配额 |
| 任务与活动 | 无 | 任务板、分配给 agent、活动时间线、产物/文件树、审批队列 |
| 权限 | 无 | 项目成员权限、目录只读/可写、危险工具审批、审计视图 |
| 通知 | 无 | 任务完成、审批、失败通知；未读状态 |
| 共享上下文 | 无 | 项目级会话与文件上下文；agent 之间的交接摘要 |

### 3.3 ada_go_v2 的角色

| 职责 | 现状 | 需要补充 |
|---|---|---|
| 多 agent 运行时 | `agent-loop` 是单 agent；flow/fanout/sched 是编排层 | 进程内 agent registry：多 agent、每 agent 独立 prompt/模型/session |
| 路由 | `chat.send` 只带文本（原型后已带 botId，但仅分 session） | `agent.*`/`group.send`/`handoff` 协议；群消息与定向消息 |
| 编排 | M4 flow/fanout/sched 已有 | 面向项目的任务分配、并发/回压、失败重试、取消 |
| 共享工作区 | workspace/fs 共享根 | 多 agent 文件冲突/锁/只读策略；文件变更事件 |
| 权限与审批 | caps/guard/approval 已有 | 按 agent/项目角色授予能力；审批事件跨 RPC |
| 会话与记忆 | session/session-jsonl | 群共享记忆 + 每 agent 私有记忆；交接摘要；会话恢复 |
| 观测 | audit-log/observe | 项目级活动流、agent 用量/步骤/工具统计 |

### 3.4 需要补的协议（项目群组）

- `agent.register`（角色、指令、模型、工具集、权限）。
- `group.create`/`group.members`/`group.send`（或统一 `chat.send` 带 `groupId`/`mention`）。
- `task.assign`/`task.status`/`handoff`：agent 间任务转移。
- `workspace.watch` 文件树与变更；`artifact.put/get` 句柄。
- `approval.request`/`response` 与项目级审批队列。
- `agent.health`/`metrics`：运行状态、步骤、token/工具用量。
- 并发/取消/回压：同一工作区的多 turn 调度与限流。

### 3.5 当前原型的直接缺口（来自实测）

- 一个进程里多 bot 已经按 session 分开 prompt 身份段，但 **model/provider 仍是共享的**；replay 游标
  就是进程级，第二个 bot 会耗尽脚本。要支持项目群组，ada_go_v2 必须把「agent 实例」做成显式对象，
  不能只靠宿主切 session/prompt。
- ada_chat 侧没有项目/成员/群聊数据结构；现在只能「一个 bot 一条对话」。
- 没有任务、审批、文件变更与活动视图，项目协作无法闭环。

---

## 4. 两个方向下的职责分工

| 层 | ada_chat | ada_go_v2 | 不放在对方的原因 |
|---|---|---|---|
| 进程与 OS | 工作区实例、pid、日志、启动/停止、托盘/单实例 | 只作为被启动的进程，不管理自己 | 进程属于宿主桌面生命周期 |
| 产品/UX | 机器人/项目/群聊/任务/审批/通知界面 | 只返回数据与事件 | agent 不应知道具体 UI |
| agent 执行 | 不实现 | agent loop、工具、模型、session | 把模型/工具语义塞进 UI 会腐蚀边界 |
| 工作区 | 让用户选择目录、授权、绑定项目 | fs/workspace 解析与策略、虚拟路径 | 目录选择是宿主策略 |
| 持久化 | 桌面配置、会话/项目元数据、窗口状态 | 运行时 session/turn 记录、审计 | 两者各有状态，需明确归属 |
| 安全 | 进程来源、用户授权、密钥入口 | caps/guard/approval、工具执行边界 | 强制执行必须在执行侧 |
| RPC | 客户端与宿主适配 | 服务端与 handler | 契约由两边共同演进，客户端/服务端分离 |

---

## 5. 建议的补充顺序

### P0（两个方向都依赖）

1. **Runtime / 供应商配置与进程面板**：设置页的 Agent 路径/preset/供应商已落地并持久化；下一步补
   运行中实例、pid、日志、重启按钮与运行时诊断，以及 API Key 的安全存储。
2. **持久化**：机器人、对话、设置、最近工作区已落盘并恢复；剩余窗口状态、审批历史与会话搜索/导出。
   会话存储优先放宿主，turn/tool 记录可由 ada_go_v2 的 session 提供。
3. **显式 agent/session 协议**：`agent.list`、`session.create`、`chat.send` 带 agentId/sessionId；
   agent 配置从宿主传入（替代装配期 patch manifest）。
4. **事件回传**：tool 开始/结果、错误、审批请求/响应、取消；接到现有工具卡片与审批 UI。
5. **补回测试**：当前按本轮要求移除了测试；重做时至少补 `supervisor`（同工作区单进程）、
   RPC 契约、真实 runtime 端到端三类测试。

### P1（个人助手）

- Bot 注册表与编辑/删除、图标/工作目录/指令/模型；首个机器人向导。
- 模型密钥的 OS keychain/秘密注入；离线 replay 与在线模型切换。
- 会话搜索/导出/附件；通知、托盘、开机启动、单实例。
- 进程健康检查、退避重启、优雅关闭、日志诊断。

### P1（项目群组）

- 项目/成员/角色数据模型；项目绑定工作区与进程。
- 群聊/线程/mention 与消息路由；任务板、活动流、文件树。
- ada_go_v2 多 agent 实例与 per-agent prompt/model/provider；任务分配与 handoff。
- 项目级权限、审批、只读工作区、审计。

### P2

- 项目共享记忆/交接摘要；文件冲突与锁；用量/成本统计。
- 远程/多人协作（TLS/网关）；项目模板与 agent 市场。

---

## 6. 当前验证与未验证

- 已验证：`go build ./...`、`go vet ./...`；跨仓库互操作测试（`internal/rpc/interop_test.go`，
  需要 `ADA_INTEROP_*`）覆盖握手/`bot.info`/`chat.send`/事件流、每 session 的身份替换与审批往返；
  审批队列另有 `internal/app/approval_test.go`；临时探针确认同工作区两机器人一个进程；
  外部 runtime（`ada-rpc-agent` + replay preset）能读工作目录 `a.txt` 并返回真实回答；
  移除一个机器人不杀进程，移除最后一个杀进程。
- 未验证 / 未实现：多 bot 在共享进程下的独立 model/provider（replay 耗尽暴露了这一点）；
  发布预设的审批（默认未启用）；窗口状态与审批历史持久化；进程面板；群聊/项目模型；chat 侧 TLS/远程传输。
- 自动化测试仍然很少（只有上面两组）；其余靠 `go build`、`go vet` 与临时探针。

## 7. 待确认的产品决策

1. 同一工作区的多个机器人是「共享一个 agent」还是「一个进程里多 agent」？
   现在实现是共享进程 + 每 bot 一个 session；项目群组方向需要后者。
2. 进程在没有机器人时是立即停止，还是保温一段时间以减少冷启动？
3. 会话/项目元数据归宿主（ada_chat）还是运行时（ada_go_v2）？
4. 个人助手是否需要多用户/远程；如果不需要，TLS/远程传输可以延后。
5. 项目群组是本机单用户协作，还是要支持多人共享同一项目？
