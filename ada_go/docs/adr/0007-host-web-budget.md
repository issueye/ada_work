# ADR 0007:host/web 与 host 总行数预算例外

- 状态:提案(待评审;通过后同步 AGENTS §5.3 与 `scripts/guard.ps1`)
- 日期:2026-11-20
- 复议日期:2027-06-30
- 相关:[ui-framework.md](../ui-framework.md) §11/§15、[host-server.md](../host-server.md)、[ADR 0002](0002-host-budget-exception.md)、AGENTS §5.3/§12

## 1. 背景

AGENTS §5.3 规定 `host/*` 总计 ≤ 2500 行、每个适配器 ≤ 800 行。现状:`host/*` 约 2163 行(milestone-status 记录),CLI 已有 ADR 0002 临时例外。

新的 `host/web` 要承载 bootstrap、同源资产、深链回退、动作 RPC、SSE、确认对话框、认证与内容寻址资产库。按设计估算 600–800 行;加上现状就会突破 2500 总预算,`guard.ps1` 会直接失败。把 `host/web` 塞进 `host/server` 会让只读 API 适配器变成有状态 SPA 宿主，职责更糟;把 SPA 运行时放进 L2 又会把「协议映射」和「浏览器执行」混在一起(见 ADR 0005)。

## 2. 决策

1. `host/*` 总预算从 2500 提高到 **3600 行**，作为临时例外，复议日期 2027-06-30。
2. 单个适配器上限从 800 提高到 **900 行**;`host/web` 目标 ≤ 800 行，`host/server`/`host/cli` 不变(`host/cli` 继续用 ADR 0002 的 1500 例外)。
3. `host/web` 内部拆包:`server.go`(路由/静态/SSE)、`bootstrap.go`(UI Plan)、`action.go`(动作/确认)、`assets.go`(内容寻址资产库);任一子文件 > 400 行考虑再拆。新增 `ada web`/`ada ui` 子命令的参数解析与 help 放 `cmd/ada`(≤200 行)，不占 `host/cli` 的 1500 例外。
4. `host/web` 仍然只做协议/同源资产/认证映射，不做编译、不做 UI 语义、不做业务;编译在 `ui/`。
5. `scripts/guard.ps1` 增加:host 总行数 3600、`ui/` 总行数 5000、运行时内核体积 35 KiB、`ui/` 内禁止 `eval`/`new Function` 文本。CI 三平台同跑。

## 3. 替代方案(至少两个)

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 并入 `host/server` | 在只读 API 适配器里加 SPA 资产/动作/SSE | 破坏 host/server「只读」语义;状态与长驻生命周期混入;更难测 |
| B. 把 host/web 放进 L2 | 以「基础设施」名义绕开 host 预算 | 它是平台协议映射，不是装载机制;会让 L2 依赖 HTTP/认证/会话 |
| C. 砍功能 | host/web 只做静态资产，不做动作/SSE/资产库 | 无法满足运行时安装与增量装配;退回「重启才生效」 |
| D. 本 ADR 例外 | 提高总预算，拆包，加 guard | 可验收;代价是 host 层变大，需在复议前收回去 |

## 4. 影响面

- AGENTS §5.3:host 总预算 2500 → 3600，适配器 800 → 900(临时)。
- `guard.ps1`:新增行数与体积门禁;CI 会在三平台跑。
- 设计:`host/web` 的职责边界不变，只是允许它存在;`host/*` 仍不 import `ui/` 的编译逻辑。
- 风险:host 层继续膨胀;复议时必须给出收敛方案(拆包、合并或下沉)。

## 5. 回滚方式

删除 `host/web` 及本 ADR，恢复 2500/800 预算;SPA 不发布，CLI/server/desktop 不受影响。

## 6. 复议条件

- `host/*` 实际行数连续两个里程碑超过 3200，且没有拆分计划 → 收回例外。
- `host/web` 出现业务判断(会话、模型、工具编排)→ 立即回滚并把逻辑移回插件。
- 桌面宿主(ADR 0001 复议)落地后，`host/web` 与 `host/desktop` 能共享协议层 → 重新评估预算。
