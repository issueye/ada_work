# M5 安全(第三刀:seam/sandbox)

> 契约来源:[seam-api-security.md](seam-api-security.md) §2。本页记录**已实现子集**、偏差与边界。

## 1. 范围

```
plugins/seam/sandbox/           契约:Mode / Op / Request / Decision / Facts / BackendInfo / Registry / seam 插件
plugins/provider/sandbox-none/  后端:policy-only(只在策略层限制,Enforced=false)
```

装配关系:`seam-sandbox` 提供 `*sandbox.Registry`;provider 注册后端;消费者 `Get(name)` 取默认(名字排序第一个)。

## 2. 契约要点

| 类型 | 说明 |
|---|---|
| `Mode` | `read-only` / `workspace-write` / `danger-full-access` |
| `Op` | `fs.read` / `fs.write` / `fs.edit` / `fs.list` / `shell.exec` |
| `Request` | 会话 id + 操作 + 虚拟路径 + 程序/参数 + Write/Network |
| `Decision` | `Allow` + `Mode` + **可展示的 Reasons** + `Escalatable`(可否走审批升级) |
| `Facts` | `Backend` / `Mode` / `Roots` / **`Enforced`** |
| `BackendInfo` | 名字 + `Probe` + `Kind`(`os-level` / `policy-only`) |

`Resolve` 是唯一判定入口;后端只负责执行时约束,不能自行放行。

> `OpNetFetch` 已随 ADR 0012 删除:网络不再有受控 seam,沙箱不参与网络判定。

## 3. `sandbox-none` 的判定规则

| 模式 | fs.read/list | fs.write/edit | shell.exec |
|---|---|---|---|
| read-only | 放行 | 拒绝(`Escalatable=true`) | 放行(交由 shell 策略判定) |
| workspace-write | 放行 | 仅 `/workspace` `/tmp`,否则拒绝(`Escalatable=true`) | 放行 |
| danger-full-access | 放行 | 放行 | 放行 |

拒绝一定带 `Reasons`(可直接展示),`Escalatable` 标记用于未来的审批升级。

**Facts 如实**:`Enforced=false`、`Backend="sandbox-none"`、`Kind="policy-only"`。
假装隔离比不隔离更危险。

## 4. 偏差与边界

1. **没有 OS 级后端**:bwrap / landlock / seatbelt / Windows ACL 均未实现;
   当前只有 policy-only。
2. **probe 失败 → fail-closed 的装配闸未实现**:没有可选后端,也没有 preset 装配期的探测拒绝。
3. **三闸已串联**:`fs-sandbox`、`shell-guard` 现在在每次调用前调用 `sandbox.Resolve`,
   后端不可用时 fail-closed;模式仍来自 sandbox 后端的 config,而不是 session 事件。
4. **模式不来自 session 事件**:`Mode(ctx, sessionID)` 返回单模式,未接 session 变更与回放。
5. **没有 `sandbox/request` / `sandbox/denied` 事件**:审计面缺口。
6. **没有 approval / caps broker**:`Escalatable` 只是标记,没有升级流程。
7. **默认后端按名字排序**:存在多个后端时默认选择与注册顺序无关,但还没有「按功能探测挑最优」的策略。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 三模式 × 操作/路径判定矩阵 | `sandboxnone.TestResolveMatrix` |
| Facts 如实报告未隔离 + Backends | `sandboxnone.TestFacts` |
| 非法模式拒绝 | `sandboxnone.TestInvalidMode` |
| 经内核装配(seam + provider) | `sandboxnone.TestKernelIntegration` |
| 无后端时 `EUNIMPLEMENTED` | `sandboxnone.TestRegistryWithoutProvider` |

## 6. 下一步

1. 让 `fs-sandbox` / `shell-guard` **调用 `sandbox.Resolve`**,把 config mode 换成沙箱判定
   (同时删除重复的模式逻辑)。
2. 实现第一个 OS 级后端(按平台选 landlock/seatbelt/Windows ACL),并把 probe 失败接到装配期。
3. `sandbox/request` / `sandbox/denied` 事件与审计。
4. 模式变更写 session 事件,`Mode` 按会话回放。
5. approval-gate:消费 `Escalatable` 走审批,fail-closed。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/...
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```