# ada v2 平台 seam 契约（llm / tools / session / prompts）

> 配套文档：[plugin-kernel-design.md](plugin-kernel-design.md)。本文是 §12.6「三段式 seam / provider / policy」的第一份落地契约，目标是**在写实现之前把四个最关键的 seam 冻结**，让 agent core 插件、provider 插件、policy 插件可以并行开发。

| 项 | 值 |
|---|---|
| 状态 | 契约 v1;session/prompts/llm/tools 第一刀已实现(llm 含 replay + OpenAI 兼容 provider,见 [m4.5-agent-core.md](m4.5-agent-core.md)) |
| 适用版本 | `apiVersion: 2`、`engines.ada >=2.0.0` |
| 依赖 | kernel（Service/Effect/Event/Config）、caps（授权）、compile/jsrt（无关） |
| 参照 | DSH `dsh-llm` / `dsh-tools` / `dsh-session` / `dsh-system-prompt` 的 seam 划分 |

---

## 0. 为什么先冻结这四个

agent core 拆成插件之后，插件之间的**唯一硬契约**就是 seam。四个 seam 覆盖了 agent 一轮的全部关键路径：

```
用户消息 ──▶ session.append(user/message)
              │
              ▼
        prompts.assemble ──▶ llm.stream ──▶ tools.invoke ──▶ session.append(tool/result)
              ▲                    │              │
              └────── session.events（唯一事实来源）◀──┘
```

| seam | 回答 | 没有它会怎样 |
|---|---|---|
| `session` | 发生过什么、模型能看到什么 | 每个插件各存一份状态，fork/resume 失效 |
| `prompts` | 这一轮的系统提示是什么 | 提示词拼接散落在各插件，顺序不可控 |
| `llm` | 模型怎么调、流怎么来的 | provider 与循环耦合，换模型要改循环 |
| `tools` | 工具有哪些、怎么执行、谁能拦 | 工具注册与执行散落，权限/超时无处统一 |

四个都必须在**无 provider 时空转**（返回 `EUNIMPLEMENTED`），产品才能只装一部分也能起来。

---

## 1. 通用约定（所有 seam 共用）

### 1.1 服务与版本

```go
// kernel 侧
type ServiceSpec struct {
    Name    string // "llm" | "tools" | "session" | "prompts"
    Version int    // 契约主版本；本文全部为 1
    Kind    string // "object"
}
```

- seam 插件 `provide` 一个**只有接口、没有实现**的服务（`seam/llm`）。
- provider 插件 `inject` seam，向 seam 注册实现：`llm.Providers().Register(name, impl)`。
- 消费者 `inject` seam（不是 provider）：循环只认 `llm.LLM` 接口。
- 版本不匹配 = 依赖未满足（fiber `pending`），不是运行期 panic。
- 一个 seam 可以有多个 provider：由 `policy/llm-router` 或 seam 内的选择规则挑一个（DSH 的做法是「注册顺序无关的选择」）。

### 1.2 错误分类（跨 seam 统一）

```go
type Error struct {
    Code      Code           // 见下
    Message   string
    Retryable bool
    Provider  string
    Details   json.RawMessage
}
```

| Code | 含义 | 谁产生 | 消费者动作 |
|---|---|---|---|
| `EUNIMPLEMENTED` | seam 在位、无 provider | seam | UI/CLI 显示「未配置后端」 |
| `EUNAVAILABLE` | provider 不可达 | provider | 按 `Retryable` 重试 |
| `EINVALID` | 请求非法（schema/参数） | seam 校验层 | 不重试，报给调用方 |
| `EAUTH` | 凭据无效 | provider | 触发授权流程（`seam/authorization`） |
| `ERATELIMIT` | 限流 | provider | 退避重试 |
| `ECONTEXT` | 上下文超长 | provider | 触发压缩（`core/compaction`） |
| `ECANCELED` | 调用方取消 | kernel | 不重试，不记失败（熔断不计） |
| `ETIMEOUT` | 超时 | kernel budget | 按策略 |
| `EDENIED` | 审批/guard 拒绝 | policy | 作为结果返回（`status=denied`） |
| `ESCHEMA` | 工具输出不符合 schema | seam/tools | 作为工具失败结果 |
| `EINTERNAL` | 实现内部错误（细节不进边界） | provider | 不重试，按通用失败处理 |
| `ELIMIT` | 超出配额/队列/帧上限 | provider | 退避或降级，不无限重试 |

