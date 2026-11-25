# ada_go_v2

**一个以插件为核心的程序基座**：Go 内核 + TypeScript 插件（纯 Go 编译与执行，无需 Node 环境）+ Cordis 式的作用域与编排。

> 当前处于 **M4.5 Agent Core 第一刀(`prompts`/`llm`/`tools`/`agent-loop` + 最小 preset)+ M5 安全与宿主(`fs`/`shell`/`sandbox`/`approval`/`caps`/`net`/`secrets`/`install`/CLI/server/registry)**:M1–M4 已落地;M4.5 已跑通「用户消息 → 模型 → 工具 → 结果」的最小回路;M5 剩余 OS 级沙箱与真实桌面窗口(ADR 0001)。**不做 v1 插件迁移,不提供 `ada migrate`。**
> **工作约定/红线**：[AGENTS.md](AGENTS.md)（动代码或改设计前先读）
> 设计文档：[docs/plugin-kernel-design.md](docs/plugin-kernel-design.md)
> seam 契约：[docs/seam-api.md](docs/seam-api.md)（llm / tools / session / prompts）

## 目录结构

```
platform/              插件编排系统(作为一个整体)
  kernel/              L1 内核(含 schema)
  assemble/ plugin/ loader/ preset/ bundle/   装配与组合
  bridge/                                     执行层桥(compile/jsrt 在独立库 ada_jsrt)
  sched/ quota/ flow/ expr/                   编排层
  caps/ install/ version/                     授权、安装、版本
  host/                CLI / server / desktop / registry / builtins
plugins/               业务插件
  seam/ provider/ policy/ core/ tool/
cmd/                   入口(ada / ada-demo / ada-ts-demo)
docs/ examples/ scripts/
```

`platform/kernel` 不 import `plugins/`;`plugins/` 只能 import `platform/` 的 seam/内核契约;
`platform/host/builtins` 是唯一反向依赖(内置插件注册表);`cmd/` 负责装配入口。

执行层的 `compile`(esbuild 降级)与 `jsrt`(moejs 宿主)已独立为同级库 **ada_jsrt**(模块 `github.com/issueye/ada_jsrt`);本仓库用 `go.mod` 的 `replace` 指向 `../ada_jsrt`,发布前需把该库发布到模块代理并删除 `replace`(CI 需先把该库 checkout 到同级目录,或按真实版本号拉取)。

## 这套方案要解决什么

ada_go v1 已经证明了「清单 + 每插件一个 moejs 运行时 + 能力桥 + 装配事务」是可行的，但它是**功能化**的：
装配顺序靠声明顺序、没有服务依赖、能力声明退化成词汇、钩子零使用、改一个插件要重建整代装配。

v2 把「插件」从**被调用的函数袋**升级成**可编排的作用域节点**：

| 抽象 | 解决 | 参照 |
|---|---|---|
| Context / Fiber | 作用域隔离、可嵌套派生、可等待的插件句柄 | Cordis `ctx` / `fiber` |
| Service | 插件间协作与依赖注入，依赖变化自动重载 | Cordis `provide/inject/epoch` |
| Effect | 一切注册可撤销，卸载逆序、依赖方先走、失败回滚 | Cordis `ctx.effect` |
| Event | 五种分发；`waterfall` 作为策略拦截的通用形态 | Cordis `emit/serial/bail/waterfall` |
| Config | JSON Schema 子集 + 分层 patch + 热更新 | Cordis Schema + DSH loader |
| **seam / provider / policy** | 平台能力三段式：接口、实现、策略各自可换 | DSH 的 `dsh-llm` / `dsh-fs-local` / `dsh-llm-retry` |
| **Agent Core 即插件** | agent、agent-loop、session、system-prompt、compaction 都是插件，由 preset 组合；产品差异不改代码 | `dsh-agent-loop`、`dsh-session`、`dsh-agent-preset` |
| 编排 | 装配（DAG + 事务）、交互（服务/事件/流水线）、调度（激活/配额/回收） | 本方案新增 |

## 核心决策（8 条）

