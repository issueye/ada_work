# M5 能力授权(caps)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §13.2(信任等级与能力上限)、§13.3(强制点)。

## 1. 范围

```
platform/caps/       L2 基础设施:能力名语法、信任等级、授权表(Broker)、拒绝审计
platform/plugin/     清单新增 caps / trust 字段并在校验期拒绝非法值
plugins/policy/caps-gate/  调用点强制(fs/shell):principal → Broker 授权
plugins/provider/caps-static/  提供 "caps" 服务(*caps.Broker),让宿主/装配层共享同一个授权表
platform/assemble/                 GrantManifests:按清单 caps/trust 灌入 Broker
```

本刀做**静态授权 + 调用点强制**:`caps-gate`(fs/shell)、`secrets-guard`、`rpc-guard`
从 `kernel.Principal(ctx)` 取调用者身份;`kernel.Context.Context()` 已负责把 principal 传进 ctx。
剩余的是信任来源与目标级约束(见 §5)。

## 2. 能力名语法

```
fs.read | fs.write
exec
process | process.isolated
secrets.<name>
config.<namespace>
ui
rpc.call | rpc.call.<method>
rpc.listen.<network>
```

- 未知命名空间、层级错误、空名一律非法(清单校验期拒绝)。
- 通配只放宽**目标**,不放宽命名空间:`rpc.call` 覆盖 `rpc.call.agent.session.list`;
  `secrets.*` 覆盖 `secrets.GITHUB_TOKEN`;持有 `fs.read` 不会获得 `fs.write`。
- `net.fetch` 已随 ADR 0012 移除:网络没有受控 seam,写进清单也会被判为未知命名空间。

## 3. 信任等级

| 能力 | 最低信任 |
|---|---|
| `fs.read` | third-party |
| `fs.write` / `ui` / `config` / `secrets` / `rpc.listen` | verified |
| `rpc.call` | third-party |
| `exec` / `process` | official |

`builtin` > `official` > `verified` > `third-party`;`AllowedByTrust` 做等级比较。
设计口径是信任来自**安装来源**(签名/仓库),不来自清单自述;当前实现已把
**磁盘上的 TS 插件**一律降为 `third-party`(`plugin.LoadDir` 的 `ApplySourceTrust`);
安装根目录则由 `install.LoadDir` 按 `index.json` 的 trust 回填 verified/official,
再交给 `builtins.ApplyAutoGrants` 灌进 Broker。程序内直接构造的 `Manifest`
仍可自述 trust(测试/宿主内部用,不经过磁盘加载)。

## 4. Broker

```go
broker := caps.NewBroker(audit)
broker.Grant("plugin-a", "fs.read", "exec")
broker.Authorize(ctx, "plugin-a", "fs.write", "/workspace/a") // → *DeniedError(EDENIED)
```

- **fail-closed**:未登记插件、未授予能力、能力名非法 → 拒绝。
- 拒绝写审计(`Deny{Plugin,Cap,Target,Reason,At}`);URL target 在审计/错误里去掉 userinfo 与 query。
  `caps-static` 把拒绝转成 `caps/deny` 事件,`audit-log` 订阅后落 JSONL。
- `AuthorizeWithTrust` 额外检查信任等级。
- `Grant` 整批校验,任一能力非法则整批失败(不会部分生效);`Revoke` 撤销全部。
- `Snapshot()` 供诊断。

## 5. 偏差与边界

1. **调用点强制已落地(fs/shell/secrets/rpc)**:`policy/caps-gate`(fs/shell)、`policy/secrets-guard`(secrets)、
   `policy/rpc-guard`(rpc)从 `kernel.Principal(ctx)` 取调用者,向 Broker 查能力;
   缺少身份/未授权一律拒绝。`caps-gate` 优先使用注入的 `caps` 服务;`assemble.GrantManifests` 把清单 `caps`/`trust` 灌入 Broker;CLI 的 `up`/`inspect` 已通过 `applyAutoGrants` 自动接线。
2. **trust 来源已接安装索引**:`plugin.LoadDir` 把磁盘非 Go 插件降为 `third-party`;
   `install.LoadDir` 对安装根目录按 `index.json` 回填 verified/official;直接拷入的非安装目录
   仍按 `third-party` 处理。
3. **安装同意只有 CLI 级**:`-yes` 已能展示能力后继续;桌面确认/逐项授权未实现,安装 trust
   已通过 `install.LoadDir` 灌入 `caps`。
4. **cap 拒绝审计已接 JSONL**:`caps.Broker` 的 `Deny` 回调由 `caps-static` 转成
   `caps/deny` 事件,`audit-log` 订阅落盘;target 已脱敏。非 `caps-static` 的临时 Broker
   (无 caps 服务时的 fallback)也发同一事件。
5. **没有目标级约束**:`fs.write` 的「工作区 + 插件自身目录」由 sandbox/fs-local 承担,
   caps 层不重复判定路径;`exec` 的程序 allowlist 未与 caps 合并。
6. **secrets 已接强制;config/ui 仍未接**:`secrets-guard` 已按 principal 授权;
   `config`/`ui` 还没有调用点。网络已不再是 caps 的管辖范围(ADR 0012)。

## 6. 测试矩阵

| 场景 | 用例 |
|---|---|
| fs 授权放行/未授权/无身份拒绝 | `capsgate.TestFSAuthorization` |
| shell exec 授权 | `capsgate.TestShellAuthorization` |
| 内核集成:`ctx.Context()` 携带身份,background 绕过被拒 | `capsgate.TestKernelIntegration` |
| caps 服务优先于 config grants | `capsgate.TestServiceBrokerPreferred` |
| caps-static 提供 Broker | `capsstatic.TestProvidesBroker` |
| 清单 caps/trust 灌入 Broker、trust 不足拒绝 | `assemble.TestGrantManifests` |
| 能力名合法/非法集合 | `caps.TestParse` |
| 通配匹配(主机/名字/命名空间不放宽) | `caps.TestGrantsWildcard` |
| 授权 fail-closed、拒绝错误码、审计、撤销 | `caps.TestBrokerAuthorize` |
| 信任等级矩阵 | `caps.TestTrust` |
| 拒绝 reason/URL target 脱敏 | `caps.TestDenyAuditReasonAndRedaction` |
| caps 拒绝落 JSONL | `auditlog.TestCapsDeny` |
| 并发授权(-race) | `caps.TestConcurrent` |
| 拒绝信息可读 | `caps.TestDeniedMessage` |
| 清单 caps/trust 校验 | `plugin.TestCapsAndTrust` |

## 7. 下一步

1. 在 plan 报告里展示能力清单与 trust 来源(磁盘/安装索引/内置)。
2. `$ADA_HOME/keys` 与签名发布工具。
3. JSONL 审计出口与脱敏。
4. JSONL 审计出口与脱敏。

## 8. CLI 自检(`ada caps`)

```powershell
go run ./cmd/ada caps examples/plugins            # 列出 caps/trust 并尝试授权
go run ./cmd/ada caps <目录> -json                # 结构化输出(ok/error/plugins)
```

- 只做**静态授权检查**(`assemble.GrantManifests`),不挂载插件;
- trust 不足 → 退出码 1,输出「不可授权」;`-json` 里 `ok=false` 并带 `error`;
- 这是「静态自检」:把清单 caps 灌进 Broker;运行时接线由 `up`/`inspect` 的 `applyAutoGrants` 完成(写进提供 `caps` 服务的插件配置)。

## 9. 验证命令

```powershell
go test -race -count=1 ./caps ./plugin
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