### 1.3 上下文、取消与预算

- 所有 seam 方法都收 `context.Context`；实现**必须**尊重 `Done`。
- 内核在 seam 调用外再包一层预算（超时/配额/熔断/审计），实现无需重复计时。
- **不要**在 seam 实现里 sleep 轮询；长任务用内核的 `hostAsync` 模式（暂停预算计时）。
- 取消的语义：`ECANCELED` 不记 provider 失败（熔断层已区分）。

### 1.4 数据边界

- Go 侧：强类型 struct。
- JS 侧（TS 插件消费 seam）：JSON 值 + Promise；**函数值、流对象、生成器不可跨边界**。
- 大对象（图片/附件）走 `seam/storage` 的资源 id，不塞进 JSON。

### 1.5 观测

每个 seam 方法自动获得一个 span：`seam=<name> provider=<id> op=<method>`，并记录 `bytes_in/out`、`retryable`、`usage`（LLM 额外记 token）。seam 实现只需调 `ctx.Log()`。

### 1.6 契约测试

每个 seam 提供一个 `seamtest` 包（conformance suite），provider 必须全绿才能进 preset：

```go
func TestProvider(t *testing.T) {
    seamtest.LLM(t, seamtest.LLMOptions{
        Provider: myProvider{},
        RequiresAuth: true,     // 需要凭据则测 EAUTH 路径
        Streaming: true,
        Cases: []string{"basic", "stream", "cancel", "context-limit", "tool-call"},
    })
}
```

### 1.7 事件命名：两个命名空间

事件名一律 `<域>/<动词>`，但**分属两个命名空间**，不得复用：

| 命名空间 | 例子 | 谁定义 |
|---|---|---|
| **总线事件**（`ctx.Emit/On/Waterfall`） | `llm/request`、`tools/pre-execute`、`fs/observe`、`turn/before` | 各 seam / core 契约 |
| **会话事件类型**（写入 session 的 `Event.Type`） | `user/message`、`assistant/message`、`tool/result`、`turn/end` | `seam/session` §4.3 |

规则：

- 总线事件用**服务复数**作域（`tools/*`、`sources/*`），会话事件类型用**单数**（`tool/result`），避免同名。
- 新增任何事件名必须先写进契约文档（AGENTS.md §7），命名不可复用。


---

## 2. `seam/llm`

### 2.1 Go 接口

```go
package llm

type LLM interface {
    // Models 列出该 provider 能提供的模型（含上下文窗口、能力位）。
    Models(ctx context.Context) ([]ModelInfo, error)

    // Complete 一次非流式补全（简单路径；内部仍需支持工具调用）。
    Complete(ctx context.Context, req Request) (Response, error)

    // Stream 流式补全：每个增量调 onDelta，最终返回完整 Response。
    // onDelta 必须尽快返回；阻塞会拖住循环。
    Stream(ctx context.Context, req Request, onDelta func(Delta) error) (Response, error)
}

type ModelInfo struct {
    ID            string   // "deepseek-chat"
    ContextWindow int
    MaxOutput     int
    Capabilities  []string // "tools" | "vision" | "stream" | "json"
    Pricing       *Pricing // 可选
}

type Request struct {
    Model       string
    Messages    []Message
    Tools       []ToolSchema   // 由 seam/tools 提供
    ToolChoice  ToolChoice     // auto | none | required | {name}
    Temperature *float64
    TopP        *float64
    MaxTokens   int
    Stop        []string
    SessionID   string
    TurnID      string
    StepID      string
    Metadata    map[string]string
}

type Response struct {
    Message   Message
    ToolCalls []ToolCall
    Usage     Usage
    Finish    FinishReason // stop | length | tool_calls | content_filter | error
    Raw       json.RawMessage
}

type Message struct {
    Role    Role     // user | assistant | tool | system
    Content []Block
}

type Block struct {
    Kind     BlockKind // text | image | tool_result | reasoning
    Text     string
    Resource string    // image: storage id
    MimeType string
    Meta     map[string]string
}

type ToolCall struct {
    ID       string
    Name     string
    Args     json.RawMessage
    Index    int
}

type Delta struct {
    Kind      DeltaKind // text | reasoning | tool_args | usage | done
    Text      string
    ToolIndex int
    ToolID    string
    Usage     *Usage
}

type Usage struct {
    InputTokens  int
    OutputTokens int
    CacheRead    int
    CacheWrite   int
}
```

