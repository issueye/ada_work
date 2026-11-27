# M5 安全(第四刀:seam/approval)

> 契约来源:[seam-api.md](seam-api.md) §5 与 [plugin-kernel-design.md](plugin-kernel-design.md) §12.2(门禁)。
> 本页记录**已实现子集**、偏差与边界。

## 1. 范围

```
plugins/seam/approval/           契约:Request / Decision / Approver / Registry / Ask(带超时) / seam 插件
plugins/provider/approval-deny/  默认审批人:永远拒绝,给出可展示理由
plugins/policy/fs-sandbox/       集成点:可升级的拒绝 → Ask;批准后放行本次操作
plugins/policy/shell-guard/      集成点:shell 门/白名单拒绝 → Ask;批准后放行
```

装配关系:

```
seam/approval 插件 ──provide "approval"──▶ *approval.Registry
approval-deny   ──inject "approval"──▶ Registry.Register(默认拒绝)
fs-sandbox / shell-guard ──可选 inject "approval"──▶ 拒绝时 Ask()
```

两个策略都用**可选依赖**(`WithOptional("approval")` + `TryService`):没有 approval 服务时不会 pending,
但可升级拒绝一律按拒绝处理(fail-closed)。

## 2. 契约要点

| 类型/函数 | 说明 |
|---|---|
| `Request` | `Source` / `Op` / `Target` / `Reason` / `Details` / `Timeout` |
| `RedactTarget` | URL target 去掉 userinfo/fragment/query;非 URL 路径原样 |
| `Decision` | `Allow` / `Reason`(必须可展示) / `Approver` |
| `Approver` | `Name()` + `Approve(ctx, Request)` |
| `Registry` | 按名注册;默认取名字排序第一个;没有审批人 → 错误 |
| `Ask(ctx, registry, backend, req)` | **唯一调用入口**:解析审批人、强制超时(默认 30s)、所有失败转拒绝错误 |

## 3. 语义

- **fail-closed**:没有服务、没有审批人、审批报错、**超时** → 全部拒绝;
  只有 `Allow=true` 才放行。
- **超时**:`Ask` 用 `context.WithTimeout` 包住 `Approve`;超时返回 `ETIMEOUT` 且 `Allow=false`,
  挂起的审批人不能阻塞调用方。
- **升级触发点**:fs 在 sandbox `Escalatable` 时问审批;shell 在「shell 门拒绝 / 只读白名单拒绝」时问审批。
  不可升级的拒绝直接拒绝,不打扰审批人。
- **事件**:调用审批人**之前**发 `approval/requested`(id/source/op/target/reason/plugin;target 已脱敏),完成后发 `approval/decided`(source/op/target/allow/approver/reason);
  只观察、不阻断。
- **批准即放行本次操作**(policy-only 后端下等价于临时提升);OS 级后端需要重新施加约束,尚未实现。

## 4. 偏差与边界

1. **审批人**:`approval-deny`(默认拒绝)、`approval-cli`(交互式 y/yes)与 ACP 客户端的
   `acp-client`(桌面宿主在 preset 装了 `seam-approval` 时注册,经 `session/request_permission` 往返)
   已落地;`core/approval`(M4.5)未实现。桌面审批仍由宿主负责超时/取消,超时按拒绝。
2. **`approval/requested` 已实现**:`Registry.SetNotifier` 在 `Ask` 等待决定前广播;没有审批人时不广播(fail-closed 先决)。
3. **批准绕过沙箱判定**:批准后的放行由策略层决定,OS 级后端尚未实现「按批准临时提升模式」。
4. **caps 已存在、尚未与审批合并**:`caps-gate`/`shell-guard` 做静态能力授权,`approval` 做动态升级;两者仍是两条独立闸门,没有「批准后临时提权」的统一模型。
5. **审批人选择**:默认按名字排序,没有「按 op/来源路由」的策略。
6. **事件时机**:`approval/decided` 在装饰器内同步发出;事件派发失败不会回滚审批结果。

### 3.1 事件载荷

| 事件 | 字段 |
|---|---|
| `approval/requested` | `id`、`source`、`op`、`target`、`reason`、`plugin`(调用者身份,可缺省) |
| `approval/decided` | `source`、`op`、`target`、`allow`、`approver`、`reason` |

`requested` 由 seam/approval 插件接入 `Registry.SetNotifier` 发出;通知器 panic 不影响审批结果(测试覆盖)。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 注册表:默认选择/重复注册/空表 | `approval.TestRegistry` |
| Ask:拒绝原样返回、无服务 fail-closed | `approval.TestAskDenied` |
| Ask:强制超时(`ETIMEOUT`)且及时返回 | `approval.TestAskTimeout` |
| 默认审批人永远拒绝 + 内核装配 + 无审批人 | `approvaldeny.TestAlwaysDenies`、`TestKernelIntegration`、`TestNoApprover` |
| fs 升级审批:允许放行 / 拒绝 / 缺失 | `fssandbox.TestApprovalEscalation` |
| shell 升级审批:允许放行 / 拒绝 | `shellguard.TestShellApproval` |
| URL target 脱敏 | `approval.TestRedactTarget`、`approvalcli.TestPromptRedacted` |

## 6. 下一步

1. 审计字段补全会话 id 与目标摘要;URL target 已在提示/事件/审计中脱敏,非 URL 路径保持原样。
2. `core/approval` 桌面 UI 审批人;CLI 审批人已由 `provider/approval-cli` 提供。
3. `policy/approval-gate` 已接到 `tools/pre-execute`;caps 已由 `caps-gate`/`secrets-guard`/`rpc-guard` 独立强制。
4. 审批人路由(按 op/来源);批量审批与会话级「记住本次决定」。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/...
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
