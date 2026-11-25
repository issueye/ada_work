# kernel API(M1 冻结版)

> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §4–§8。本页是 M1 期间**唯一允许实现**的 API 面:只包含内核机制,不含任何 seam、jsrt、assemble。
> 规则:本页先改,代码后改(AGENTS.md §7)。
> 破坏性变更记录:[ADR 0003](adr/0003-handler-context-and-timeout-gate.md)(监听器签名与 waterfall 超时闸门)。

## 0. 已拍板的六个决定

| # | 决定 | 取值 |
|---|---|---|
| D1 | 模块路径 / Go 版本 | `github.com/issueye/ada_go_v2`,`go 1.27` |
| D2 | Schema 子集 | `type`、`properties`、`required`、`default`、`enum`、`const`、`items`、`minimum`、`maximum`、`minLength`、`maxLength`、`pattern`、`additionalProperties`、`oneOf`、`minItems`、`maxItems`、`$defs`/`$ref`(仅文档内) |
| D3 | 服务表 API | 泛型 `Service[T](ctx, name)` + `TryService`;注册用 `Provide(name, any, opts...)`,值类型在解析时校验 |
| D4 | 错误包位置 | `platform/kernel/errors.go`:`Code` 枚举 + `*kernel.Error`;seam 各自定义自己的错误 |
| D5 | 日志 | `log/slog`,`ctx.Log()` 返回带 `plugin` 属性的 logger |
| D6 | 监听器签名 | `Handler(ctx, *Context, args)`、`WaterfallHandler(ctx, *Context, input, Next)`;超时即取消,waterfall 超时后拒绝 `Next`(ADR 0003) |

## 1. M1 的范围(与不做)

做:Context 派生、Fiber 六态、Service 表、Effect 栈、五种事件分发、Config、Schema、App/Inspect、Go 插件演示、生命周期测试。

不做:`plugins/seam/*`、`provider/*`、`policy/*`、`compile`、`jsrt`、`assemble`、`flow`、`sched`、`platform/host/*`;不引入任何第三方依赖。

**并发模型(M1 明确假设)**:生命周期操作(挂载/卸载/重载/派发)在**单一控制协程**调用,与设计 §3.2 的 kernel goroutine 一致;`-race` 只需覆盖「注册表读取 vs 控制协程写入」这一层。跨协程调用在 M2 接入执行层时统一加锁。

**监听器的并发例外**:每次调用监听器都会起一个 goroutine(为了能对单个监听器计时与取消),内核只保证「分发调用返回时已决定是否继续」,不保证超时监听器已经停止——见 §3.7 与 §6 边界。

## 2. 类型与签名

```go
package kernel

// 插件接口:Go 插件与(未来)JS 适配器共用
type Plugin interface {
    Name() string
    Inject() Inject
    Provide() []string
    Config() *schema.Schema
    Apply(ctx *Context, cfg Config) (Dispose, error)
}

type Inject struct {
    Names      []string       // 必需依赖;缺失则 fiber 停在 pending
    Optional   []string       // 可选依赖;缺失也激活
    MinVersion map[string]int // 期望的服务契约主版本
}

type Dispose func(context.Context) error

// 上下文:不可变派生
type Context struct{ /* 私有 */ }

func (c *Context) App() *App
func (c *Context) Fiber() *Fiber
func (c *Context) Entry() EntryMeta
func (c *Context) Extend(EntryMeta) *Context
func (c *Context) Isolate(name, label string) *Context
func (c *Context) Intercept(name string, cfg ServiceConfig) *Context
func (c *Context) ServiceConfig(name string) ServiceConfig

func (c *Context) Service(name string) (any, error)      // 必须已 inject
func (c *Context) TryService(name string) (any, bool)    // 绕过可见性(可选依赖/诊断)
func (c *Context) Provide(name string, v any, opts ...ProvideOption) (Dispose, error)
func (c *Context) Set(name string, v any) error           // 仅提供者可写
func (c *Context) Effect(label string, fn func() (Dispose, error)) (Dispose, error)

func (c *Context) Plugin(p Plugin, cfg Config) (*Fiber, error)
func (c *Context) Dispose(ctx context.Context) error

// 事件:五种分发;监听器拿到本次派发的 ctx
type Handler func(ctx context.Context, c *Context, args []any) (any, error)
type WaterfallHandler func(ctx context.Context, c *Context, input any, next Next) (any, error)
type Next func(any) (any, error)

func (c *Context) On(name string, fn Handler, opts ...OnOption) Dispose
func (c *Context) OnWaterfall(name string, fn WaterfallHandler, opts ...OnOption) Dispose
func (c *Context) Emit(name string, args ...any)                          // 无调用方 ctx
func (c *Context) EmitContext(ctx context.Context, name string, args ...any)
func (c *Context) Parallel(ctx context.Context, name string, args ...any) error
func (c *Context) Serial(ctx context.Context, name string, args ...any) error
func (c *Context) Bail(ctx context.Context, name string, args ...any) (any, error)
func (c *Context) Waterfall(ctx context.Context, name string, input any, args ...any) (any, error)

func (c *Context) Log() *slog.Logger
func (c *Context) Config() Config

// Fiber:六态 + epoch
type State int // pending / loading / active / failed / unloading / disposed

func (f *Fiber) ID() string
func (f *Fiber) Plugin() Plugin
func (f *Fiber) State() State
func (f *Fiber) Epoch() uint64
func (f *Fiber) Ready(ctx context.Context) error   // active→nil;failed→错误;pending→CodePending
func (f *Fiber) Reload(ctx context.Context) error
func (f *Fiber) Update(cfg Config) error
func (f *Fiber) Dispose(ctx context.Context) error  // 返回副作用回收的聚合错误

// App
func New(opts Options) *App
func (a *App) Root() *Context
func (a *App) Plugin(p Plugin, cfg Config) (*Fiber, error) // 挂在根作用域
func (a *App) Fiber(id string) (*Fiber, bool)
func (a *App) Inspect() Snapshot
func (a *App) Stop(ctx context.Context) error

// 诊断快照
type Snapshot struct {
    Fibers   []FiberInfo    // 只含仍在注册表的实例
    Services []ServiceInfo
	Effects  []EffectInfo
    Events   []string
}
type FiberInfo struct{ ID, Plugin, State string; Epoch uint64; Parent string }

// EffectInfo 是诊断快照里的一个副作用登记项(标签与归属,不含 disposer)。
type EffectInfo struct{ Fiber, Plugin, Label string }

// Fiber.Touch()/LastActive() 与 App.ReclaimIdle 见 §3.11。
```