1. **内核是 Go 的，且只有内核不是插件**：插件树、服务表、副作用栈、事件、配置在内核；平台能力（llm/tools/fs/shell/storage…）与 **agent core**（agent / agent-loop / session / system-prompt / compaction…）都是插件，seam + provider + policy 三段式；产品差异只由 **preset** 声明决定。
2. **esbuild 只做 TS → JS 降级**：`api.Transform` 逐文件、内存态、纯 Go、无 Node；补上 moejs `CompileTS` 不支持的语法（enum/namespace/参数属性/装饰器/TSX/CJS 形态）。
3. **moejs 执行，边界写死**：每插件一 Runtime 一 goroutine；`*Module` 进程内共享；字节码不可落盘；跨边界只走 JSON；环境能力默认全关。
4. **能力 ≠ 服务**：能力是授权（调用点强制 + 审计），服务是协作接口（`inject` 决定可见性）。
5. **发布物不带 esbuild**：编译发生在安装期/开发期，产物是 `plugin.artifact.json`（JS + sourcemap + 摘要 + 签名）。
6. **编排分三层**：装配编排产出依赖图与事务报告；交互编排产出服务调用、事件链与可声明流水线；调度编排产出激活事件、配额与闲置回收。
7. **mygo 是宿主之一**：内核不 import mygo；桌面适配器负责窗口、协议、类型化事件与主线程切换。
8. **v1 只作参照，不做迁移**：v1 的经验与反例用于设计；不提供 `ada migrate`，不移植 v1 官方插件。

## 文档结构