### 2.2 事件与策略链

| 事件 | 模式 | 用途 |
|---|---|---|
| `llm/request` | Waterfall（`*Request`） | 改写请求：截断、注入系统消息、切模型 |
| `llm/stream` | Waterfall（`*Delta`） | 过滤/改写增量（脱敏、翻译、限速） |
| `llm/response` | Emit（`*Response`） | 用量统计、成本、审计 |
| `llm/error` | Emit（`*llm.Error`） | 告警、降级切换 |

策略插件示例（全部是 `policy/*`，不碰 provider）：

- `policy/llm-retry`：监听 `llm/error`，按 `Retryable` 退避重发（DSH `dsh-llm-retry` 同款）。
- `policy/llm-token-cap`：在 `llm/request` 里按 `ModelInfo.MaxOutput` 夹 `MaxTokens`。
- `policy/llm-router`：多 provider 时按 `ModelInfo` 与配置选实现。
- `policy/llm-fallback`：主 provider `EUNAVAILABLE` 时切备。

### 2.3 provider 契约

1. **注册即声明**：`Register(name, impl, ProviderInfo{Vendor, Models, Version})`。
2. **必须支持取消**：`Stream` 在 `ctx.Done` 后尽快返回 `ECANCELED`，不泄漏连接。
3. **必须支持工具调用**：`Response.ToolCalls` 与 `Delta{tool_args}` 都要能给。
4. **错误映射**：HTTP 状态/服务商错误 → §1.2 的 Code（不要抛裸 error）。
5. **测试性**：`provider/llm-replay` 用录制的 JSONL 回放（DSH `dsh-llm-replay` 的思路），供无需 key 的快照测试。

### 2.4 配置

```jsonc
{ "id": "llm-deepseek", "plugin": "provider/llm-deepseek",
  "config": { "endpoint": "https://api.deepseek.com", "defaultModel": "deepseek-chat",
              "timeout": "120s", "maxRetries": 0 } }
```

`maxRetries` 默认 **0**：重试是 `policy/llm-retry` 的事，provider 重试会与策略叠加成双倍。

### 2.5 验收用例（`seamtest.LLM`）

| 用例 | 断言 |
|---|---|
| basic | 一次 Complete 返回非空 Message 与 Usage |
| stream | 至少 3 个 delta，顺序单调，最后一个 `done` |
| tool-call | 返回 ToolCall，`Args` 是合法 JSON |
| cancel | 取消后 ≤200 ms 返回 `ECANCELED`，无后台 goroutine 残留 |
| context-limit | 超长请求返回 `ECONTEXT`（由 provider 或 token-meter 判定） |
| auth | 无效凭据返回 `EAUTH`，不重试 |

---

## 3. `seam/tools`

### 3.1 Go 接口

