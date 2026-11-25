# 脚本插件:task-helper(TS + 注入 ada:fs)

> 示例插件,验证脚本插件通过注入 moejs 模块做**持久化工具**。

## 1. 目的

`task-helper` 是 TypeScript 脚本插件:

- 通过 `ctx.tool` 注册 `task_add` / `task_list` / `task_done` / `task_remove`;
- 通过宿主注入的 `ada:fs` 模块读写工作区 `.ada/tasks.json`;
- 不直接执行系统调用;fs seam 负责路径、配额与策略。

## 2. 文件

```
examples/task-ts/
  plugins/task-helper/plugin.ts     TS 插件
  plugins/task-helper/plugin.json   kind=ts,inject: tools/fs
  task.preset.json                  agent 栈 + task-helper
  replay.jsonl                      回放:add → list
  workspace/                        任务文件目录(.ada/tasks.json)
```

## 3. 注入模块

`platform/bridge` 在插件 `inject:fs` 时注入:

```js
const host = globalThis.__adaHost
export function read(path) { return host.fs.read(path) }
export function write(path, content, createDirs) { return host.fs.write(path, content, createDirs) }
```

`fs.read/write` 落到 `seam/fs` 的 `Read/Write`;路径为虚拟路径 `/workspace/...`。

## 4. 工具

| 工具 | 参数 | 行为 |
|---|---|---|
| `task_add` | `title`(必填)、`priority`(1-5,默认 3) | 追加任务,id 自增 |
| `task_list` | `includeDone`(默认 false) | 列出未完成任务 |
| `task_done` | `id` | 标记完成 |
| `task_remove` | `id` | 删除任务 |

存储格式:

```json
[{"id":1,"title":"修复登录","priority":4,"status":"open"}]
```

## 5. 运行

```powershell
go run ./cmd/ada agent --preset examples/task-ts/task.preset.json -message "添加任务并列出"
```

真实端点:把 preset 切到 `llm-openai`、设置 `ADA_LLM_API_KEY` 即可(已手工验证)。

## 6. 测试

| 场景 | 用例 |
|---|---|
| TS 编译/注入 `ada:fs`/`ctx.tool` 装配 | `platform/host/cli.TestUpTaskScriptPlugin` |
| 真实工具调用(add→list 写文件) | `platform/host/cli.TestAgentTaskScriptPlugin` |
| 工具真实读写 | 手工/真实端点:add → list 写入 `.ada/tasks.json` |

## 7. 已知边界

1. **任务文件不是会话事实**:状态在 `.ada/tasks.json`,不在 session 事件里;多会话共享同一文件,并发写没有锁。
2. **无 fsync/原子事务**:`fs.Write` 单次原子写,但 read-modify-write 不是事务;并发 add 可能丢更新。
3. **无删除确认/权限分级**:任何能调用工具的主体都可改任务文件;生产应挂 caps/策略。
4. **没有 schema 强校验**:`task_add` 用 JS 侧最小校验;非法 JSON 会抛出。
5. **依赖注入模块名固定**:`ada:fs` 没有版本化/多模块注册表。
6. **无 SDK 类型**:`ctx.tool`/`ada:fs` 类型仍由示例本地声明。
