# bridge：`ada:session` 与 `ctx.store`

> 实现：`platform/bridge/session_store.go`；相关：[plan-plugin.md](plan-plugin.md)、[seam-session.md](seam-session.md)、[m2-execution.md](m2-execution.md)。

## 1. `ada:session`

```ts
import { current, append } from "ada:session"
```

| 方法 | 行为 |
|---|---|
| `current()` | 返回 `{id, title, workspace}`；没有会话上下文时返回 `null` |
| `append(kind, data)` | 追加一条插件自有 session 事件，返回 `seq` |
| `history(from?, limit?)` | 返回 `seq >= from` 的事件，默认 `from=0`、`limit=50` |
| `recent(limit?)` | 返回最近 N 条事件，默认 20 |

事件形状：`{seq, type, ts, turnId?, stepId?, source, ignorable?, data?}`，
`data` 保持原始 JSON。
读取限制：`limit` 最大 200、结果 JSON 最大 256 KiB；超限报错，按 `from`
分页而不是静默截断。没有会话上下文时 `history/recent` 都会报错。

`append` 的约束：

- 事件类型由宿主生成：`plugin:<插件 id>/<kind>`，并强制 `Ignorable=true`；
  TS **不能**伪造 `user/message`、`assistant/message`、`tool/*` 这些模型可见事件；
- `kind`：≤64 字节，只允许 `[a-z0-9._/-]`；
- `data`：合法 JSON，≤64 KiB（默认 `null`）；
- 只在有会话上下文的工具调用里可用；没有会话时报错。

用途：按会话键控的外部存储、记录插件自己的观察结果、给 `session.load`
留下可回放的插件事件。模型可见的对话消息仍由 `core/agent-loop` 写。

安全提示：事件里可能包含模型对话、工具参数与结果；脚本不得把 secret
写进事件、日志或 store。

## 2. `ctx.store` / `ada:store`

apply 期间可以用 `ctx.store`，工具里可以：

```ts
import { get, set, remove, list } from "ada:store"
```

两者是**同一个实例**（同一个 bridge 插件实例内）。

| API | 说明 |
|---|---|
| `get(key)` | 缺失返回 `null` |
| `set(key, value)` | 值是任意可 JSON 序列化的 JS 值 |
| `remove(key)` | 删除键 |
| `list(prefix?)` | 排序后的键数组 |

限制：key ≤128 字节、value ≤64 KiB、最多 1000 个键；超限报错，不截断。

生命周期：**进程内、随插件 fiber 卸载丢弃**。重启或重载后为空——
它不是持久存储；需要持久化时要等 storage seam，不要把 store 当数据库。

## 3. 失败语义

- 没有会话：`current()` 返回 `null`，`append()` 抛错；
- store 超限：抛错；
- 值里有函数/循环引用：序列化失败并抛错。

## 4. 示例

```ts
import { current, append } from "ada:session"
import { get, set } from "ada:store"

export function apply(ctx: any) {
  ctx.store.set("boot", Date.now())
  ctx.tool({ name: "demo", input: { type: "object" } }, () => {
    const sess = current()
    const n = Number(get("n") || 0) + 1
    set("n", n)
    if (sess) append("note", { sessionId: sess.id, n })
    return JSON.stringify({ sess, n })
  })
}
```

## 5. 测试

```powershell
go test ./platform/bridge -run 'TestJSSessionAndStore|TestJSSessionHistory|TestJSPlanModule'
go test ../ada_jsrt/jsrt -run TestHostPlanCallbacks
```

`TestJSSessionAndStore` 覆盖：会话身份、插件事件前缀与 `Ignorable`、
store 跨调用保留、`ctx.store` 与 `ada:store` 共享实例、无会话时 `current()=null`。
`TestJSSessionHistory` 覆盖：`history`/`recent` 的范围、原始 `data` JSON、
超限报错与无会话报错。

## 6. 边界

- 事件读取是只读且有界的（默认 50、最大 200 条、256 KiB）；
- 没有持久化 store（跨重启/跨进程）；
- store 只对同一插件实例可见，不是全局 KV；
- 插件事件不会进入模型上下文（`ModelVisible` 不包含 `plugin:*`）。