```go
package tools

type Tools interface {
    // Register 注册一个工具；返回 disposer。同名重复注册失败。
    Register(def ToolDef) (kernel.Dispose, error)

    // List 返回当前 agent 可见的工具（已应用 restrict 与 guard 的可见性）。
    List(ctx context.Context) ([]ToolInfo, error)

    // Restrict 只做「移除」：之后的 List 里不再出现这些名字（DSH ctx.tools.restrict 同款）。
    Restrict(names ...string) (kernel.Dispose, error)

    // Guard 是同步拒绝：无论注册顺序如何都生效，适合硬性禁止。
    Guard(fn Guard) (kernel.Dispose, error)

    // Invoke 执行一个工具。管线见 §3.3。
    Invoke(ctx context.Context, call CallInfo) (Result, error)
}

type ToolDef struct {
    Name     string
    Summary  string
    Input    *schema.Schema
    Output   *schema.Schema // 可选
    ReadOnly bool
    Timeout  time.Duration  // 0 = 用内核默认（30s）
    Source   string         // 注册它的插件 id（内核填）
    Tags     []string
}

type ToolInfo struct {
    ToolDef
    Visible bool
}

type CallInfo struct {
    ID        string // 模型给的 tool_call id
    Name      string
    Args      json.RawMessage
    SessionID string
    TurnID    string
    StepID    string
    Origin    string // "model" | "user" | "plugin"
}

type Result struct {
    Content   []llm.Block   // 复用 llm 的 Block，text/image/tool_result
    Details   json.RawMessage
    Status    Status        // ok | denied | error | timeout
    Terminate bool          // 该结果让循环结束（DSH 的 terminate 语义）
    Usage     *llm.Usage
}

type Guard func(ctx context.Context, call CallInfo) error // 返回 EDENIED 即拒绝
```

### 3.2 工具结果与模型看到的形态

- `Result.Content` 直接进 session（`tool/result` 事件），下轮由 `session`→`llm.Request` 还原。
- `Details` 只给 UI/诊断，**不进模型上下文**（避免把调试信息喂给模型）。
- `Status=denied|timeout|error` 也要以 `tool_result` 形态回给模型（模型需要知道失败原因），但不再重试。

### 3.3 执行管线（固定顺序，插件在固定点介入）

```
Invoke
  ├─ 1. 可见性：List/Restrict 决定「这个工具对该 agent 是否可见」        （同步）
  ├─ 2. guard 链：任一 Guard 返回错误 → Status=denied，不进 handler     （同步）
  ├─ 3. tools/pre-execute（Waterfall）：可改写 args、可返回 ask 请求审批 （可异步）
  ├─ 4. 参数校验：Input schema；失败 → ESCHEMA，返回 error 结果         （同步）
  ├─ 5. tools/execute（Waterfall）：包裹实际执行（可重试、可替换）       （可异步）
  │      └─ handler（Go 函数 或 JS 插件方法），受 Timeout/预算约束
  ├─ 6. tools/post-execute（Waterfall）：可变换 Result（截断/脱敏/富化）  （可异步）
  └─ 7. tools/result（Emit）：最终结果观察点（遥测、审计、成本）          （发出即返回）
```

与 DSH 的对应：`tools/pre-execute`、`tools/execute`、`tools/post-execute` 是 waterfall；`tools/result` 是观察点；「拒绝必须与顺序无关」用 `Guard`，「按 agent 隐藏工具」用 `Restrict`。

### 3.4 审批接入（policy 而非 seam）

审批**不是 tools 的一部分**：`policy/approval-gate` 在 `tools/pre-execute` 里调 `approval` 服务，返回「拒绝」或「允许」：

```go
kernel.WaterfallOn(ctx, tools.PreExecute, func(c *kernel.Context, call *tools.CallInfo, next kernel.Next) (*tools.CallInfo, error) {
    if !risk(call) { return next(call) }                       // 低风险直接放行
    d, err := kernel.Service[approval.Approval](c, "approval").Ask(c, approval.Request{Tool: call.Name, Args: call.Args})
    if err != nil { return nil, err }                          // 失败关闭
    if !d.Allow { return nil, tools.Denied("user denied") }
    return next(call)
})
```

默认 **fail-closed**（与 DSH `dsh-user-approval` 一致）：审批服务不可用时拒绝，而不是放行。

### 3.5 验收用例（`seamtest.Tools`）

| 用例 | 断言 |
|---|---|
| register/dup | 同名重复注册返回错误；disposer 后工具消失 |
| restrict | restrict 后 List 不含该名，Invoke 返回 `EINVALID` |
| guard | Guard 拒绝与注册顺序无关；同步生效 |
| schema | 非法 args 返回 `ESCHEMA`，不调用 handler |
| timeout | handler 超时返回 `Status=timeout`，不泄漏 goroutine |
| post-execute | 变换生效，`tools/result` 看到变换后的结果 |
| denied | 审批拒绝时 handler 未被调用，结果仍回给模型 |

