# 脚本插件:git-helper(TS + 注入 moejs 模块)

> 示例插件,用于验证「脚本插件 + 注入 moejs 模块 + 工具注册」在真实环境下的链路。

## 1. 目的

`git-helper` 是一个 **TypeScript 插件**,本身不实现 git 调用,只做:

- 参数校验(路径/limit/stat);
- 通过 `ctx.tool(spec, handler)` 注册 `git_status` / `git_diff` / `git_log`;
- 通过宿主注入的 moejs 模块 `ada:shell` 调用 `shell` seam。

git 的实际执行由 `shell` seam 完成;命令白名单/沙箱/审批由 shell 策略负责。

## 2. 实现

```
plugins/tool/git-helper/plugin.ts   脚本插件本体(内置插件目录)
plugins/tool/git-helper/plugin.json 清单:kind=ts,inject tools/shell
examples/git-ts/                    示例 preset + replay(插件清单统一在 plugins/ 下)
```

宿主能力(类型与实现见 `ada_jsrt/jsrt`、`platform/bridge` 的文档注释):

| JS API | 实现 |
|---|---|
| `ctx.tool(spec, handler)` | `ada_jsrt/jsrt` 登记 handler,`platform/bridge` 转成 `tools.ToolDef` |
| `import { run } from "ada:shell"` | `platform/bridge` 注入的 moejs 模块;`run(program,args,dir)` 经 shell seam 执行 |
| `ctx.log` / `ctx.provide` / `ctx.effect` | 既有的 bridge Host 能力 |

约束:

- 只有清单声明 `inject: {required:["tools","shell"]}` 的 TS 插件才能拿到这两个服务;
- `ctx.tool` 只能在 `apply` 期间调用;handler 返回值是字符串(非字符串会 JSON 序列化);
- 只读工具(`readOnly:true`)只是声明,实际权限仍由 shell 策略与 caps 决定。

## 3. 运行

```powershell
# replay 版本(无真实模型,CI 可跑)
go run ./cmd/ada agent --preset examples/git-ts/git.preset.json -message "看看 git 状态"

# 真实端点:把 preset 的 disable 改为 llm-replay、patch 模型,并设置 ADA_LLM_API_KEY
```

## 4. 测试

| 场景 | 用例 |
|---|---|
| TS 插件装配 + `ctx.tool` + 注入模块 | `platform/host/cli.TestAgentGitScriptPlugin` |
| tools/bridge 基础 | `platform/bridge` 既有用例;`platform/host/builtins` |
| 真实环境 | `ada agent` 真实端点手工验证(读取 git 状态并总结) |

## 5. 已知边界

1. **无流式/增量输出**:工具 handler 同步返回字符串。
2. **取消随工具 ctx 传播到原生 shell**:`jsrt` 把当前 worker 任务的 ctx 传给 `ada:shell`,但 JS 侧没有 `AbortSignal`;超时仍由 `tools` 的 30s 与 shell-local 超时兜底。
3. **参数只做最小校验**:路径拒绝 `-` 前缀/NUL;命令固定为 `git status/diff/log`,不接受任意程序。
4. **依赖宿主注入模块名**:目前固定为 `ada:shell`;没有版本化/多模块注册表。
5. **TS 类型仅示例内声明**:`@ada/sdk` 尚未补 `ctx.tool`/`ada:shell` 类型。
6. **无审批/沙箱**:示例 `git-ts` 只挂 shell-local;生产应挂 `shell-guard`/sandbox/caps。

## 6. 下一步

1. 把 `ctx.tool` 与 `ada:shell` 写入 `@ada/sdk` 类型与脚本插件契约;
2. ~~支持 `run` 携带取消~~ 已由 jsrt 把工具调用 ctx 传给原生 shell;下一步是在 JS 侧暴露 `AbortSignal`;
3. 增加 `ada:git` 专用模块(把 git 参数校验/输出格式化放在模块内,而不是插件脚本)。
