# M5 安全(第二刀:seam/shell)

> 契约来源:[seam-api-security.md](seam-api-security.md) §4。本页记录**已实现子集**、偏差与边界。
> `Command.Dir` 的 `/workspace` 可由 [workspace-plugin.md](workspace-plugin.md) 托管；cwd 限制仍不是 OS 级沙箱。

## 1. 范围

```
plugins/seam/shell/             契约:Command / Result / Chunk / Env / Chain / seam 插件
plugins/provider/shell-local/   实现:无 shell 直接 exec、受管环境、超时、输出上限、流式
plugins/policy/shell-guard/     策略:只读白名单、shell 模式默认拒绝
```

装配关系与 fs 一致:`seam-shell` 提供 `*shell.Chain`;provider `Install` 实现;policy `Decorate`;消费者 `Get` 得到组装后的 `Shell`。

## 2. 契约要点(已实现)

| 类型/字段 | 说明 |
|---|---|
| `Command.Program` | 程序名/路径,**不含空白**;`echo; rm -rf` 这类串被拒绝(不交给 shell) |
| `Command.Args` | 直接 exec 参数,不经 shell 解析 |
| `Command.Dir` | 虚拟路径(`/workspace`、`/tmp`);越界拒绝;缺省为 workspace 根 |
| `Command.Env` | 只接受受管变量(白名单 ∩ 宿主 + `ADA_*`);注入其它键 → `EINVALID` |
| `Command.Timeout` | 缺省 120s;上限 30m(更长请走 jobs) |
| `Command.Shell` | 默认 false;true 需要 `allowShell` 授权,否则 `EDENIED` |
| `Result.Facts` | 本次实际约束;未接沙箱时 `Enforced=false`,结果如实反映 |
| `Result.Truncated` | 输出超限(默认 4 MiB,可配)时置 true,命令被终止 |
| `Stream` | 按 pipe 读取块回调(**≤64 KiB**),同步回调形成回压;回调失败 → 终止命令 |

## 3. 安全不变量(实现侧)

- **无 shell 默认**:`exec.CommandContext(program, args...)`,程序名有空白直接拒绝。
- **环境净化**:子进程 `Env` 只含受管变量;宿主 `SECRET`/未声明变量不可见。变量名匹配在
  Windows 上大小写不敏感;`PATH`/`PATHEXT` 缺失时补系统默认值(不覆盖显式值)。
- **超时终止**:`context.WithTimeout` + 显式 `terminate`;POSIX 用进程组杀整棵树。
- **输出上限**:收集器截断并取消命令;超限返回 `Truncated=true`(不是静默截断)。
- **工作目录**:虚拟路径解析,`..`/反斜杠/NUL/未知根拒绝。
- **非零退出不是错误**:退出码是结果的一部分;seam 错误只表示「没跑起来/被拒绝/超时」。

## 4. 偏差与边界(诚实记录)

1. **Windows 进程树已接 Job Object**(`process_windows.go`:CreateJobObject + KILL_ON_JOB_CLOSE),
   POSIX 用 `Setpgid` + `kill(-pgid)`;另加 `exec.Cmd.WaitDelay=2s`,避免进程退出后管道被残留子进程拖住
   (`TestTimeout` 曾在整机负载下偶发 6s,现已降低该风险)。
2. **`Facts` 是 shell 本地类型**:sandbox seam 已落地,但 `Facts` 还没有从 `sandbox.Facts` 回填,
   `Enforced` 恒为 false、`Backend="none"`;设计要求的「Facts 来自 sandbox」未满足。