---

## 4. `seam/session`

### 4.1 不变量（比接口更重要）

1. **append-only**：会话是事件日志，只追加，不改写。
2. **唯一事实来源**：模型看到的任何内容都必须能从事件重建（fork / resume / replay）。
3. **未知事件必须可忽略**：插件不得发明 `type`；插件自有数据写 `plugin:<id>/...` 且 `ignorable=true`，旧读取器/旧版本才能继续打开会话。
4. **单写者**：一个 session 同时只有一个写入方（agent-loop 或其代理）。读者可任意多。
5. **顺序即因果**：`seq` 单调递增；跨插件的因果关系靠 `turn/step` 坐标表达，不靠时间。

### 4.2 Go 接口

```go
package session

type Session interface {
    ID() string
    Meta() Meta

    Append(events ...Event) ([]uint64, error)    // 返回分配的 seq
    Events(from uint64, limit int) ([]Event, error)
    Subscribe(from uint64) (<-chan Event, kernel.Dispose)   // 订阅（含历史回放）

    Turn() TurnInfo
    Fork(at uint64) (Session, kernel.Dispose, error)
    Compact(ctx context.Context, upTo uint64, summary Event) error

    Project(name string) (any, error)             // 见 §4.4
}

type Event struct {
    Seq       uint64
    TS        time.Time
    Type      string          // 见 §4.3 标准类型
    SessionID string
    TurnID    string
    StepID    string
    Source    string          // 插件 id 或 "core"
    Data      json.RawMessage
    Ignorable bool            // 未知读者可跳过
}

type TurnInfo struct {
    ID     string
    Index  int
    State  TurnState // running | done | aborted
    Started time.Time
}

type Meta struct {
    Title     string
    Workspace string
    Model     string
    Parent    string // fork 来源
}
```

### 4.3 标准事件类型

| Type | 载荷要点 | 谁写 |
|---|---|---|
| `user/message` | `Message`（llm 复用） | 入口/UI |
| `assistant/message` | `Message` + `Usage` | agent-loop |
| `tool/call` | `CallInfo` | agent-loop |
| `tool/result` | `tools.Result` | agent-loop |
| `turn/start` / `turn/end` | `TurnInfo` | agent-loop |
| `step/start` / `step/end` | 步骤坐标、耗时 | agent-loop |
| `compact/summary` | 被压缩范围 + 摘要事件 | `core/compaction` |
| `error` | `Error`（可重试/不可重试） | 任意 |
| `platform/plugin/<id>/<kind>` | 插件自有，`Ignorable=true` | 插件 |

> 约定：**模型可见的事件**只有 `user/message`、`assistant/message`、`tool/result`、`compact/summary`；其余是控制面，不拼进请求。

### 4.4 投影（projection）

派生状态（token 计数、turn 大纲、goal、团队任务板）不直接监听原始事件重扫，而是注册投影：

```go
type Projection interface {
    Name() string
    Version() int
    Apply(state any, ev Event) (next any, changed bool)  // 纯函数、同步；忽略则返回原引用
    View(state any) any
}
```

- 内核缓存 `(session, projection)` 的当前值；`changed=false` 时不传播。
- 版本变化（`Version` 变）自动作废旧检查点并重放。
- 持久化：`seam/storage` 里的检查点，冷读只重放尾段。

### 4.5 持久化

`provider/session-jsonl` 是默认后端：一行一事件、按 `seq` 追加、崩溃安全（`fsync` 策略可配）。`provider/session-sqlite` 为可选（查询/大历史）。**持久化不是 session seam 的职责**：seam 只定义读写契约，后端由 provider 注入。

### 4.6 验收用例（`seamtest.Session`）

