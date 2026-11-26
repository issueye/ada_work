# core/plan — 会话级计划

> 契约：[seam/plan](../plugins/seam/plan/plan.go)；工具：[plugins/core/plan](../plugins/core/plan/plan.go)；
> ACP 映射见 [rpc-control-layers.md](rpc-control-layers.md)。

## 1. 它是什么

`core/plan` 给每个会话一份可演进的计划：步骤、状态（`pending` / `in_progress` / `completed`）、
优先级（`high` / `medium` / `low`）。模型通过工具创建与更新，下一轮请求里会自动看到
最新计划；ACP 客户端通过 `session/update` 的 `plan` 收到同一份内容。

**计划不是内存状态**：每次变更都写成一条 session 事件：

```text
type: plugin:plan/updated
data: {"title":"...","steps":[{"content":"...","status":"pending","priority":"high"}]}
ignorable: true
```

因此它随 session 一起持久化、随 fork 复制、随 `session/load` 回放，不依赖任何进程内 map。

## 2. 工具

| 工具 | 参数 | 说明 |
|---|---|---|
| `plan_set` | `title?`、`steps[{content,status?,priority?}]` | 创建或整体替换计划 |
| `plan_update` | `index`（1 开始）、`status` | 更新某一步 |
| `plan_show` | 无 | 查看当前计划 |

约束：最多 50 步、每步内容 ≤ 200 字符、标题 ≤ 100 字符；空状态补 `pending`，
空优先级补 `medium`；`plan_update` 的越界 index 与非法 status 都返回工具错误。

工具只能由 agent-loop 调用：它从调用 ctx 取会话（`session.FromContext`），
没有会话上下文或 `CallInfo.SessionID` 与会话不一致时直接拒绝，不猜会话。

## 3. 模型如何看到计划

`agent-loop.render` 在还原历史时只保留**最新**一条计划事件，并把它作为一条
system 消息插到「最后一条 user 消息之前」：

```text
# 当前计划: 发布
1. [x] 写代码 (high)
2. [~] 跑测试
```

这样每轮请求都带着当前计划，但不会因为历史里多次 `plan_set` 而重复刷屏。

## 4. ACP 映射

`control-acp` 把 `plugin:plan/updated` 映射成 ACP 的 plan 更新：

```json
{
  "sessionId": "mem-1",
  "update": {
    "sessionUpdate": "plan",
    "entries": [
      {"content": "写代码", "status": "completed", "priority": "high"},
      {"content": "跑测试", "status": "in_progress", "priority": "medium"}
    ]
  }
}
```

实时执行与 `session/load` 回放走同一套映射。

## 5. 接入方式

- `plan` 已注册为 builtin；
- preset 里显式启用（例如 `examples/acp/plugins/plan`）；
- 与 ACP 一起用时，`ada acp` 不需要额外配置：计划事件经 control 层进入
  `session/update`；
- TS 插件可 `import { get, set, update } from "ada:plan"` 读写当前会话计划，
  但必须在有会话上下文的工具调用里使用（见 §8）：

```ts
import { get, set, update } from "ada:plan"

export function apply(ctx: any) {
  ctx.tool({ name: "plan_demo", input: { type: "object" } }, () => {
    set({ title: "TS", steps: [{ content: "第一步" }, { content: "第二步" }] })
    update(1, "completed")
    return JSON.stringify(get())
  })
}
```

## 6. 会话上下文

`seam/session` 新增：

```go
ctx = session.WithSession(ctx, sess)
sess, ok := session.FromContext(ctx)
```

agent-loop 在 `Run` 开始时注入；作用域是一次 Run，不是跨会话索引。
这是给 Go 工具/策略写控制面事件用的；TS 工具目前拿不到 session，见
[seam-session.md](seam-session.md)。

## 7. 测试

```powershell
go test ./plugins/core/plan
go test ./plugins/core/agent-loop -run TestPlanInjectedBeforeLastUserMessage
go test ./plugins/provider/control-acp -run TestPlanMapping
go test ./platform/host/cli -run TestACPServe
```

- `plan_set/update/show` 与事件落盘、越界/非法参数、缺失会话上下文；
- 注入位置（system 消息在最后一条 user 消息之前）与只注入一次；
- ACP `plan` entries 映射；
- 真实 `ada acp` 会话里 `plan_set` 能到达客户端。

## 8. 边界

- 只保留最新计划；历史计划事件仍在 session 里，但不会全部注入请求。
- 没有自动推断步骤完成（例如工具成功 ≠ 步骤完成），状态由模型显式更新。
- 计划是会话级，不跨会话共享；需要跨会话的任务请用外部存储（如 `task-helper`）。
- TS 通过 `ada:plan` 读写计划，通过 `ada:session.current()` 拿会话身份，
  通过 `history` / `recent` 只读有界地读取事件（见 [bridge-session-store.md](bridge-session-store.md)）。
