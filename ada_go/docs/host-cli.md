# platform/host/cli(命令行宿主适配器)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §12(宿主)与 AGENTS §2.2(宿主适配器)。

## 1. 范围

```
platform/host/cli/    命令实现:参数解析、输出、退出码
cmd/ada/     只做 os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
```

把它从 `main` 里拆出来的理由:

- **可测试**:`Run(args, out, errOut)` 注入 writer,测试不必启动子进程;
- **可嵌入**:其它宿主(desktop/server)可以复用同一套命令;
- **只做映射**:不做业务,业务仍在内核/插件/基础设施。

## 2. 命令与退出码

| 命令 | 作用 |
|---|---|
| `plan <目录|--preset 文件> [-json]` | 清单 → 依赖图 → 顺序/问题 |
| `up <目录|--preset 文件>` | 挂载、打印 fiber/服务、停止 |
| `inspect <目录|--preset 文件> [-json] [-chain]` | 诊断快照(fiber/服务/effect/事件/装饰器链) |
| `agent <目录|--preset 文件> -message <文本> [-workspace <id>] [-json]` | 挂载 agent 栈并跑一轮;`-workspace` 只接受 preset 中已配置的 ID |
| `verify <目录> [-keys 文件] [-require-signature]` | 清单/摘要/验签/信任 |
| `caps <目录|--preset 文件> [-json]` | 静态能力自检:清单 caps/trust → Broker 授权 |
| `repo <索引 URL> [-keys] [-require-repo-signature] [-json]` | 在线读取仓库索引(可选验签) |
| `check <目录|--preset 文件> [-json]` | 清单 + 依赖图 + caps + 逐插件编译校验 |
| `serve-repo -root <目录> -addr <地址> [-token] [-rate-limit N] [-cert 证书 -key 私钥]` | 只读仓库服务(SIGINT 优雅停止;可 TLS/限流) |
| `serve -root <目录> -addr <地址> [-token] [-rate-limit N] [-cert 证书 -key 私钥]` | 只读 HTTP API 服务(SIGINT 优雅停止;可 TLS) |
| `install <目录> -dest <根> [-keys 文件] [-require-signature]` | 校验后原子安装 |

退出码:`0` 成功 / `1` 计划不可执行或装配/校验失败 / `2` 用法错误。

## 3. 测试

`platform/host/cli.Test*` 直接调用 `Run` 并断言输出与退出码:

- plan 文本顺序、`-json` 合法与 `ok=true`、坏目录 → 1;
- 无参数/未知命令 → 2;
- verify(未签名 ts → third-party);
- inspect(挂载 TS preset 并输出 fibers/services);
- up(挂载后停止);
- install(安装到临时目录并检查文件);
- 内置安全栈 up(seam-fs + fs-local + caps-static + caps-gate)。

**内置 Go 插件注册表**:与 `platform/host/builtins` 共享同一份内置 seam/provider/policy(seam-fs、fs-local、caps-static、caps-gate、observers、redact、seam-secrets/secrets-env、seam-rpc/rpc-json/rpc-tls/rpc-guard/rpc-observe 等),因此 `up`/`inspect` 可以直接挂安全/secret/rpc 栈;第三方 Go 插件仍需宿主编译时注册。`up`/`inspect` 会在挂载前把清单 caps 灌进提供 `caps` 服务的插件(`applyAutoGrants`),让 `caps-gate` 用同一份授权表。

**顺带修掉一个缺陷**:`install` 曾复用 `verify` 的参数解析,导致 `-dest` 被当成未知参数而失败;
现在有独立的 `parseInstallArgs` 与测试覆盖。

## 4. 偏差与边界

1. **宿主适配器已拆开**:`platform/host/desktop`(视图映射骨架)、`platform/host/server`(只读 HTTP)、`platform/host/registry`(仓库服务)已落地;CLI 仍是最完整的一个。
2. **Go 插件仅限内置**:CLI 注册了仓库内置 Go 插件;第三方 Go 插件需宿主编译注册;CLI 对 ts 插件完整可用。
3. **文本输出是人读契约,不是机器契约**:脚本请用 `-json`(plan/inspect)。
4. **交互式审批已可选**:`provider/approval-cli` 提供 stdin/stdout 审批;默认拒绝人仍是默认策略。
5. **没有 `ada version`**:宿主版本需由调用方提供。

## 5. 下一步

1. `platform/host/desktop`(mygo):窗口、主线程、命令面板映射。
2. `ada version` 与版本注入。
3. `ada agent`:交互跑一轮(当前 `up` 只做一次性挂载)。
4. 安装/升级/回滚/仓库命令补齐文档表格。`ada agent` 已支持单条 `-message`;交互 REPL 未做。

## 6. 验证命令

```powershell
go test -race -count=1 ./platform/host/cli
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```