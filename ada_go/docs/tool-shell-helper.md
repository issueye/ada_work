# shell-helper:把 shell seam 暴露成模型工具(TS 实现)

`plugins/tool/shell-helper` 属于 **tool 插件**,当前用 **TypeScript** 实现:它把 `seam/shell`
的命令执行映射成模型可调用的 `run_shell`。启用它是「agent 可以执行命令」的唯一显式开关;
是否真的允许某条命令、在哪个目录、能跑多久,由 shell 实现与策略(`shell-guard`、`sandbox`、
可选的 `approval`/`caps`)决定。本插件不做白名单、审批或沙箱判断。

Go 版本已删除:插件目录只保留 `plugin.json` 与 `plugin.ts`,由 esbuild 编译、moejs 执行。

## 配置

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `program` | Windows `cmd.exe`;其他 `/bin/sh` | 执行命令的 shell 程序,必须无空白;经 `shell-local` 的 Program 校验 |
| `args` | Windows `["/C"]`;其他 `["-c"]` | 传给 shell 程序的固定前缀,命令本身作为下一个参数 |
| `maxTimeoutSeconds` | 120,上限 1800 | 单条命令允许的最长秒数;工具参数可再缩短 |

默认 shell 由宿主注入的 `__adaOS` 决定(`ada:shell` 的 `platform()`),preset 仍可覆盖
`program`/`args`。命令形态是 `program + args + command`,例如 Windows 上实际执行:

```text
cmd.exe /C echo probe-ok
```

因此管道、重定向由 shell 解析。工作目录通过 seam 的虚拟路径传递:留空是 `/workspace`,
相对目录会变成 `/workspace/<dir>`;`shell-local` 会把它解析到工作区真实路径并校验符号链接。

## 为什么需要扩展 moejs / bridge

旧 `ada:shell` 只有 `run(program,args,dir)`:

- 固定 30s 超时;
- 只返回 stdout 字符串;
- 非零退出直接抛错,工具无法拿到退出码;
- `ctx.tool` 没有超时参数,脚本工具无法设置 `ToolDef.Timeout`;
- 脚本工具只能返回字符串,不能声明 `denied`/`error`/`timeout`、`Details` 或 `Terminate`。

本次扩展了执行层,而不是把 shell 逻辑塞回 Go:

| 扩展点 | 位置 | 作用 |
| --- | --- | --- |
| `Host.ShellExec` | `ada_jsrt/jsrt` | 新增 `__adaHost.shellExec(program,args,dir,options)` |
| `ada:shell.exec` | `platform/bridge` | 返回 `{stdout,stderr,exitCode,truncated,durationMs,enforced,backend,mode}` |
| `ada:shell.platform()` | `platform/bridge` | 暴露宿主 `GOOS`,供 TS 选择默认 shell |
| `ctx.tool.spec.timeoutMs` | `ada_jsrt/jsrt` + `platform/bridge` | 映射为 `tools.ToolDef.Timeout` |
| 结构化工具结果 | `platform/bridge` | JS 返回 `{status,content,details,terminate}` 时映射为 `tools.Result` |

`ada:shell.run` 保持旧契约,已有 `git-helper`/`command-helper` 不受影响。

## 结果与事件

工具结果包含 stdout、stderr、退出码与截断提示;`Details` 字段带 program、dir、exitCode、
耗时、`enforced`(是否 OS 级隔离)与 sandbox mode。非零退出码仍返回 `StatusOK`,让模型自己判断,
真正的执行错误(拒绝/找不到目录/超时)返回 `StatusError`。

宿主(`cmd/ada-rpc-agent`)把以下过程实时转发为 seam RPC 事件:

| 事件 | 载荷 |
| --- | --- |
| `chat/tool`(running) | `turn`、`id`、`name`、`args`、`status="running"` |
| `chat/tool`(结果) | 同上,另加 `status`(`ok`/`denied`/`error`/`timeout`)、`text`、`durationMs` |

ada_chat 按 `turn` + `id` 更新对话里的工具卡片;卡片默认折叠,只显示工具名、状态和耗时,
点击标题才展开参数与结果。并行的工具调用按 id 合并,结果到达的顺序不影响卡片顺序。

## 安全边界

- 默认发布 preset(`agent-release`)启用的沙箱是 `sandbox-none`(**policy-only**,不做 OS 级隔离),
  模式为 `workspace-write`。`run_shell` 能执行任意命令,权限等同当前用户;设置页/README 必须
  明确提示这一点。
- 只读模式(`read-only`)下,`shell-guard` 的白名单会成为命令门:没有列入 `allow` 的程序会被拒绝。
  需要命令执行时不要把 preset 切到只读模式。
- 启用 `caps-gate` 后,`shell-guard` 还会要求调用者具备 `exec` 能力;本仓库发布 preset 未启用它。
- 没有接 `approval` 时,策略拒绝一律 fail-closed;本仓库不提供 GUI 审批,因此不启用审批插件。
- 命令输出回到模型的体积上限为 64 KiB(UTF-8 安全截断);`shell-local` 自身还有 4 MiB 上限并置
  `Truncated`。

> 注意:`shell-helper` 把 `program`/`args` 直接交给 shell 实现,**不会设置 `Command.Shell=true`**。
> 因此 `shell-guard.allowShell` 对 `run_shell` 不构成门禁;真正的 workspace-write 放行由
> `sandbox-none` 完成。发布前若要收紧,应在 `shell-guard` 中增加针对 shell 程序的显式门禁,
> 或改为设置 `Command.Shell` 并走 `shell-local.allowShell`。

## 示例

```powershell
go run ./cmd/ada up --preset examples/shell-helper-ts/shell.preset.json
go run ./cmd/ada agent --preset examples/shell-helper-ts/shell.preset.json -message "执行命令"
```

示例 preset 组合了 agent 栈、shell 栈与 TS `shell-helper`;回放脚本让模型调用
`run_shell` 执行 `echo shell-helper-ts`。

## 测试

```powershell
go test ./platform/bridge -run 'JSTool|JSShell'
go test ./platform/host/cli -run 'ToolShell'
go test ./plugins/...   # 不再有 shell-helper 的 Go 包
```

覆盖:当前平台默认 shell、`ada:shell.exec` 结构化结果、`ctx.tool` 超时映射、结构化工具结果
(`error`/`details`/`terminate`)、TS 插件端到端调用真实 `shell-local`。跨 JSON 边界的
shell 输出在 bridge 内再限 256 KiB(stdout+stderr),避免把 4 MiB 大对象塞进 JSON。
