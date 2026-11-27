# ADR 0018:移除 seam/rpc 与 rpc-* 通讯栈

- 状态:**已接受**
- 日期:2026-11-27
- 相关:[seam-rpc.md](../seam-rpc.md)(已改写为移除说明)、[rpc-control-layers.md](../rpc-control-layers.md)、[ADR 0009](0009-rpc-json-budget.md)(本 ADR 撤销其预算例外)、AGENTS §0/§2.2/§5.3/§12
- 复议日期:2027-06-30

## 1. 背景

`seam/rpc` + `provider/rpc-json`/`rpc-net`/`rpc-pipe`/`rpc-stdio`/`rpc-tls` +
`policy/rpc-guard`/`rpc-observe` 是 M5 早期为「桌面宿主 ↔ agent 进程」设计的跨进程控制面:
自研帧格式、challenge/auth 握手、事件订阅、取消、心跳,外加传输与门禁。

宿主侧的演进已经把这条路径整体替换掉:

- ada_chat 已改为 ACP(stdio 上的 JSON-RPC 2.0),删除了 `internal/rpc` 客户端、私有 pipe/token
  端点与 `chat.send`/`chat.approve` 事件面(提交 `9a2d9ef`);
- ada_go 的 RPC 宿主入口 `cmd/ada-rpc-agent` 与控制面适配 `provider/control-rpc` 已删除
  (提交 `400acfc`)。

删除后核查依赖(`go list` 全仓 importers):RPC 栈只剩 `platform/host/builtins` 的注册表与
它们彼此的测试;**没有任何 preset 启用 `rpc-*`,没有任何 cmd/产品路径调用**;`rpc-stdio`
连注册表都没有。也就是说,它是「装得上但没人装」的死代码。

继续保留的代价是真实的:

1. **两套控制面长期并行**:ACP 已经是唯一产品路径,而 RPC 仍占着 seam 契约、7 个内置插件、
   一份 452 行的协议文档与 ADR 0009 的预算例外;评审时每次都要区分「现役/遗留」。
2. **预算与例外失真**:`rpc-json` 的 1300 行例外(ADR 0009)只为这个插件存在;插件没了,
   例外就成了永久豁免的惯性来源。
3. **错误的可扩展性信号**:一个「没有消费者、却有一整套实现」的 seam 会诱导后来者沿用旧协议,
   而不是先证明需求。

## 2. 决策

**整体移除 seam/rpc 与 rpc-* 通讯栈**,并撤销 ADR 0009:

- 删除 `plugins/seam/rpc`、`plugins/provider/rpc-json`、`rpc-net`、`rpc-pipe`、`rpc-stdio`、
  `rpc-tls`、`plugins/policy/rpc-guard`、`plugins/policy/rpc-observe`;
- 从 `platform/host/builtins` 注册表删除对应 7 项(及一致性测试期望);
- `scripts/guard.ps1` 删除 `rpc-json ≤ 1300` 的单插件预算例外;
- `docs/seam-rpc.md` 改写为「已移除 + 理由 + 替代路径」,保留历史入口;
- ADR 0009 标记「已被本 ADR 撤销」,不再作为任何预算依据。

`jsonrpc2` **不删**:它是 ACP 的消息通道(`platform/host/acp`、`provider/control-acp` 依赖),
与本次删除的「自研 RPC v1 帧协议」无关。

## 3. 备选方案

| 方案 | 做法 | 否决理由 |
|---|---|---|
| **A. 保留契约、移出 builtins** | 删实现,只留 `seam/rpc` 契约供第三方宿主 import | 契约没有实现、没有消费者,仍要被文档与预算覆盖;留下的是「半个 seam」,比干净删除更难判断能否依赖 |
| **B. 保留 rpc-json 协议层、只删传输与门禁** | 留帧/握手/调用,删 net/pipe/tls/guard/observe | 协议离开传输不可用,等于保留一份不能运行的实现;`rpc-guard` 的授权还是唯一安全边界,删掉后协议层反而更不可用 |
| **C. 整体删除(本决策)** | 8 个包 + 注册项 + 预算例外 + 文档入口 | 采用:没有任何产品路径依赖;删除后 ACP 是唯一控制面,契约面收敛 |
| **D. 维持现状** | 全部保留,标注 deprecated | 保留 8 个包 + 1 份 452 行文档 + 1 条预算例外,持续制造「两套控制面」的评审噪声;不作为默认选项 |

## 4. 影响面

- **代码**:删除 8 个插件包,生产约 2779 行、测试约 1405 行、7 份 `plugin.json`;
  `builtins.Plugins()` 少 7 项;`rpc-json` 的预算例外从 guard 移除。
- **契约**:`seam/rpc` 连同它的地址/帧/对端/传输类型一起删除。平台**不再提供跨进程通讯 seam**;
  `platform/host/server`、`registry` 的 HTTP 宿主不受影响。
- **文档/ADR**:`seam-rpc.md` 改为移除说明;`rpc-control-layers.md` 去掉 RPC 层;
  `milestone-status.md`、`host-cli.md`、`packaging.md`、`caps.md`、`seam-approval.md`、
  `AGENTS.md` 产物表与预算表同步;ADR 0009 撤销。
- **产品行为**:无变化——没有任何 preset 或宿主路径启用这些插件;`ada acp` /
  `ada agent` / `ada up` 的行为与删除前一致(删除前它们也不会装 `rpc-*`)。
- **未来加通讯端口**:需要新的 seam + ADR,而不是复活旧协议。

## 5. 回滚

- 单个提交可完整回滚:`git revert <commit>` 恢复 8 个包、注册项、预算例外与文档原文。
- 契约文本仍可在 git 历史与本文引用中找到,不依赖线上备份。
- 若只回滚部分内容(例如只恢复 `rpc-json`),必须同时恢复其传输与 guard,否则装配期即不可用;
  因此回滚粒度应当是整个 ADR。

## 6. 复议

2027-06-30 前重新评估一次:若出现**具体的**非 ACP 宿主接入需求(有产品、有契约、有测试),
应写新 ADR 设计该通讯 seam,并说明与 ACP 的边界;不得直接复活 `rpc-json` 的帧格式。