## 3. 语义要点(实现必须遵守)

1. **依赖门控**:`reconcile` 计算 epoch = 所有必需依赖实现 UID 的哈希;缺失 → `pending`;变化 → 重载(卸载→加载)。
2. **单写者**:`Set` 类 API 只有提供者 fiber 可用;重复 `Provide` 同名同隔离槽 → `CodeServiceRegistered`。
3. **可见性**:`Service` 只允许访问 `Inject` 声明过的名字,否则 `CodeNotInjected`。`TryService` 是**文档化的绕过入口**,只给「可选依赖探测」与诊断用;根上下文(平台上下文)不做 inject 检查。业务代码不应依赖它绕过门禁(P6 依赖永不静默)。
4. **Effect**:后进先出;fiber 卸载时按逆序执行,每个有超时(默认 5s),panic 被捕获为 `CodePanic`;**回收错误聚合返回,并保留各条目的原始错误码**(不会一律改写成 `EINVALID`)。
5. **依赖方先卸载**:提供者的 `teardown` 先遍历**它自己提供的服务**,把这些服务的依赖方置为 `pending`(触发卸载),再解除自身对上游服务的依赖,最后逆序回收。卸载一个消费者**不得**影响共享同一依赖的兄弟插件。
6. **失败可复活**:`Apply` 失败后作用域会被换新,`Reload`/`Update`/依赖变化都能重新装配。同一依赖指纹下不自动重试——否则必然失败的插件会把 `recompute` 拖进死循环。
7. **事件**:默认按 `priority` 降序、同级按注册顺序;`emit` 吞错并记日志;`parallel` 聚合错误;`serial` 遇错即停;`bail` 取首个非 nil 结果;`waterfall` 不调 `Next` 即短路。
   - 监听器收到「父 ctx + 本监听器超时」派生的子上下文,**超时即取消**;
   - 单监听器超时默认 200ms,`WithTimeout` 可放宽,**上限 2s**(超出被截断);
   - **waterfall 超时闸门**:本次派发超时后,`Next` 一律返回 `CodeTimeout`,下游绝不会被执行——无论监听器是否遵守 ctx。waterfall 是策略/拦截唯一原语,超时后放行下游等于 fail-open(AGENTS P7);
   - `Emit` 不携带调用方上下文;需要取消传导时用 `EmitContext`;
   - `Stop` 之后事件总线封口,不再派发。
8. **Config**:先用 schema 补默认值并校验,再交给 `Apply`;校验失败 → `CodeConfig`,不进入 `Apply`。
9. **停机与注册表**:`Fiber.Dispose` 与 `App.Stop` 都会把实例从注册表移除,`Inspect` 不会列出已卸载的 fiber;`Stop` 之后 `Inspect` 为空。
10. **可观测**:`Inspect` 只返回快照,不返回 disposer;`Effects` 按登记顺序列出每个 fiber 当前登记的副作用标签,卸载后立即消失。
11. **闲置回收**:`Fiber.Touch()` 标记活动(服务解析自动 touch);`App.ReclaimIdle(ctx, ttl, now)` 回收超过 ttl 无活动且没有 dependents 的 fiber,返回被回收的 id。`now` 由调用方传入,便于测试;有依赖者的提供者分轮回收,避免一次调用拆散依赖链。
12. **调用者身份**:`Context.Principal()` 返回插件名,`Context.Context()` 返回携带 principal 的 `context.Context`;`kernel.Principal(ctx)`/`kernel.WithPrincipal(ctx,id)` 读写。安全策略(如 `policy/caps-gate`)据此做能力授权;缺少 principal 必须按拒绝处理。

