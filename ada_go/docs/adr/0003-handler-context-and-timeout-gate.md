# ADR 0003:监听器接收 `context.Context`,waterfall 超时后拒绝下游

- 状态:已接受
- 日期:2026-11-19
- 复议日期:2026-12-19
- 触发条款:AGENTS.md §12.2(修改公开 API,破坏性)

## 背景

M0/M1 的 `Handler` 与 `WaterfallHandler` 只有 `(c *Context, args/input ...)` 参数,监听器拿不到本次派发的上下文。内核的超时实现是「另起 goroutine 执行监听器,主协程 `select` 等待」;`WithTimeout` 只让**调用方**不再等待,监听器本身不知道已经超时。

这带来两个可复现的后果:

1. **幽灵执行(真实缺陷)**:waterfall 监听器超时后,它仍会继续运行并调用 `next`,于是下游监听器在 `Waterfall` 已经返回 `ETIMEOUT` **之后**才执行。实测:第一层 `Sleep(150ms)`、超时 20ms,`Waterfall` 返回时下游未执行,300ms 后下游已执行。
   waterfall 是设计里唯一的策略/拦截原语(design §7.2、AGENTS P7),「超时即失败、下游照跑」意味着任何审批/沙箱闸门在超时路径上是 fail-open。
2. **不可取消**:监听器没有 ctx,无法在超时后释放自己持有的资源;P1-C 实测 200 次超时留下 202 个仍在运行的 goroutine。

## 决策

1. `Handler` 改为 `func(ctx context.Context, c *Context, args []any) (any, error)`。
2. `WaterfallHandler` 改为 `func(ctx context.Context, c *Context, input any, next Next) (any, error)`。
3. 传给监听器的 ctx 是「父 ctx + 该监听器超时」派生的子上下文;**超时即取消**,监听器应当据此退出并释放资源。
4. waterfall 增加**超时闸门**:超时后 `next` 一律返回 `CodeTimeout`,不再进入下游。这是与监听器是否遵守 ctx 无关的确定性约束——即使监听器忽略取消信号,下游也不会被执行。
5. 分发结果改为经**带缓冲 channel** 回传,超时后被放弃的 goroutine 不会阻塞在发送上。
6. `Emit` 增加 ctx 版本 `EmitContext`;`Emit` 保留为「无调用方上下文」的语法糖。`App.Stop` 之后事件总线**封口**,不再派发。

## 替代方案

1. **不传 ctx,只做超时闸门**(最小改动):能消除幽灵执行,但监听器仍无法取消;M2 的 moejs `Interrupt`、M5 的沙箱/工具执行都需要一个可取消信号,届时仍要改签名,等于把破坏性变更推迟到有外部插件之后。否决。
2. **把本次调用的 ctx 挂在 `*Context` 上**(如 `c.Ctx()`):不破坏签名,但 `*Context` 是插件实例级、可并发复用的不可变值,把「每次派发的临时上下文」挂上去会造成并发下的语义混乱(同一 `*Context` 在不同派发里对应不同 ctx)。否决。
3. **监听器改为 `func(context.Context, *Event) error` 的强类型形态**(design §7.1 的方向):更安全,但事件类型化是 M3/M4 的独立议题,先做会让本次修复牵扯面过大。保留为后续演进方向。

## 影响面

- **破坏性**:所有 `ctx.On` / `ctx.OnWaterfall` 回调签名都要加首个 `ctx` 参数。
- 受影响代码:`kernel/` 实现、`kernel/kernel_test.go`、`cmd/ada-demo`;仓库内暂无第三方插件,故无迁移负担。
- 文档:同步更新 [kernel-api.md](../kernel-api.md) §2/§3、[plugin-kernel-design.md](../plugin-kernel-design.md) §4.2/§7.2/§7.3。
- 语义变化:超时不再只影响调用方,而是**确定性阻断下游**;这是收紧,不是放松。

## 回滚方式

单提交可回滚:恢复 `Handler`/`WaterfallHandler` 签名(去掉首个 ctx 参数)、删除 waterfall 的超时闸门、`EmitContext` 退化为 `Emit` 内部直接 `context.Background()`。回滚后重新暴露幽灵执行与不可取消两个问题,因此不建议回滚,除非 M2 确定改用方案 3。