| 用例 | 断言 |
|---|---|
| append 顺序 | seq 连续、并发写被拒绝（单写者） |
| 重建 | 从事件重建的请求与写入前一致（golden） |
| fork | fork 后父子互不影响；子可继续 append |
| 未知事件 | 注入 `plugin:x/y` + `ignorable` 后旧读取器仍能打开 |
| 投影 | `changed=false` 不触发下游；版本变化触发重放 |
| 冷启动 | 持久化后重开，尾段重放结果一致 |

> 实现进度、与契约的偏差以及 provider 约定见 [seam-session.md](seam-session.md)。

---

## 5. `seam/prompts`

### 5.1 Go 接口

```go
package prompts

type Prompts interface {
    // Section 注册一段系统提示；返回 disposer。
    Section(s Section) (kernel.Dispose, error)

    // Assemble 组装本轮的系统提示（走 prompts/assemble waterfall）。
    Assemble(ctx context.Context, in AssembleInput) (Assembly, error)

    // Budget 返回当前模型的提示预算（tokens）。
    Budget(ctx context.Context) (int, error)
}

type Section struct {
    Key    string    // 唯一键："identity" | "rules" | "tools" | "context" | "plugin:<id>"
    Title  string
    Body   string
    Order  int       // 越小越靠前；同 order 按 key 稳定排序
    Scope  Scope     // session | turn | step
    Media  []llm.Block
}

type AssembleInput struct {
    SessionID string
    TurnID    string
    StepID    string
    Model     llm.ModelInfo
    Budget    int
}

type Assembly struct {
    Sections []Section
    Text     string   // 渲染后的系统提示
    Tokens   int
    Dropped  []string // 因预算被裁掉的 key（可观测）
}
```

### 5.2 组装规则

1. 收集所有 `Scope` 匹配的 section，按 `Order, Key` 稳定排序。
2. 走 `prompts/assemble` 的 **waterfall**：监听者可整体替换 `Assembly`（如按模型重写），但**默认必须调 `next()`**。
3. 超预算：按 `Order` 从后往前裁，`Dropped` 记录被裁的 key（UI 显示「因预算省略」）。
4. 渲染结果写入 `session` 的**本轮上下文**（`step/start` 事件），保证 replay 一致。

### 5.3 边界（与 DSH 的教训一致）

- **加提示用 `Section`，不要监听 `assemble` 往里塞**：`assemble` 是整体替换点，用它加文本会破坏别的插件的顺序与预算。
- **不要用 prompts 改工具清单**：工具由 `seam/tools` 的 `List/Restrict` 决定；在提示里删工具描述只会让模型「看得见却调不到」。
- 每个 section 的 Key 全局唯一：`plugin:<id>/<name>` 避免与 core 冲突。

### 5.4 验收用例（`seamtest.Prompts`）

| 用例 | 断言 |
|---|---|
| 顺序稳定 | 同 order/同 key 多次 assemble 结果一致 |
| 预算裁剪 | 超预算时按 order 从后裁，`Dropped` 准确 |
| waterfall | 不调 `next` 即整体替换；调 `next` 正常合并 |
| 冲突键 | 重复 key 注册失败并点名现有 owner |
| replay | 同 session 重放得到同一 `Text` |

---

## 6. 四个 seam 如何撑起一轮 agent

`core/agent-loop` 是最小的消费方，骨架如下（证明四个 seam 足够）：

```go
func (l *Loop) Step(ctx context.Context) error {
    sess := l.session                 // seam/session
    evs, _ := sess.Events(l.cursor, 0)
    l.cursor += uint64(len(evs))

    asm, err := l.prompts.Assemble(ctx, prompts.AssembleInput{SessionID: sess.ID(), ...})
    if err != nil { return err }

    req := llm.Request{
        Model:    l.model.ID,
        Messages: l.render(sess, asm),              // 事件 → 消息
        Tools:    l.toolSchemas(ctx),               // List() → ToolDef.Input/Output 适配成 llm.ToolSchema
    }
    resp, err := l.llm.Stream(ctx, req, l.onDelta)
    if err != nil { return err }
    _, _ = sess.Append(session.Event{Type: "assistant/message", Data: mustJSON(resp.Message)})

    for _, call := range resp.ToolCalls {
        res, err := l.tools.Invoke(ctx, tools.CallInfo{ID: call.ID, Name: call.Name, Args: call.Args, ...})
        if err != nil && !errors.Is(err, tools.ErrDenied) { return err }
        _, _ = sess.Append(session.Event{
            Type: "tool/result",
            Data: mustJSON(map[string]any{"call": call, "result": res}),
        })
    }
    _, _ = sess.Append(session.Event{Type: "turn/end", Data: mustJSON(sess.Turn())})
    return nil
}
```

