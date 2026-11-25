# seam/secrets 与 provider/secrets-env

> 契约来源:[plugin-kernel-design.md](plugin-kernel-design.md) §13(capability `secrets.<name>`)。
> 本页记录已实现子集、偏差与边界。

## 1. 范围

```
plugins/seam/secrets/         契约:Getter/Chain/错误(只类型与链)
plugins/provider/secrets-env/ 实现:从进程环境按白名单读取
plugins/policy/secrets-guard/ 策略:principal + secrets.<name> 授权
```

授权由 `secrets-guard` 强制(`secrets.<name>`,可用 `secrets.*` 通配);审计尚未实现。

消费方不止 Go 插件:桥接层给所有 TS 插件注入 `ada:secrets`(`get(name)` / `has(name)`),
底层是 `jsrt` 的 `__adaHost.secretsGet(name)`,调用时透传工具调用的 ctx——所以
`secrets-guard` 仍然按 principal 生效,key 不会出现在 config、日志或工具结果里。
`has(name)` 只返回布尔,供脚本「有 key 就用、没有就换 provider」,不泄露值。

## 2. 契约要点

| 类型/方法 | 语义 |
|---|---|
| `Getter.Get(ctx,name)` | 按名读取;未知/未配置 → 错误 |
| name 规则 | `^[A-Z0-9_]{1,64}$`(大写字母/数字/下划线) |
| `Chain.Install/Decorate/Get/Applied` | 与 fs/net 链同构 |
| 错误 | 参数错误/未配置/拒绝都用 seam 错误码 |

## 3. provider 行为

- 环境变量名:`ADA_SECRET_<name>`;
- **必须显式 `allow` 名单**;名单为空 → 任何 secret 都不可读(fail-closed);
- 名单里但环境变量缺失/为空 → 「未配置」错误;
- 非法名 → 参数错误;
- 不写日志、不返回部分值。

## 4. 偏差与边界

1. **caps 强制已落地**:`secrets-guard` 从 `kernel.Principal(ctx)` 取调用者并向 Broker 查 `secrets.<name>`;provider 白名单仍需同时满足。
2. **没有审计/redact**:读取本身不产生事件;调用方必须避免把值写进日志/事件。
3. **只有环境来源**:文件、keyring、vault 未实现;没有轮换/热更新。
4. **明文在进程内存**:没有零化;环境变量本身可被子进程继承(与 shell 的受管环境需配合)。
5. **不提供列举接口**:调用方必须知道名字;没有 `List`。
6. **allow 名单是静态配置**:增删需要重启装配。
7. **TS 侧可读**:`ada:secrets` 对所有 TS 插件可导入,白名单内 secret 对同进程的脚本插件都可读;
   真正的边界是 `secrets-env` 的 allow 与 `secrets-guard` 的 principal,不是「脚本不知道模块」。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 白名单内读取 | `secretsenv.TestGet` |
| 不在白名单 | `secretsenv.TestNotAllowed` |
| 环境缺失 | `secretsenv.TestMissingEnv` |
| 非法名 | `secretsenv.TestInvalidName` |
| 内核集成 | `secretsenv.TestKernelIntegration` |
| guard:具体/通配授权、未授权/无身份 | `secretsguard.TestAllowed`、`TestDenied` |
| guard 使用注入的 caps 服务 | `secretsguard.TestServiceBroker` |
| TS 侧按名读取(`ada:secrets`) | `bridge.TestSearchDeepSeek`、`TestSearchAutoFallsBackToDuckDuckGo`、`TestSearchDeepSeekWithoutSecret` |

## 6. 下一步

1. `policy/secrets-guard`:principal + `secrets.<name>` 能力检查(复用 caps 服务)。
2. 审计事件 `secrets/observe`(只记名字,不记值)。
3. 文件/keyring 来源与轮换。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/provider/secrets-env
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```