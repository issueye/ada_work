# M4.5 / M5 验收矩阵

> 本文件把里程碑验收项映射到**实现位置、证据命令与已知边界**。口径来源:
> [plugin-kernel-design.md](plugin-kernel-design.md) §17、AGENTS §10。
> 最近更新:2026-11-24(583 个 Test + 5 个 Benchmark;不做 v1 插件迁移)。
>
> 执行层在 2026-11-24 接入**调用边界驱动的事件循环**(`await`/`fs.promises`/`setTimeout 调用内生效),
> 见 [event-loop.md](event-loop.md) 与 [ADR 0014](adr/0014-call-scoped-event-loop.md);后台常驻定时器仍不支持。

## 0. 结论速览

| 里程碑 | 状态 | 说明 |
|---|---|---|
| M4 编排 | ✅ 完成(文档口径) | platform/sched/platform/quota/platform/flow/platform/bundle/回收/延迟激活/jobs 锁 |
| M4.5 Agent Core | ⚠️ 第一刀完成 | prompts/llm/tools/agent-loop + 最小 preset + memory/jsonl session + turn-stats 投影 + compaction-basic;真实端点与策略已落地,模型摘要可选/并行工具 |
| M5 安全与宿主 | ⚠️ 部分完成 | 安全 seam/caps/install/宿主/C13/inspect chain 已落地;仅剩 **OS 级沙箱强制** 与 **真实桌面窗口** |
| RPC 通讯(新增) | ✅ P0–P4(TLS) | seam/rpc + rpc-json + tcp/unix/pipe + rpc-guard/rpc-observe + tls://;SO_PEERCRED/桌面 Transport 待做 |

## 1. M4.5 验收矩阵

| 任务 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| C1 session seam + memory/jsonl | `plugins/seam/session`、`plugins/provider/session-memory`/`session-jsonl`、`plugins/core/session-stats` | `session` 单测、`sessiontest`;jsonl 契约/重开/并发/半行恢复;`sessionstats` 投影 | 投影检查点未持久化;jsonl 无多进程租约 |
| C2 prompts seam + system-prompt | `plugins/seam/prompts`、`plugins/core/system-prompt` | `prompts` 3 个用例 | 引擎在 seam 内,未拆 provider;预算为字符估算 |
| C3 llm seam + provider | `plugins/seam/llm`、`plugins/provider/llm-replay`/`llm-openai` | `replay` 2 个用例、`openai` wire/真实端点(可选) | OpenAI 兼容端点已实测;stream 改写结果未回传;无 deepseek 专用适配 |
| C4 tools seam + 文件工具 | `plugins/seam/tools`、`plugins/tool/text-helper`(TS)、`platform/bridge`(`ada:fs`) | `tools` 2 个用例;`bridge.TestJSToolText`(读/写/编辑/搜索/列目录);`cli.TestAgentReplay` | 搜索仅字面量;编辑 read-before-edit;`ada:fs.search` 参数顺序已修正 |
| C4b 脚本工具 | `plugins/tool/git-helper`、`platform/bridge`(`ctx.tool`/`ada:shell`) | `cli.TestAgentGitScriptPlugin`;真实端点手工 | 无 SDK 类型;JS→原生 shell 无 ctx 取消 |
| C4c 脚本持久化工具 | `plugins/tool/task-helper`、`platform/bridge`(`ada:fs`) | `cli.TestUpTaskScriptPlugin`、`cli.TestAgentTaskScriptPlugin`;真实端点手工 | 任务文件不是会话事实;read-modify-write 无事务 |
| C4d 脚本命令工具 | `plugins/tool/command-helper`、`ada:shell` | `cli.TestUpCommandScriptPlugin`、`cli.TestAgentCommandScriptPlugin`;真实端点手工 | 白名单不是安全边界;无 shell 解析 |
| C4e 脚本文本工具 | `plugins/tool/text-helper`、`ada:fs` | `cli.TestUpTextScriptPlugin`、`cli.TestAgentTextScriptPlugin`;真实端点手工 | 搜索字面量/无正则;fs-local 路径约束 |
| C4f TS shell 工具 | `plugins/tool/shell-helper`(TS)、`platform/bridge`(`ada:shell.exec`/结构化工具结果)、`ada_jsrt/jsrt` | `bridge.TestJSToolResultMapping`、`TestJSShellExecStructured`、`cli.TestAgentToolShellScriptPlugin` | 仍是策略/沙箱之外的执行入口;发布安全边界见 tool-shell-helper.md |
| C4g Node 内置模块 | `ada_jsrt/jsrt`、`ada_jsrt/jsrt/nodemods`、moejs `node` 包 | `bridge.TestNodeBuiltins`、`TestNodeChildProcess`、`TestNodeHTTPDNSNet`、`TestNodeHTTPError`、`TestNodeFSDisabledByPolicy` | `node:fs/path/os/events/util/assert/url/querystring/string_decoder/process/child_process/http/https/dns/net/buffer`;有 fs/workspace 策略时 `node:fs` 不可用;网络模块不做 `net-guard` 限制(ADR 0011/0012,已移除该策略);无 `createServer`/`listen` |
| C4h HTTP 工具与 zlib | `plugins/tool/http-helper`(TS)、`ada_jsrt/jsrt/nodemods`(`node:zlib`)、`ada_jsrt/jsrt`+`platform/bridge`(`ada:secrets`) | `bridge.TestJSToolHTTPHelper`、`TestSearchDeepSeek`/`DuckDuckGo`/`SearXNG`、`bridge.TestNodeZlib` | 网络无 seam/caps/审批(ADR 0011/0012),默认不在发布 preset 启用;`node:zlib` 只做同步 gzip/deflate/raw,输出上限 8 MiB;`http_search` 的 DeepSeek key 只经 `ada:secrets` 按名读取 |
| C4i 插件开发工具 | `plugins/tool/plugin-helper`(TS)、`examples/plugin-helper` | `bridge.TestPluginHelperScaffoldAndLoad`(生成→体检→列表→装载→调工具)、`TestPluginHelperPreset`、`cli.TestPlanPluginHelperExample` | **只生成 TS**(生成即装配);Go 插件需 builtins 注册后重编;写入受 fs 策略约束;不做删除操作 |
| C4j 技能工具 | `plugins/tool/skill-helper`(TS)、`examples/skill` | `bridge.TestSkillHelperListAndShow`/`TestSkillHelperErrors`/`TestSkillHelperScaffold`、`cli.TestPlanSkillHelperExample` | 技能是 Markdown 数据(SKILL.md + 附带文件),只读/写入全走 `ada:fs`;`skill_show` 只允许技能目录内的相对文件;不执行 `scripts/` |
| C5 agent-loop | `plugins/core/agent-loop` | `agentloop.TestTurnWithReplayAndTool`、`TestContextBudgetReturnsECONTEXT`、`TestCompactionFitsBudget`、`TestParallelTools` | 并行工具(maxParallelTools);contextBudget + compaction-basic;模型摘要可选;无 core/agent 拆分 |
| C6 最小 preset + golden | `examples/agent` | `ada plan/up --preset examples/agent/agent.preset.json`;事件序列 golden;`TestRealAgentTurn` 实测真实端点 tool call | 默认回放模型;真实端点需 env |
| C7 策略桩 | `plugins/policy/approval-gate`、`llm-retry`、`tool-timeout` | approval-gate 2、llm-retry 3、tool-timeout 1 | 全部落地;后续可加 tool-repeat-guard 等 |
| C8 会话级计划 | `plugins/seam/plan`、`plugins/core/plan`、`platform/bridge`(`ada:plan`) | `core/plan` 单测、`agent-loop.TestPlanInjectedBeforeLastUserMessage`、`control-acp.TestPlanMapping`、`bridge.TestJSPlanModule`、`cli.TestACPServe` | 只注入最新计划;步骤状态由模型显式更新;TS 通过 `ada:plan` 访问 |
| C9 脚本会话与私有状态 | `platform/bridge`(`ada:session`、`ctx.store`、`ada:store`)、`ada_jsrt/jsrt` | `bridge.TestJSSessionAndStore`、`bridge.TestJSSessionHistory`、`jsrt.TestHostPlanCallbacks` | 插件事件强制 `plugin:<id>/` + Ignorable;store 进程内、重启不保留;事件读取有界(≤200 条 / 256 KiB) |

## 2. M5 验收矩阵

### 2.1 沙箱与文件/命令安全

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| sandbox 判定 seam | `plugins/seam/sandbox` | `sandbox`(5) | 只做判定,不强制 |
| policy-only 后端 | `plugins/provider/sandbox-none` | `sandbox` 用例 | `Enforced=false`(**OS 级强制见 ADR 0001**) |
| fs seam/provider | `seam/fs`,`provider/fs-local` | `fs`(7) | TOCTOU 已知;父目录符号链接写逃逸已修(`TestWriteThroughSymlinkParent`) |
| fs 沙箱策略 | `policy/fs-sandbox` | `fs-sandbox`(5) | 可升级走 approval |
| fs 脱敏/观察 | `policy/fs-redact`,`fs-observe` | 各 5 | 观察不含内容 |
| shell seam/provider | `seam/shell`,`provider/shell-local` | `shell-local`(11) | 超时杀进程组(POSIX)/Windows Job + WaitDelay |
| shell 策略(platform/caps/审批) | `policy/shell-guard` | `shell-guard`(8) | `allowShell` 仍是配置开关 |
| shell 脱敏/观察 | `policy/shell-redact`,`shell-observe` | 各 5 | 分块边界可能漏脱敏 |

### 2.2 审批与能力授权

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| approval seam | `plugins/seam/approval` | `approval`(7) | fail-closed,无审批人即拒绝 |
| 审批实现 | `approval-deny`,`approval-cli` | `approval-cli`(5) | CLI y/yes 交互 |
| 静态能力授权 | `plugins/provider/caps-static`,`platform/caps/` | `caps`(7)、`caps-static`(1) | 安装根按 index trust 回填;普通磁盘目录 third-party |
| 调用点强制 | `policy/caps-gate` | `caps-gate`(4) | fs/shell 调用点 |
| C13 拒绝矩阵 | `platform/securitytest` | `TestFSMatrix`、`TestFSNoCapDenied`、`TestShellMatrix` | 覆盖 fs/shell;caps 拒绝已入审计;通用 payload 脱敏待补 |
| ~~网络升级审批~~ | ~~`policy/net-guard`~~ | 已移除 | ADR 0012:网络不再有受控 seam |

### 2.3 网络与 secret

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| Node 网络模块 | `ada_jsrt/jsrt/nodemods`、`platform/bridge/node_net.go` | `TestNodeHTTPDNSNet`、`TestNodeHTTPError`、`TestNodeHTTPSInsecure` | 直连标准库,不经 seam/caps(ADR 0011/0012);无 createServer |
| ~~net seam/provider~~ | ~~`seam/net`,`provider/net-http`~~ | 已移除 | ADR 0012:无消费者,且策略无法覆盖 Node 路径 |
| secrets seam/provider | `seam/secrets`,`provider/secrets-env` | `secrets-env`(5) | 白名单空=拒绝;`ADA_SECRET_*` |
| secrets 授权 | `policy/secrets-guard` | `secrets-guard`(3) | 名称正则 + principal |

### 2.4 工作区

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| workspace 契约与链 | `plugins/seam/workspace` | `workspace`(5) | Chain/绑定/错误码;seam 无实现 |
| 本地工作区 provider | `plugins/provider/workspace-local` | `workspace-local`(8) | 多根/只读/符号链接;根在装配期校验 |
| 路径策略 | `plugins/policy/workspace-guard` | `workspace-guard`(5) | 段前缀 + 单段 glob;非完整 gitignore |
| 工作区查询工具 | `plugins/tool/workspace-helper` | `workspace-helper`(3) | 只读;结果不含宿主根 |
| fs/shell 托管解析 | `provider/fs-local`、`provider/shell-local` | `fs-local` 托管模式 3 例、`shell-local` 托管模式 3 例 | 旧 preset 仍走独立模式;`/tmp` 仍本地 |
| 会话绑定 / CLI | `core/agent-loop`、`platform/host/cli` | `agentloop.TestWorkspaceBindingPropagates`、`cli.TestAgentWorkspace` | 绑定来自 `session.Meta.Workspace`;模型无切换工具 |
| 真实端点回归 | `examples/workspace/real.preset.json`、`platform/bridge.TestJSNativeFSInheritsCallBinding` | 2026-11-21 手工:docs 查询/读取、docs 只读写拒绝、repo 写读、policy 拒绝、workspace_list、shell 托管 cwd | JS 原生 fs/shell 已继承工具 ctx;事件/waterfall 回调仍用事件 ctx(落 `Current`) |

### 2.5 安装、审计与宿主

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| 安装校验/摘要/签名 | `platform/install/` | `install`(31) | Ed25519;签名文件可选 |
| 原子安装/索引/回滚 | `platform/install/` | `install` 用例 | 锁 TTL 10 分钟,TTL 误回收可能 |
| 在线索引/artifact | `platform/install/fetch.go`,`artifact.go` | `install` 用例 | zip 16MiB/1000 文件;artifact 不单独签名 |
| 在线仓库服务端 | `platform/host/registry` | `registry`(7) | 只读;无上传/发布 |
| CLI | `platform/host/cli` | `platform/host/cli`(30) | 参数手写解析;DevX 预算例外 |
| 只读 HTTP API | `platform/host/server` | `platform/host/server`(14) | Token/限流/TLS/CORS 可选 |
| 桌面映射骨架 | `platform/host/desktop` | `desktop`(4) | **无真实窗口(mygo),见 ADR 0001** |
| 审计落盘 | `policy/audit-log` | `audit-log`(7) | 无 fsync;大小轮转 |
| `ada inspect --chain` | `platform/host/cli` | `cli.TestInspectChain`、`cli.TestAgentWorkspace` | 显示 fs/shell/workspace 优先级 |
| 内置栈接线 | `platform/host/builtins` | `builtins.TestBuiltinGoPlugins`、`cli.TestUpSecretsStack` | secrets 已接入宿主;其它 Go 插件仍需编译时注册 |

### 2.6 RPC 通讯(P0–P4 TLS,新增能力)

| 验收项 | 实现位置 | 证据 | 已知边界 |
|---|---|---|---|
| RPC 契约与链 | `plugins/seam/rpc` | `rpc`(3) | Chain/Registry;无 I/O |
| 帧/握手/调用/事件/取消 | `plugins/provider/rpc-json` | `rpc-json`(8) | 无写队列;`rpc-json` 预算例外见 ADR 0009 |
| tcp/unix 传输 | `plugins/provider/rpc-net` | `rpc-net`(6) | unix socket 0600/stale;非 loopback 由宿主自控 |
| Windows 命名管道 | `plugins/provider/rpc-pipe` | `rpc-pipe`(3) | DACL 当前用户+SYSTEM;拒绝远程客户端 |
| 授权(rpc-guard) | `plugins/policy/rpc-guard` | `rpcguard.TestDial`/`TestListen`/`TestListenDenied`/`TestAuthRequiresSecret` | 地址/方法/peer 白名单 + caps `rpc.call`/`rpc.listen`;无 Broker 时只做白名单 |
| 观察(rpc-observe) | `plugins/policy/rpc-observe` | `rpcobserve.TestObserveOutbound`/`TestObserveInbound` | payload 不含 params/result;inbound peer 来自 principal |
| TLS 传输 | `plugins/provider/rpc-tls` | `rpctls.TestTLSRoundTrip`/`TestTLSRPCIntegration` | 需要证书/CA;远程明文 TCP 仍拒绝;`SO_PEERCRED` 待做 |

## 3. 门禁与证据

```powershell
pwsh scripts/guard.ps1     # 词汇/依赖/gofmt/build/vet/staticcheck/race/交叉编译/kernel+host 预算
pwsh scripts/bench.ps1     # 单次调用 ≤ 20 μs、50 插件冷启动 ≤ 150 ms
go test -race -count=1 ./...
go test -race -count=1 ./plugins/seam/rpc ./plugins/provider/rpc-json ./plugins/provider/rpc-net ./plugins/provider/rpc-pipe
go run ./cmd/ada plan --preset examples/agent/agent.preset.json
go run ./cmd/ada inspect --chain --preset examples/agent/agent.preset.json
go run ./cmd/ada plan --preset examples/workspace/workspace.preset.json
go run ./cmd/ada agent --preset examples/workspace/workspace.preset.json --workspace docs -message "查询工作区并读取 a.txt"
```

当前实测:

- 测试 **532 个 Test + 4 个 Benchmark**,`go test -race ./...` 全绿;
- kernel 生产 **2396 / 3000** 行;
- host 生产 **2367 / 2500**(cli 1443 / 1500,其它最大 408 / 800);
- bench:ServiceResolve 58ns、Emit 1.61μs、waterfall 5.55μs、50 插件 348μs;
- 本机默认 `staticcheck 2025.1.1` 与 Go 1.27 export data 不兼容;用 `2026.2.2` 覆盖 PATH 后 guard 全绿(CI 用 `@latest`)。

## 4. 明确未完成项

1. **OS 级沙箱强制**(landlock/seatbelt/Job Object 级 fs/shell):当前只有 policy-only;
2. **真实桌面窗口(mygo)**:只有 Renderer/View/Subscribe/OpenPage 骨架;
3. **M4.5 基础已落地**;剩余摘要质量评估、检查点持久化与 `core/agent` 拆分;
4. **无剩余安全 P0**;M5 剩余 OS 级沙箱与真实桌面窗口(ADR 0001)。

前两项记录在 [ADR 0001](adr/0001-deferred-m5-scope.md),复议日期 2027-01-15;在此之前 **不得宣称 M5 完成**。