## 4. 错误码(M1 子集)

`EINVALID` / `ENOTINJECTED` / `ESERVICE` / `ESERVICEINUSE` / `ENOTPROVIDER` / `EEFFECTDISPOSED` / `EFIBERDISPOSED` / `EFIBERFAILED` / `EPENDING` / `ECONFIG` / `ETIMEOUT` / `EPANIC` / `ECYCLE`(预留,环检测在 M3 装配报告)/ `EUNIMPLEMENTED`

## 5. 验收(M1 DoD)

- 20 个 Go 插件按依赖拓扑激活,挂载顺序任意(`TestTwentyPluginTopology`);
- 提供者缺失时消费者 `pending`,提供者上线后自动激活;提供者下线后消费者回到 `pending`;
- Effect 逆序回收、父子级联;卸载错误聚合上报;重载 100 次无重复监听、无残留副作用;
- 卸载消费者不影响兄弟插件;失败的插件可经 `Reload` 复活;
- waterfall 超时后下游不执行;监听器在超时时收到 ctx 取消;
- `Inspect` 能列出 fiber/服务/effect 标签,卸载后立即消失;
- `go test -race ./...` 全绿;
- `platform/kernel/` 无 agent 词汇、无 `os/exec|net/http|os.Open`、生产代码 ≤ 3000 行(gofmt、交叉编译同属门禁)。

## 6. M1 实现状态(2026-11-19)

已落地:`platform/kernel/`(生产 2396 行,测试 1062 行)+ `platform/kernel/schema` + `cmd/ada-demo` + `scripts/guard.ps1` + `docs/adr/0003-handler-context-and-timeout-gate.md`。

```powershell
go test -race -count=1 -timeout 120s ./...   # 29 + 7 个用例全绿
go test -run xxx -bench . -benchmem ./kernel # 性能基线
go run ./cmd/ada-demo                        # 依赖门控、级联重载演示
pwsh scripts/guard.ps1                       # 门禁:词汇/依赖/gofmt/build/vet/staticcheck/race/交叉编译/预算
pwsh scripts/bench.ps1                       # 性能预算:≤20 μs/次、50 插件冷启动 ≤150 ms
```

本机性能基线(i5-13420H / windows-amd64,`pwsh scripts/bench.ps1`;预算来自 AGENTS §6.3):

| 基准 | 实测 | 预算 | 说明 |
|---|---|---|---|
| `BenchmarkServiceResolve` | **36 ns**,0 allocs | 1 µs | 服务解析 + 可见性检查 |
| `BenchmarkEmitSingleListener` | **1.46 µs**,10 allocs | 20 µs | 广播一个监听器(含计时 goroutine) |
| `BenchmarkWaterfallThreeLayers` | **4.72 µs**,36 allocs | 20 µs | 三层 waterfall |
| `BenchmarkMountFiftyPlugins` | **0.37 ms**,1463 allocs | 150 ms | 50 插件冷启动(每次重建 App) |

门禁:`pwsh scripts/bench.ps1` 校验绝对预算;同机回归用 `-WriteBaseline` 生成基线、`-Baseline` 做 ±15% 比对(跨机器基线会把「换了台快机器」误报成回归)。

本轮修复(每条都有回归用例):

| 缺陷 | 回归用例 |
|---|---|
| waterfall 超时后下游「幽灵执行」 | `TestWaterfallTimeoutBlocksDownstream` |
| 监听器无法取消、超时 goroutine 不被回收 | `TestListenerSeesCanceledContext` |
| 卸载消费者误拆共享依赖的兄弟插件 | `TestDisposeDoesNotTouchSiblings` |
| 提供者下线未正确通知依赖方 | `TestProviderDisposeStopsConsumer` |
| `failed` 插件永久无法复活 | `TestFailedFiberCanRecover` |
| 卸载错误被静默丢弃 | `TestDisposeReportsDisposerFailure` |
| fiber 注册表只增不减 | `TestDisposedFiberLeavesRegistry` |
| `Intercept` 合并方向相反 | `TestInterceptNearestWins` |
| `Stop` 后仍派发事件 | `TestStopSealsEventsAndRegistry` |
| schema 嵌套关键字/type 静默放行、`$ref` 不解析 | `platform/kernel/schema` 7 个用例 |

已知边界(文档化而非隐藏):

1. 生命周期操作假定在单一控制协程调用(见 §1);跨协程并发留给 M2。
2. `Failed` 的 fiber 在同一依赖指纹下不会自动重试,重试由 `Reload`/`Update`/依赖变化触发。
3. 隔离槽没有提供者时不回落到全局槽(有意的严格语义)。
4. 超时只能「停止等待」:不遵守 ctx 的监听器 goroutine 会继续跑到自己返回为止。waterfall 的下游由超时闸门保证不会被执行,但监听器自身的副作用无法撤销。
5. `$ref` 只解析文档内引用(`#/...`);外部引用在解析期报错,不存在的目标与成环在 `Apply`/`Validate` 时报错。
6. 循环依赖目前只表现为参与者全部 `pending`,环路径点名留给 M3 的装配报告;`CodeCycle` 已预留。
