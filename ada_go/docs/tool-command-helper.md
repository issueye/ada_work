# 脚本插件:command-helper(TS + 注入 ada:shell)

> 示例插件:通过注入模块执行**受控命令**,用于真实环境下的工具链验证。

## 1. 目的

`command-helper` 是 TypeScript 脚本插件:

- 通过 `ctx.tool` 注册 `cmd_list` / `cmd_run`;
- 通过宿主注入的 `ada:shell` 执行命令;
- 程序白名单来自插件配置 `allow`,默认 `["git","go"]`;
- 参数以数组传入,不经过 shell 解析。

## 2. 文件

```
plugins/tool/command-helper/plugin.ts   TS 插件(内置插件目录)
plugins/tool/command-helper/plugin.json kind=ts,inject: tools/shell, config.allow
plugins/seam/shell/                     shell seam
plugins/provider/shell-local/           workspace=仓库根

examples/command-ts/
  command.preset.json                  agent 栈 + command-helper
  replay.jsonl                         git --version 回放
```

## 3. 工具

| 工具 | 参数 | 行为 |
|---|---|---|
| `cmd_list` | 无 | 返回 allow 列表 |
| `cmd_run` | `program`(必填)、`args`(字符串数组)、`dir`(默认 `/workspace`) | 白名单校验后执行,返回 stdout(+stderr) |

约束:

- `program` 必须命中 `allow`;否则拒绝;
- `args` 最多 64 个,拒绝 NUL/换行;
- `dir` 必须以 `/workspace` 开头且不含 `..`;
- 执行失败(非零退出码)会变成工具错误,模型可见。

## 4. 运行

```powershell
go run ./cmd/ada agent --preset examples/command-ts/command.preset.json -message "执行 git --version"
```

真实端点:切换 `llm-openai`、设置 `ADA_LLM_API_KEY` 后即可;已手工验证返回 `git version 2.47.0.windows.2`。

## 5. 测试

| 场景 | 用例 |
|---|---|
| 装配/编译/注入 `ada:shell` | `platform/host/cli.TestUpCommandScriptPlugin` |
| 命令调用(回放) | `platform/host/cli.TestAgentCommandScriptPlugin` |
| 真实端点 | 手工:模型调用 `cmd_run` 执行 `git --version` |

## 6. 已知边界

1. **脚本白名单不是安全边界**:白名单只限制程序名;生产必须叠加 `shell-guard`/sandbox/caps/审批,否则模型可执行白名单程序的所有子命令(如 `git clean`)。
2. **无 shell 解析**:参数数组直接传给程序;不能使用管道/重定向;需要 shell 语义时应由受信插件显式调用 `ctx.shell`。
3. **无流式输出**:工具同步返回合并后的字符串。
4. **取消随工具 ctx 传播到原生 shell**:`jsrt` 会把当前任务 ctx 传给 `ada:shell`;超时仍靠 tools 30s 与 shell-local 默认值,JS 侧无 `AbortSignal`。
5. **无 SDK 类型**:`ctx.tool`/`ada:shell` 类型仍由示例声明。
6. **`dir` 只做词法校验**:真实路径映射由 shell-local/策略负责。
