# M4 流水线(flow)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §12.2(3)「流水线」与 §12.4「受限表达式求值」。
> 本页记录**已实现**的声明式子集与已知边界;`parallel`、`fanout`(parallel 的别名)、`forEach`、`Compensate`、`subflow` 已落地。

## 1. 范围

```
flow.json ──flow.Parse──▶ *Flow(数据)
                              │
                              ▼
                       flow.Run(ctx, Runtime, Options)
                              │
              ┌───────────────┴────────────────┐
              ▼                                ▼
   Runtime.CallService(service, method)   Runtime.DispatchEvent(name, mode)
              │                                │
              ▼                                ▼
   kernel.Context.Service(inject 门禁)     EmitContext / Waterfall
```

流水线是 **L2 基础设施(不是插件)**:它把「先调谁、再调谁、失败怎么办」变成数据;
真正的调用仍走内核的服务与事件(同一授权/预算/审计),因此产品差异不进入代码。

## 2. 数据模型

```go
type Flow struct {
    Name       string `json:"flow"`
    Steps      []Step `json:"steps"`
    OnFailure  []Step `json:"onFailure,omitempty"`
    Compensate []Step `json:"compensate,omitempty"` // 预留
}

type Step struct {
    ID         string        `json:"id"`
    Use        *UseStep      `json:"use,omitempty"`
    Event      *EventStep    `json:"event,omitempty"`
    Parallel   *ParallelStep `json:"parallel,omitempty"`
    Abort      string        `json:"abort,omitempty"`
    When       string        `json:"when,omitempty"`
    Retry      *Retry        `json:"retry,omitempty"`
    Timeout    Duration      `json:"timeout,omitempty"`
    Checkpoint bool          `json:"checkpoint,omitempty"`
}

type ParallelStep struct {
    Branches []Step `json:"branches"`
    Mode     string `json:"mode,omitempty"` // abort(默认) | collect
}

type ForEachStep struct {
    Over        string `json:"over"`        // "$.path",必须是数组
    As          string `json:"as,omitempty"` // 每项注入值表的变量名,默认 item
    Step        Step   `json:"step"`         // 模板步骤(不能再是 forEach)
    Concurrency int    `json:"concurrency,omitempty"` // 0 = 4,上限 32
}

type SubflowStep struct {
    Path  string         `json:"path"`            // 相对当前 flow 文件目录
    Input map[string]any `json:"input,omitempty"` // "$.path" 按父值表解析
}
```

- `use` / `event` / `parallel` / `abort` **四者互斥**;`when` 是真值门。
- `use.args` 里的字符串 `"$.step.field"` 会解析成上一步的结果(递归解析 map/数组)。
- `Duration` 在 JSON 里写 `"500ms"`、`"2s"`;写错在解析期报错。
- **未知字段一律报错**(流线是数据契约,拼错的字段不能被静默忽略)。

## 3. `when`:受限表达式

实现位于 `platform/expr/` 包(flow 与 bundle 共用)。只允许:路径(`$.step.field`)、字面量(字符串/数字/true/false/null)、比较(`== != > >= < <=`)、
逻辑(`&& || !`)、括号。**没有函数调用、没有赋值、没有 I/O**——数据不允许带执行。

| 规则 | 说明 |
|---|---|
| 路径 | 从值表里读 `step` 的字段;缺失路径视为 `null` |
| 相等 | 数字按数值比较,其余按深比较 |
| 大小 | 只比较数字与字符串;类型不可比时**报错**,而不是静默为假 |
| 短路 | `&&` / `||` 短路 |
| 条件类型 | 最终结果必须是布尔(或 `null` 视为假);数字等非布尔的裸条件报错 |
| 编译期 | 语法错误在 `Validate`/`Run` 开始前暴露 |

## 4. 执行语义

`parallel` 步骤:分支**并发**执行;
- `mode=abort`(默认):任一分支失败 → 取消其余分支,流线失败;
- `mode=collect`:等待全部分支,失败的错误记入父步骤结果 `{ok:false, errors:[...]}`,流线继续;
- 分支结果按分支 id 合并;**父值优先不被覆盖**;父步骤结果固定为 `{ok, errors}`;
- 分支内可以再写 `when`/`retry`/`timeout`;`checkpoint` 只支持在父步骤上。

`forEach` 步骤:
- `over` 必须是 `$.path` 且解析为数组;`as` 把当前项注入值表(默认 `item`);
- 模板步骤逐项执行,按 `concurrency` 节流(默认 4,上限 32);任一项失败 → 取消其余并让流线失败;
- 结果按索引收集到父步骤 `{ok:true, results:[...]}`。

`subflow`:
- `path` 只允许相对路径(拒绝绝对路径、反斜杠、`..` 逃逸);解析相对**当前 flow 文件**所在目录;
- `input` 里的 `$.path` 先按父值表解析,作为子流线的初始值;
- 子流线结果(值表)回填到父步骤 `values[step.ID]`,后续可 `$.sub.子步骤`;
- **环检测** + **嵌套上限 10**;子流线不继承 checkpoint(避免 RunID 冲突),继承 Clock;
- 子流线失败 → 父步骤失败(可被 `retry`/`timeout` 包裹)。

`Compensate`:
- 主链任一步失败后,**逆序**执行补偿步骤(尽力而为);补偿失败记入 `__compensateErrors`;
- 补偿步骤不进入 `Completed`;同样支持 `when`/`retry`/`timeout`。