依赖关系（由 `inject` 推导，装配器排拓扑序，preset 只列集合）：

```
session ─┐
prompts ─┼─▶ agent-loop ─▶ tool/* · policy/*（按需）
llm ─────┘
   ▲
tools ──（List 提供 schema）
```

---

## 7. 与 DSH 包的对照

| 本方案 | DSH | 说明 |
|---|---|---|
| `seam/llm` + `provider/llm-*` + `policy/llm-*` | `dsh-llm` · `dsh-llm-deepseek` · `dsh-llm-retry` | seam/provider/policy 三段完全对应 |
| `seam/tools` + `executor/native` + `policy/tool-*` | `dsh-tools` · `dsh-tool-executor-native` · `dsh-tool-call-timeout-policy` | 管线事件名沿用 |
| `seam/session` + `provider/session-jsonl` + `core/compaction` | `dsh-session` · `dsh-session-persistence-jsonl` · `dsh-compaction-basic` | 事件溯源与投影一致 |
| `seam/prompts` + `policy/prompt-*` | `dsh-system-prompt` | `section()` 与 `assemble` 的关系一致 |
| `policy/approval-gate` | `dsh-user-approval` | fail-closed 默认一致 |
| `presets/*.preset.json` | `dsh-agent-preset` + `presets/*.patch.yml` | 数据组合，列表顺序不决定加载顺序 |

---

## 8. 版本演进规则

- seam 的 `Version` **只增不改**：破坏性变化 → `Version+1`，旧 provider 继续服务旧版本。
- kernel 允许同名的两个版本并存（`ServiceSpec{Name:"llm", Version:1}` 与 `2`），消费者声明期望版本；这样 provider 可以逐个升级。
- 每个 seam 的 `Version` 变化必须同时更新：`seamtest` 用例、本文档、TS d.ts、preset 的 `engines.ada`。
- 弃用：版本 N 的 seam 标记 `Deprecated` 后至少保留一个大版本，并在 `ada inspect` 里显示「谁还在用 N」。

---

## 9. 附录 A：TS 侧形态（`@ada/sdk` 生成的 d.ts 节选）

```ts
export interface LLM {
  models(): Promise<ModelInfo[]>
  complete(req: Request): Promise<Response>
  stream(req: Request, onDelta: (d: Delta) => void | Promise<void>): Promise<Response>
}

export interface Tools {
  register(def: ToolDef): Promise<Disposer>
  list(): Promise<ToolInfo[]>
  restrict(...names: string[]): Promise<Disposer>
  guard(fn: (call: CallInfo) => void | Promise<void>): Promise<Disposer>
  invoke(call: CallInfo): Promise<Result>
}

export interface Session {
  id: string
  append(...events: Event[]): Promise<number[]>
  events(from: number, limit?: number): Promise<Event[]>
  subscribe(from: number, fn: (ev: Event) => void): Promise<Disposer>
  fork(at: number): Promise<Session>
  project<T = unknown>(name: string): Promise<T>
}

export interface Prompts {
  section(s: Section): Promise<Disposer>
  assemble(input: AssembleInput): Promise<Assembly>
  budget(): Promise<number>
}

// 用法（TS 插件）
const tools = ctx.service<Tools>('tools')
ctx.effect('my tool', () => tools.register({ name: 'weather', input: schema /* … */ }))
```

跨边界注意：TS 侧的 `stream` 回调是异步函数，每帧都过一次 JSON 桥；**不要在 TS 里做高频 onDelta 聚合**，需要高频处理就放 Go 侧循环。