| 文档 | 内容 |
|---|---|
| [plugin-kernel-design.md](docs/plugin-kernel-design.md) §1–§2 | 背景、v1 复盘、Cordis 取舍、目标与非目标、8 条硬约束 |
| §3 | 分层、进程/线程模型、调用链路 |
| §4–§8 | Context/Fiber、Service、Effect、Event、Config（含 Go API 与语义表） |
| §9–§11 | 插件形态与清单 v2、esbuild 编译管线与 Artifact、moejs 运行时与桥 |
| §12 | **编排**：装配 / 交互（流水线）/ 调度 / 组合 patch / **Agent Core 的 preset 组合（§12.6）** |
| §13–§15 | 安全与信任、可观测与 CLI、mygo 集成、性能与体积（含实测数） |
| §16–§18 | 目录与骨架代码、里程碑（19–21 周）、风险 |
| 附录 A–E | Cordis→Go 对照、错误码、v1→v2 迁移映射、12 个 spike、待决问题 |
| [seam-api.md](docs/seam-api.md) | **首批 seam 契约**：llm / tools / session / prompts 的 Go 接口、事件、管线、不变量与契约测试 |
| [m4.5-agent-core.md](docs/m4.5-agent-core.md) | **M4.5 第一刀**:prompts/llm/tools/agent-loop 的实现、最小 preset、golden 与已知边界 |
| [seam-api-security.md](docs/seam-api-security.md) | **安全 seam 契约**：sandbox / fs / shell 的三道闸、虚拟路径、装饰器强制链、拒绝矩阵 |
| [kernel-api.md](docs/kernel-api.md) | M1 冻结的内核 API 与实现边界 |
| [m2-execution.md](docs/m2-execution.md) | M2 执行层第一刀:compile / jsrt(已迁至独立库 ada_jsrt)/ bridge 的契约、测试矩阵与已知边界 |
| [event-loop.md](docs/event-loop.md) | **执行层事件循环**:调用边界驱动、可 await 的东西、定时器作用域与已知边界 |
| [m3-assembly.md](docs/m3-assembly.md) | M3 装配层:清单、依赖图、preset、事务化装配与 `ada plan`/`up` |
| [m4-orchestration.md](docs/m4-orchestration.md) | M4 编排:sched 与 quota |
| [m4-flow.md](docs/m4-flow.md) | M4 流水线:声明式 flow、受限表达式、checkpoint/resume |
| [m4-composition.md](docs/m4-composition.md) | M4 组合层:bundle / profile(整体替换与补丁) |
| [version.md](docs/version.md) | 版本与引擎约束:semver 解析与 engine/engines 校验 |
| [host-cli.md](docs/host-cli.md) | M5 宿主适配器第一刀:platform/host/cli 命令实现与测试 |
| [host-server.md](docs/host-server.md) | 第二宿主适配器:platform/host/server 只读 HTTP API |
| [host-desktop.md](docs/host-desktop.md) | 宿主适配器骨架:platform/host/desktop 视图映射(WebView 壳见 ADR 0008 提案;当前仍是 native 骨架) |
| [host-registry.md](docs/host-registry.md) | 在线仓库服务端:platform/host/registry 只读 HTTP |
| [audit-log.md](docs/audit-log.md) | M5 审计落盘:policy/audit-log JSONL |
| [tool-git-helper.md](docs/tool-git-helper.md) | 脚本插件示例:TS git-helper + 注入 ada:shell + ctx.tool |
| [tool-task-helper.md](docs/tool-task-helper.md) | 脚本插件示例:TS task-helper + 注入 ada:fs |
| [tool-command-helper.md](docs/tool-command-helper.md) | 脚本插件示例:TS command-helper + 注入 ada:shell |
| [tool-text-helper.md](docs/tool-text-helper.md) | 唯一文件工具:text-helper(TS)+ ada:fs 读写/编辑/搜索/列目录 |
| [tool-shell-helper.md](docs/tool-shell-helper.md) | TS 工具插件:shell-helper + ada:shell.exec(含 moejs/bridge 扩展) |
| [tool-http-helper.md](docs/tool-http-helper.md) | TS 工具插件:http-helper(http_request + http_search)+ node:http/node:zlib/ada:secrets |
| [tool-plugin-helper.md](docs/tool-plugin-helper.md) | TS 工具插件:plugin-helper(清单体检 / 生成 TS 插件骨架 / 改 preset) |
| [tool-skill-helper.md](docs/tool-skill-helper.md) | TS 工具插件:skill-helper(技能清单 / 读取 SKILL.md / 生成技能目录) |
| [tool-memory-helper.md](docs/tool-memory-helper.md) | TS 长期记忆:memory-helper + ada:fs/node:crypto/Intl.Segmenter |
| [tool-tool-helper.md](docs/tool-tool-helper.md) | Go 工具插件:tool-helper 的 tool_find / tool_exec |
| [node-compat.md](docs/node-compat.md) | Node 内置模块兼容层:node:fs/path/os/events/util/.../crypto |
| [env-file.md](docs/env-file.md) | 环境变量文件(.env):加载位置、优先级、语法与安全边界 |
| [adr/0001](docs/adr/0001-deferred-m5-scope.md) | ADR:延迟 OS 级沙箱与真实桌面窗口(复议 2027-01-15) |
| [adr/0002](docs/adr/0002-host-budget-exception.md) | ADR:host 层代码预算例外(CLI 适配器) |
| [adr/0003](docs/adr/0003-handler-context-and-timeout-gate.md) | ADR:监听器 ctx 与 waterfall 超时闸门 |
| [adr/0004](docs/adr/0004-execution-layer-deps.md) | ADR:执行层引入 esbuild 与 moejs |
| [adr/0005](docs/adr/0005-ui-infrastructure.md) | ADR(提案):UI 编译器与浏览器运行时归入 L2 基础设施 |
| [adr/0006](docs/adr/0006-custom-ui-components.md) | ADR(提案):受控自定义 UI 组件(反转「插件不画 UI」) |
| [adr/0007](docs/adr/0007-host-web-budget.md) | ADR(提案):platform/host/web 与 host 总行数预算例外 |
| [adr/0008](docs/adr/0008-desktop-webview-shell.md) | ADR(提案):桌面只做 WebView 壳(mygo WebView2/Electron)，不做 native UI |
| [adr/0009](docs/adr/0009-rpc-json-budget.md) | ADR:rpc-json 单插件预算例外(已接受) |
| [adr/0010](docs/adr/0010-session-jsonl-budget.md) | ADR:session-jsonl 单插件预算例外(已接受) |
| [adr/0013](docs/adr/0013-tool-plugin-budget-exception.md) | ADR:memory-helper / http-helper 预算例外(复议 2027-06-30) |
| [adr/0014](docs/adr/0014-call-scoped-event-loop.md) | ADR:执行层事件循环按调用边界驱动(已修订 2026-11-26,新增 LoopResident;复议 2027-03-31) |
| [adr/0015](docs/adr/0015-refused-node-modules.md) | ADR:不实现 worker_threads / inspector / async_hooks / trace_events(复议 2027-06-30) |
| [adr/0016](docs/adr/0016-no-fs-watch.md) | ADR:fs.watch/watchFile 的实现边界(已修订 2026-11-26,LoopResident 下轮询;复议 2027-06-30) |
| [seam-rpc.md](docs/seam-rpc.md) | RPC 通讯:P0–P4(TLS) 已实现(tcp/unix/pipe、帧/握手/取消、rpc-guard/observe、tls://) |
| [node-compat.md](docs/node-compat.md) | Node 内置模块兼容层:node:fs/path/os/events/util 等 |
| [seam-secrets.md](docs/seam-secrets.md) | M5 secret 能力:seam/secrets 与 provider/secrets-env |
| [seam-session.md](docs/seam-session.md) | seam/session 与 session-memory |
| [seam-fs.md](docs/seam-fs.md) | M5:seam/fs、fs-local 与 fs-sandbox |
| [workspace-plugin.md](docs/workspace-plugin.md) | 工作区:seam/workspace、workspace-local、workspace-guard、workspace-helper、会话绑定与 fs/shell 托管解析 |
| [seam-shell.md](docs/seam-shell.md) | M5:seam/shell、shell-local 与 shell-guard |
| [seam-sandbox.md](docs/seam-sandbox.md) | M5:seam/sandbox 与 sandbox-none |
| [seam-approval.md](docs/seam-approval.md) | M5:seam/approval、approval-deny 与升级审批 |
| [caps.md](docs/caps.md) | M5 能力授权:caps 语法、信任等级与 Broker |
| [install.md](docs/install.md) | M5 安装与签名:清单/摘要/Ed25519/原子安装 |
| [packaging.md](docs/packaging.md) | 编译与发布打包:builtins 自动导出清单、发布 preset 与包内自检 |
| [ui-framework.md](docs/ui-framework.md) | 提案:无 Node 的插件化 SPA 前端框架（组件/组件包即一项功能 / Go 编译 / 远程加载 / 信任模型） |
| [spikes.md](docs/spikes.md) | M0 验证报告（S1/S2/S6/S8/S9 通过） |

## 实测基线（Go 1.27.1 / windows/amd64 / CGO=0）

| 二进制 | 未 strip | strip |
|---|---|---|
| hello | 1.85 MB | 1.22 MB |
| + moejs | 12.32 MB | 8.63 MB |
| + esbuild | 11.65 MB | 8.33 MB |
| 两者同进程 | ≈ 22 MB | ≈ 16 MB |

`Transform` 单文件 0.77–0.88 ms；moejs 新建 Runtime 1.39 µs、加载插件 71 µs、单次调用 6.86 µs、每 Runtime 常驻 80.5 KiB。接入事件循环后 `BenchmarkCallSync` 实测约 4.5 µs/次(同步入口,**含**循环 enter/exit 与引导模块回调),仍满足 ≤20 µs 的调用预算。

## 当前状态:M1 内核 + M2 执行层 + M3 装配 + M4 编排 + M4.5 Agent Core 第一刀 + M5 安全与宿主

```powershell
go test -race -count=1 -timeout 300s ./...   # 本仓库全量 Test + Benchmark(执行层测试在 ../ada_jsrt)
(cd ../ada_jsrt; go test ./jsrt -run 'Await|Timer|Loop')  # 事件循环:await / fs.promises / 定时器作用域
go run ./cmd/ada-demo                        # 依赖门控 + 级联重载演示
pwsh scripts/guard.ps1                       # 门禁:词汇/依赖/gofmt/build/vet/staticcheck/race/交叉编译/预算
pwsh scripts/build.ps1                       # 编译入口到 bin/(-GOOS/-GOARCH 可交叉编译)
pwsh scripts/package.ps1 -Version 0.1.0      # 编译+内置/示例插件清单+发布 preset,产出 dist/
go run ./cmd/ada-ts-demo                     # M2:TS 插件编译→加载→提供服务/派发事件→内核消费
go run ./cmd/ada plan examples/plugins       # M3:读清单→依赖图→装配顺序/问题报告
go run ./cmd/ada up --preset examples/git-ts/git.preset.json  # M3:现场编译 TS 插件并真的挂载一遍
go run ./cmd/ada plan --preset examples/agent/agent.preset.json   # M4.5:最小 agent preset
go run ./cmd/ada up --preset examples/agent/agent.preset.json     # M4.5:挂载 agent 栈(一次性)
go run ./cmd/ada inspect --chain --preset examples/agent/agent.preset.json  # M5:装饰器链快照
go run ./cmd/ada up --preset examples/agent/real.preset.json               # M4.5:真实 OpenAI 兼容端点(需 env)
go run ./cmd/ada agent --preset examples/agent/agent.preset.json -message "读取 a.txt 并总结"  # M4.5:跑一轮
go run ./cmd/ada up --preset examples/secrets/secrets.preset.json # M5:secrets 栈(seam-secrets+secrets-env)
go run ./cmd/ada agent --preset examples/git-ts/git.preset.json -message "看看 git 状态" # 脚本插件 + 注入模块
go run ./cmd/ada agent --preset examples/task-ts/task.preset.json -message "添加任务并列出" # 脚本插件 + ada:fs
go run ./cmd/ada agent --preset examples/command-ts/command.preset.json -message "执行 git --version" # 脚本插件 + 受控命令
go run ./cmd/ada agent --preset examples/shell-helper-ts/shell.preset.json -message "执行命令" # TS 工具插件 + ada:shell.exec
go run ./cmd/ada up --preset examples/http/http.preset.json # TS 工具插件 + node:http/node:zlib(仅装配示例)
go run ./cmd/ada up --preset examples/plugin-helper/plugin.preset.json # TS 插件开发工具(生成/体检/改 preset)
go run ./cmd/ada up --preset examples/skill/skill.preset.json # 技能工具(SKILL.md 清单/读取/生成)
go run ./cmd/ada agent --preset examples/text-ts/text.preset.json -message "写文件、替换并搜索" # 脚本插件 + 文本工具
go run ./cmd/ada agent --preset examples/workspace/workspace.preset.json --workspace docs -message "查询工作区并读取 a.txt" # 工作区绑定 + 只读 docs
go run ./cmd/ada agent --preset examples/workspace/real.preset.json --workspace docs -message "查询工作区并读取 a.txt" # 工作区真实端点(需 ADA_LLM_API_KEY)
go run ./cmd/ada plan --preset examples/presets/base.preset.json   # M3:preset(disabled/patch)
go run ./cmd/ada inspect --preset examples/git-ts/git.preset.json  # M3:诊断快照(fiber/服务/effect)
go run ./cmd/ada verify examples/git-ts/plugins/git-helper  # M5:清单/摘要/验签/信任
go run ./cmd/ada list -dest <安装根>                 # M5:本机安装索引
go run ./cmd/ada uninstall <id> -dest <安装根>        # M5:卸载全部版本
go run ./cmd/ada upgrade <目录> -dest <安装根>         # M5:升级并清理旧版本
go run ./cmd/ada rollback <id> -dest <安装根>          # M5:回滚到上一版本
go run ./cmd/ada update <id> -repo <仓库> -dest <安装根>  # M5:离线仓库更新
go run ./cmd/ada caps examples/plugins -json          # M5:能力自检(caps/trust → Broker)
go run ./cmd/ada check examples/text-ts/plugins      # M5:清单/计划/platform/caps/编译一键校验
pwsh scripts/bench.ps1                       # 性能预算:≤20 μs/次、50 插件冷启动 ≤150 ms
```

Node 内置模块:`node:fs`/`node:fs/promises`/`node:buffer`(moejs `node` 包,`node:fs` 另由宿主补齐 `mkdtemp`/`symlink`/`link`/`chmod`/`chown`/`truncate`/`utimes`)与 `node:path`/`node:os`/`node:events`/`node:util`/`node:assert`/`node:url`/`node:querystring`/`node:string_decoder`/`node:process`/`node:child_process`/`node:http`/`node:https`/`node:dns`/`node:dns/promises`/`node:net`/`node:crypto`/`node:zlib`/`node:timers`/`node:timers/promises`/`node:stream`/`node:stream/promises`/`node:readline`/`node:readline/promises`/`node:tty`/`node:perf_hooks`(宿主 embed);`node:fs` 在 fs/workspace 有策略装饰器时不可用,避免绕过安全链;网络模块直连标准库,`seam/net` 与 `net-guard` 已移除(见 [ADR 0011](docs/adr/0011-node-network-direct.md)、[ADR 0012](docs/adr/0012-remove-seam-net.md));`worker_threads`/`inspector`/`async_hooks`/`trace_events` 不实现(见 [ADR 0015](docs/adr/0015-refused-node-modules.md))。见 [node-compat.md](docs/node-compat.md)。实现位于独立库 `ada_jsrt` 的 `jsrt` 包。

执行层有事件循环([event-loop.md](docs/event-loop.md)):插件可以 `await fs.promises.*`、`await new Promise(r => setTimeout(r, n))`,`async apply` 与 async 工具处理器都会在调用返回前结算;跨调用的后台定时器不支持(**调用边界驱动**,见 [ADR 0014](docs/adr/0014-call-scoped-event-loop.md))。

`node:fs` 另由宿主补齐了 `mkdtemp`/`symlink`/`link`/`chmod`/`chown`/`truncate`/`utimes` 的 sync / callback / promise 三形态(路径夹在 `/workspace`、写权限沿用 `Write` 开关,见 [node-compat.md](docs/node-compat.md) §3.1)。

已完成:`platform/kernel/`、`platform/kernel/schema`、`cmd/ada`、`cmd/ada-demo`、`cmd/ada-ts-demo`、`platform/bridge/`、独立库 `ada_jsrt`(compile/jsrt)、`platform/plugin/`、`platform/preset/`、`platform/assemble/`(含延迟激活)、`platform/loader/`、`platform/sched/`、`platform/quota/`、`platform/flow/`、`platform/bundle/`、`platform/expr/`、`plugins/seam/session` + `provider/session-memory`、`plugins/seam/prompts` + `core/system-prompt`、`plugins/seam/llm` + `provider/llm-replay`/`provider/llm-openai`、`plugins/seam/tools` + `tool/text-helper`/`tool/shell-helper`/`tool/memory-helper`/`tool/http-helper`(TS) + `tool/tool-helper`(Go)、`policy/approval-gate`/`policy/llm-retry`/`policy/tool-timeout`、`core/agent-loop`、`examples/agent`(最小 preset + 回放脚本)、`plugins/seam/fs` + `provider/fs-local` + `policy/fs-sandbox`、`plugins/seam/shell` + `provider/shell-local` + `policy/shell-guard`、`plugins/policy/fs-redact`/`fs-observe`/`shell-observe`/`shell-redact`、`plugins/seam/sandbox` + `provider/sandbox-none`、`plugins/seam/approval` + `provider/approval-deny`/`approval-cli`、`plugins/seam/workspace` + `provider/workspace-local` + `policy/workspace-guard` + `workspace-helper`、`examples/workspace`、`platform/caps/`、`platform/install/`、`platform/host/cli`、`platform/host/server`、`platform/host/builtins`、`platform/host/desktop`(视图映射骨架)、`platform/host/registry`、`scripts/build.ps1`、`scripts/package.ps1`、`scripts/tools/builtins-dump`、`scripts/guard.ps1`、`scripts/bench.ps1`、`docs/m4.5-agent-core.md`、`docs/kernel-api.md`、`docs/m2-execution.md`、`docs/m3-assembly.md`、`docs/m4-orchestration.md`、`docs/m4-flow.md`、`docs/seam-session.md`、`docs/seam-fs.md`、`docs/workspace-plugin.md`、`docs/packaging.md`、`docs/seam-shell.md`、`docs/seam-sandbox.md`、`docs/seam-approval.md`、`docs/adr/0001-deferred-m5-scope.md`、`docs/adr/0002-host-budget-exception.md`。

剩余:M4.5 的 token 计数、投影检查点持久化、`core/agent` 拆分与摘要质量评估;M5 的 OS 级沙箱与真实桌面窗口(ADR 0001);RPC 的 `SO_PEERCRED`/宿主接入;UI 框架仍为提案。**不做 v1 插件迁移。**

本轮修复了 M1 评审中复现的缺陷(waterfall 超时幽灵执行、兄弟插件被误拆、failed 不可复活、卸载错误被吞、fiber 注册表泄漏、Intercept 合并反向、Stop 后仍派发、schema 静默放行),每条都有回归用例;清单见 [kernel-api.md §6](docs/kernel-api.md)。

## 下一步

1. M4.5:`session-jsonl` + projection + `policy/tool-timeout`;真实模型、重试与审批已落地。
2. M4.5:并行工具、compaction/上下文预算、`core/agent` 拆分。
3. M5:C13 拒绝矩阵与 `inspect --chain` 已落地;剩余 OS 级沙箱与真实桌面窗口按 ADR 0001 复议。
4. 文档与示例继续跟随代码;示例必须能在 CI 装配(golden/preset)。