| 能力 | 语义 |
|---|---|
| 顺序 | 严格按 `steps` 顺序;每步结果写入值表 `values[step.id]` |
| `when` | 为假 → 跳过并记入 `Skipped`,不再执行动作 |
| `abort` | 执行到该步且 `when` 为真 → 返回 `EABORTED`(可携带原因) |
| `retry` | `max` 是**重试次数**,总尝试 = `max+1`;`backoff` 用注入的 `Clock` 等待 |
| `timeout` | 每次尝试独立 `context.WithTimeout`;实现必须尊重 `ctx` |
| `checkpoint` | 该步成功后写检查点(`Store` + `RunID`);写失败按运行失败处理 |
| `resume` | 相同 `RunID` 再跑:跳过已完成步骤并恢复值表;检查点的流线名必须一致 |
| `OnFailure` | 主链失败后按顺序执行;结果写入 `values["__failureErrors"]` |

`Run` 返回 `Result{Values, Completed, Skipped}`,失败时也返回(便于诊断)。

## 5. Runtime:与内核的接线

```go
type Runtime interface {
    CallService(ctx context.Context, service, method string, args map[string]any) (any, error)
    DispatchEvent(ctx context.Context, name, mode string, input any) (any, error)
}
```

- `flow.KernelRuntime(ctx)` 用插件上下文实现:
  - `CallService` 走 `ctx.Service`(**inject 门禁**),服务必须实现 `flow.Invoker`:
    ```go
    type Invoker interface {
        Invoke(ctx context.Context, method string, args map[string]any) (any, error)
    }
    ```
  - `DispatchEvent` 的 `emit` 走 `EmitContext`,`waterfall` 走 `Waterfall`。
- 测试与宿主可替换 `Runtime`(例如只允许白名单服务的受限实现),Flow 数据不变。

## 6. 测试矩阵

| 场景 | 用例 |
|---|---|
| 路径/比较/逻辑/字面量/短路 | `flow.TestEvalExpressions` |
| 非法表达式、类型不可比、非布尔条件 | `flow.TestEvalRejectsBadExpressions` |
| 顺序执行 + `$.path` 参数解析 + event | `flow.TestRunSequential` |
| `when` 跳过 / `abort` 否决 | `flow.TestWhenSkipAndAbort` |
| retry 成功 / 用尽失败 | `flow.TestRetrySucceeds`、`flow.TestRetryExhausted` |
| 步骤超时 | `flow.TestStepTimeout` |
| checkpoint / resume | `flow.TestCheckpointResume` |
| FileStore 往返/runID 校验/原子写 | `flow.TestFileStoreRoundTrip`、`TestFileStoreRunID`、`TestFileStoreNoTemp` |
| OnFailure 步骤 | `flow.TestOnFailureSteps` |
| 声明式 JSON 解析与校验 | `flow.TestParseFlowJSON` |
| 并行分支结果合并 + 后续引用 | `flow.TestParallelBranches` |
| collect 模式错误收集、abort 模式失败 | `flow.TestParallelCollect`、`flow.TestParallelAbort` |
| 分支真的并发 | `flow.TestParallelConcurrent` |
| 声明式 parallel 解析/校验 | `flow.TestParseParallel` |
| forEach 结果收集与后续引用 | `flow.TestForEachResults` |
| forEach 并发上限 | `flow.TestForEachConcurrency` |
| forEach 失败点名索引 | `flow.TestForEachError` |
| 声明式 forEach 解析/校验 | `flow.TestParseForEach` |
| 补偿逆序执行 | `flow.TestCompensateReverse` |
| fanout 解析/校验/执行 | `flow.TestFanout` |
| subflow 加载/输入/结果回填 | `flow.TestSubflowRunsChild` |
| subflow 缺失文件 | `flow.TestSubflowMissingFile` |
| subflow 环检测 | `flow.TestSubflowCycle` |
| 声明式 subflow 解析/校验 | `flow.TestParseSubflow` |
| 与内核集成(服务 + waterfall) | `flow.TestKernelRuntimeIntegration` |

## 7. 已知边界与下一步

1. **fanout 是 `parallel` 的别名**:`{"fanout":{"branches":[...]}}` 与 `parallel` 等价(设计里的「并发分支 + join」);分支/项内/子流线内 `checkpoint` 不支持;值表是浅拷贝,分支共享的 map 值不可并发改写;subflow 之间没有共享 trace ID。
2. **没有子流水线**:`subflow` 与 trace 共享未实现。
3. **补偿已实现**:逆序、尽力而为;没有「补偿成功后回滚状态」的语义,也不保证重试全部补偿步骤。
4. **参数模板只有 `$.path`**:不支持在 args 里写表达式;需要时先做求值器扩展。
5. **检查点有内存与文件两种实现**:`MemoryStore`(测试)与 `FileStore`(JSON 文件、原子写、runID 白名单);没有跨进程锁与 fsync,`ctx.store` 仍需 store seam(M4.5)。
6. **没有 flow 服务插件**:目前由宿主/插件在代码里调用 `flow.Run`;还没有 `flow` 服务与清单入口。
7. **JS 侧不可用**:bridge 尚未暴露 flow API。

## 8. 验证命令

```powershell
go test -race -count=1 ./flow
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```