---

## 10. 附录 B：待决问题（评审时定）

1. **`llm.Request.Tools` 的类型**：直接复用 `seam/tools` 的 schema，还是 llm 自持一份「模型友好」的 schema？（倾向复用，避免两套描述不一致）
2. **session 的 `Compact` 归属**：放在 session seam（本文写法），还是 `core/compaction` 通过 append `compact/summary` 完成？（倾向后者，seam 只做 append）
3. **`prompts.Budget` 的来源**：由 `llm.ModelInfo` 提供，还是 `policy/context-budget` 插件计算？（倾向前者，policy 只做裁剪策略）
4. **tools 的 `Timeout` 默认值**：30s 是否统一，还是按 `ReadOnly` 区分（读 10s、写 120s）？
5. **`seam/session` 是否允许「多 session 共享一个 provider」**：本文默认允许，但 checkpoint 需要按 session 分区；确认存储布局。
6. **投影的持久化归属**：`seam/session` 还是独立的 `seam/projection`（DSH 是后者）？本文暂并入 session，若后续独立再拆。
7. **四个 seam 之后的下一个**：建议 `seam/fs` 与 `seam/sandbox`（DSH 里 policy 最厚的一层），需要单独一页契约。

---

## 附录 C：M4.5 实施顺序（可执行清单）

前提：M1（内核）与 M2（执行层）已完成；本清单只做「seam + 最小 agent core」，不含完整工具集与 UI。

| # | 任务 | 产出 | DoD（完成定义） | 估算 |
|---|---|---|---|---|
| C1 | `seam/session` + `provider/session-memory` + `provider/session-jsonl` | 事件信封、`Append/Events/Subscribe/Fork`、投影接口 | `seamtest.Session` 全绿；fork/重建 golden 通过 | 3 d |
| C2 | `seam/prompts` + `core/system-prompt` | Section 注册、assemble waterfall、预算裁剪 | `seamtest.Prompts` 全绿；同 session 重放同 Text | 2 d |
| C3 | `seam/llm` + `provider/llm-replay` + `provider/llm-openai` | 请求/响应类型、流式、用量、错误映射 | `seamtest.LLM` 全绿；replay 能跑无 key 快照 | 4 d |
| C4 | `seam/tools` + `executor/native` + `tool/text-helper` | 注册/restrict/guard/invoke、五段管线 | `seamtest.Tools` 全绿；管线事件顺序断言 | 3 d |
| C5 | `core/agent` + `core/agent-loop`（最小） | 一轮循环：assemble → stream → 工具 → append；取消/超时 | 跑通 1 个工具调用回合；取消后无 goroutine 泄漏 | 4 d |
| C6 | `presets/base.preset.json` + 集成测试 | 只含 C1–C5 的最小产品 | 一条用户消息 → 模型 → 工具 → 结果 → turn/end 的事件序列 golden | 2 d |
| C7 | 策略桩 | `policy/llm-retry`、`policy/tool-timeout`、`policy/approval-gate`（auto-allow / ask 两档） | 拒绝与超时路径有审计；重试不叠加 | 2 d |

**顺序为什么固定**：事件类型是四者的公共词汇——`session` 先定信封（`user/message`、`assistant/message`、`tool/call`、`tool/result`），`prompts` 才能写「本轮上下文」事件，`llm` 才能产出 `assistant/message`，`tools` 才能产出 `tool/result`，`agent-loop` 最后把它们串起来。反过来做会返工。

**C6 的验收 golden**（第一条端到端契约，冻结即可防回归）：

```
seq 1  user/message      "读取 a.txt 并总结"
seq 2  step/start        prompt.tokens=812
seq 3  assistant/message  tool_calls=[{name:"read_file", args:{path:"a.txt"}}]
seq 4  tool/call         read_file
seq 5  tool/result       status=ok, content=[{text:"hello"}]
seq 6  assistant/message  "文件内容是 hello"
seq 7  turn/end          state=done
```

任何 seam 或 core 改动的 PR 都要让这条 golden 保持不变（除非显式批准变更）。