3. **没有事件**:`shell/start`、`shell/exited`、`shell/denied` 尚未发出(观测/审计缺口)。
4. **没有 `shell-env` 策略**:provider 自身限制 env;独立策略文件未做。
5. **沙箱判定已接到 `sandbox.Resolve`**:`shell-guard` 先问 sandbox(操作/程序),再做 shell 门与白名单;程序级 jail 仍靠 OS 级后端(未实现)。
6. **能力位可选、审批已实现**:注入 `caps` 服务时,`shell-guard` 要求 principal + `exec` 能力(fail-closed);`approval` 升级已有;`allowShell` 仍是配置开关。
7. **流式顺序**:stdout/stderr 由两个 reader 并发推送,`Seq` 只保证每路递增,不保证两路全序。
8. **后台任务**:超过 30s 的命令被拒绝(未接 jobs seam)。
9. **Windows 路径特性**(盘符/UNC/保留名)未覆盖;`Dir` 的符号链接已解析并校验 root(`TestDirSymlinkEscape`)。
10. **Windows 环境变量名大小写与 PATH 兜底**:系统环境块常见写法是 `Path`/`SystemRoot`,而白名单按
    `PATH`/`SYSTEMROOT` 书写;实现按大小写不敏感匹配,避免整条 PATH 被丢弃导致
    `where`/`git`/`curl` 等外部命令全部「不是内部或外部命令」。若父进程完全没有 PATH/PATHEXT
    (被启动器/快捷方式裁剪),补 `%SystemRoot%\System32` 等系统目录与默认扩展名,但**不覆盖**
    显式 PATH——环境净化仍是策略默认值。

### 4.1 脱敏限制

`shell-redact`(模式表来自 `platform/redact`,与 `fs-redact` 共用)按**块**脱敏:若密钥被拆到两个流式块里仍可能漏;完整结果(stdout/stderr)在结束时统一脱敏。

### 4.2 `shell/observe` 事件

`policy/shell-observe` 在每次 Run/Stream 后 `Emit("shell/observe", payload)`:

| 字段 | 类型 | 说明 |
|---|---|---|
| `op` | string | `run` / `stream` |
| `program` | string | 程序名;**不含 args**(参数可能含 secret) |
| `exit` | int | 退出码 |
| `bytes` | int64 | stdout+stderr 字节数 |
| `durationMs` | int64 | 执行耗时(毫秒) |
| `plugin` | string | 调用者插件 id |
| `error` | string | 失败文本(成功时缺省) |

观察不是门禁:`shell-guard`/`caps-gate` 的拒绝也会被记录(observe priority=250 最外层)。

## 5. 测试矩阵

| 场景 | 用例 |
|---|---|
| 基本执行:输出/退出码/耗时/Facts | `shelllocal.TestRunEcho` |
| 环境净化与未白名单注入 | `shelllocal.TestEnvIsolation` |
| Windows 变量名大小写(`Path`/`SystemRoot`) | `shelllocal.TestManagedEnvWindowsNameCase`、`TestRunSeesMixedCasePath` |
| PATH/PATHEXT 缺失时补系统默认值 | `shelllocal.TestManagedEnvWindowsPathFallback`、`TestManagedEnvWindowsPathKept` |
| `Program` 含空白拒绝 | `shelllocal.TestRejectsProgramWithSpace` |
| 超时(`ETIMEOUT`)且及时返回 | `shelllocal.TestTimeout` |
| 输出上限截断 | `shelllocal.TestOutputTruncation` |
| 流式 chunk + 汇总 | `shelllocal.TestStream` |
| `Shell=true` 默认拒绝 | `shelllocal.TestShellModeDenied` |
| 非零退出码是结果不是错误 | `shelllocal.TestNonZeroExit` |
| `Dir` 越界拒绝 | `shelllocal.TestDirEscape` |
| 只读白名单 / shell 授权 / 内核装配 / 无 provider | `shellguard.TestReadOnlyAllowlist`、`TestShellFlag`、`TestKernelIntegration`、`TestKernelWithoutProvider`、`TestProviderRequiresWorkspace` |

## 6. 下一步

1. `sandbox` seam + probe(landlock/bwrap/Job Object),让 `Facts` 真正回填;Windows 用 Job Object 杀树。
2. `shell/start`/`shell/exited`/`shell/denied` 事件与审计。
3. `policy/shell-env` 独立装饰器(env 过滤从 provider 移出)。
4. `seam/jobs`:长任务、后台、交互。
5. ~~caps broker~~ 已实现(可选注入);下一步把 `allowShell` 也纳入授权流程。

## 7. 验证命令

```powershell
go test -race -count=1 ./plugins/...
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```