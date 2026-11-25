# ADR 0002:host 层代码预算例外(CLI 适配器)

- 状态:已接受(临时例外)
- 日期:2026-11-20
- 复议日期:2027-01-15
- 相关:AGENTS §5.3、[host-cli.md](../host-cli.md)、[host-server.md](../host-server.md)、[host-registry.md](../host-registry.md)、[host-desktop.md](../host-desktop.md)

## 1. 背景

AGENTS §5.3 规定 `host/*` ≤ 1500 行、单适配器 ≤ 800 行。当前实测(生产代码):

| 适配器 | 行数 |
|---|---|
| `host/cli`(含 check/serve/serve-repo/agent 等命令) | 1410 |
| `host/server` | 404 |
| `host/registry` | 232 |
| `host/desktop` | 120 |
| `host/builtins`(共享注册表) | 120 |
| **合计** | **2320** |

超出预算的原因:

- CLI 是 DevX 工具,承载了 plan/up/inspect/verify/install/update/repo/check/serve 等十几个命令的参数解析与输出;
- 所有命令必须可测试,因此参数解析与 writer 注入都留在 Go 里;
- 拆分为 `host/cli/commands` 子包会引入跨包共享 helper(parseTarget/loadTarget/consent),短期内是高风险重构。

预算的初衷是「宿主只做映射」;CLI 的多数行数是**参数解析与文本输出**,仍属于映射,不是业务。

## 2. 决策

1. 临时放宽 `host/*` 总预算到 **2500 行**;`host/cli` 单适配器 **1500 行**;其它适配器仍 **≤800**。
2. 在 `scripts/guard.ps1` 中**实际检查** host 预算(总、cli、其它最大值),不依赖口头约定。
3. 复议日期前把 CLI 命令实现拆到 `host/cli/commands`(或改用 `flag` 包压缩),然后回滚本例外。

## 3. 替代方案(至少两个)

| 方案 | 做法 | 否决/暂缓理由 |
|---|---|---|
| A. 立即拆分 `host/cli/commands` | 把命令实现移入子包,`cli.go` 只做 dispatch | 需要导出/下沉 parseTarget/loadTarget/consent;拆完适配器总量仍 2000,只是分布变化 |
| B. 用 `flag` 包重写参数解析 | 去掉手写 switch,压缩 CLI 行数 | 改造量中等,但会改变输出/错误消息;可作复议期方案 |
| C. 不把 host 预算纳入 CI | 维持文档约定 | 预算变成口号,已经超了却无人发现,不可接受 |
| D. 把 CLI 从 host 移到 cmd/ | `cmd/ada` 直接实现命令 | 违反「宿主只做映射」的目录约定,且无法复用 writer 注入测试 |

## 4. 影响面

- 预算检查会覆盖 host 各适配器;当前数字通过(2320 ≤ 2500,1410 ≤ 1500)。
- 不改任何运行时行为。
- AGENTS §5.3 表格需要标注本例外并引用本 ADR。

## 5. 回滚方式

拆分 CLI 后删除本 ADR 并把 guard 阈值改回 1500/800;无数据迁移。

## 6. 复议条件

- `host/cli` 拆分完成(命令包 + 共享 helper);
- 或 CLI 参数解析改用标准库后行数降到 800 以内;
- 或产品决定 CLI 不属于产品宿主、从 host 移到独立的 dev 工具目录(需同步改 AGENTS §2.2)。
