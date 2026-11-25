# ADR 0001:延迟 M5 的 OS 级沙箱与真实桌面窗口

- 状态:已接受(范围例外,非永久豁免)
- 日期:2026-11-20
- 复议日期:2027-01-15
- 相关:[plugin-kernel-design.md](../plugin-kernel-design.md) §12/§17、[seam-sandbox.md](../seam-sandbox.md)、[host-desktop.md](../host-desktop.md)、AGENTS §10

## 1. 背景

M5 的验收口径包含「OS 级沙箱后端」与「desktop 宿主(mygo)」。当前仓库状态:

- `plugins/provider/sandbox-none` 是 **policy-only**(`Enforced=false`),`fs-sandbox`/`shell-guard` 的执行约束由策略与审批保证,不是操作系统强制;
- `host/desktop` 只有 **Renderer/View 映射骨架**,没有真实窗口、菜单、主线程;
- 环境与依赖限制:
  1. `golang.org/x/sys` 固定在 2022 年版本,不包含 landlock 包装;使用原始 syscall 需要重新验证内核版本与三平台交叉编译;
  2. mygo 是新的第三方 UI 依赖,按 AGENTS §12 需要新依赖 ADR,并带来平台构建与测试成本;
  3. CI 只做三平台交叉编译与单元/契约测试,没有 Linux 5.13+/macOS/Windows 的真实 OS 沙箱验收环境。

继续在本轮内实现这两项会引入**不可验证的强制语义**(宣称 Enforced=true 但无法测试)或**新依赖**,违背「没有契约测试与文档更新的能力不算完成」。

## 2. 决策

1. **OS 级沙箱**:本轮不实现 landlock/seatbelt/Job Object 的 fs/shell 强制;`sandbox-none` 继续如实标记 `Kind: "policy-only"`、`Enforced=false`。
2. **desktop**:不引入 mygo;保留 `host/desktop` 的 Renderer/View/Subscribe/OpenPage 骨架,明确「不是完整桌面宿主」。
3. **状态表述**:README/AGENTS 继续把这两项列为 **M5 剩余**,不声称 M5 完成。
4. 复议日期前若补齐依赖/测试环境,按新 ADR 立项实现;否则再次复议。

## 3. 替代方案(至少两个)

| 方案 | 做法 | 否决/暂缓理由 |
|---|---|---|
| A. 升级 x/sys + 原始 landlock syscall | Linux-only `sandbox-landlock`,ruleset + path-beneath,`Enforced=true` | 需要内核 5.13+ 的运行时测试与 seccomp 兜底;当前 CI 无该环境,容易变成「写了但没验证」 |
| B. seccomp/chroot/命名空间 | 用 `unix.Chroot`/seccomp 限制 fs/shell | 需要 root 或特权;跨平台不可用;会改变整个进程的语义,与「按会话约束」契约不符 |
| C. 直接引入 mygo 实现窗口 | 新第三方依赖 + 窗口/菜单/主线程映射 | 触发新依赖 ADR;需要桌面平台构建矩阵;与本轮目标(安全能力)无直接关系 |
| D. 纯 Web UI(host/server + 浏览器) | 用已有 host/server + 前端页面替代桌面窗口 | 可作为 desktop 的替代交付,但不是 mygo 桌面宿主;需要前端工程与打包 |

## 4. 影响面

- 安全:OS 级强制缺失;策略层(caps/sandbox.Resolve/approval)仍是唯一门禁,已在文档中如实标注。
- 宿主:桌面入口不可用;CLI/server 可用。
- 里程碑:M5 保持「部分完成」;不得在 release notes 中宣称 OS 级沙箱/桌面窗口完成。
- 无运行时行为变化(纯文档 + 现状确认)。

## 5. 回滚方式

删除本 ADR 并改回「未决」状态;或在新 ADR 中记录实现方案后回滚本决定。无代码/数据迁移。

## 6. 复议条件

- Linux 5.13+ 测试环境可用,且 CI 能跑 landlock 拒绝矩阵;
- 决定引入 mygo(配套新依赖 ADR 与桌面构建矩阵);
- 或产品明确接受「Web UI 替代桌面窗口」并修改 M5 验收口径(需主设计 §17 更新)。
