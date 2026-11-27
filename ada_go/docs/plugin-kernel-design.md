# ada_go_v2 插件内核设计方案

> **一句话**：把 ada_go v1 里「清单 + 每插件一个 moejs 运行时 + 能力桥 + 装配事务」这套已验证的骨架，升级成 **Cordis 式的作用域内核**（Context / Fiber / Service / Effect / Event / Config），TypeScript 插件由 **esbuild 的 Go API 降级为 JS**（无 Node），由 **moejs** 在进程内执行，并在其上补三层真正的**编排**：装配编排、交互编排、调度编排。

| 项 | 值 |
|---|---|
| 状态 | 设计稿（待评审），未写实现 |
| 目标仓库 | `E:\codes\go_projects\ada_go_v2`（当前为空） |
| 依赖基线 | `github.com/egoist/mygo`（桌面壳/UI）、`github.com/MoeForge/moejs v0.1.0-alpha.11`、`github.com/evanw/esbuild v0.28.2` |
| 参考实现 | DeepSeek Harness 的 `@deepseek-ai/cordis@4.1.0`（Context/Fiber/Service/Effect/Event/Loader） |
| 现实基线 | ada_go v1（`internal/agent_core/plugins/platform/host/*`、`internal/common/pluginapi/*`） |
| 编写日期 | 2026-11-19 |

---

## 0. 结论摘要（TL;DR）

1. **内核是 Go 的，不是 JS 的。** 插件树、服务表、副作用栈、事件分发、配置校验全部在 Go 侧；JS 只是「插件的一种实现语言」，和 Go 插件走完全相同的 `Plugin` 接口、`Context`、`Effect` 语义。这样基座能力（文件、网络、进程、密钥、模型供应商）永远不被 JS 运行时绑架。**agent core 同样不是内核的一部分**：`agent`、`agent-loop`、`session`、`system-prompt`、`compaction` 都是 core 插件，由 preset 组合（§12.6）。
2. **Cordis 的核心不是 Proxy，是作用域。** `ctx.extend/isolate/intercept`、`fiber` 六态、`inject` 门控、`effect` 逆序回收、`epoch` 指纹重载、五种事件分发——这七件事在 Go 里都能 1:1 表达，只有「`ctx.x` 属性代理」换成显式 `ada.Service[T](ctx)`。方案给出完整 API 与语义表（§4–§8）。
3. **esbuild 只做一件事：TS → JS 降级。** 用 `api.Transform`（LoaderTS/TSX、Target ES2022、Format ESM、inline sourcemap），逐文件、内存态、纯 Go、无 Node；打包（Bundle）只作为「插件带 npm/CJS 依赖」的离线逃生通道，不进主链路（§10）。
4. **moejs 的边界必须写死。** 每个插件一个 Runtime（可选池化）、一个 goroutine、一个模块图；`*Module` 进程内编译一次跨运行时共享；**字节码不能落盘**（moejs 无序列化 API），所以冷启动靠「Artifact 缓存 + 懒激活」，不靠字节码文件（§11）。**已预验**：esbuild 降级 + `Link` 多模块、共享 Module 池化、ESM 循环依赖三条主线均通过（附录 D）。
5. **能力 ≠ 服务。** 能力（capability）是「能不能碰宿主资源」的**授权**；服务（service）是「插件之间怎么协作」的**接口**。v1 把能力退化成清单词汇（装了也不拦），v2 恢复为**双重门禁：服务可见性 + 授权调用点**（§13）。
6. **编排分三层，各有明确产出。** 装配编排产出「依赖图 + 事务报告」；交互编排产出「服务调用 + waterfall 中间件 + 可声明流水线」；调度编排产出「激活事件 + 懒加载 + 配额 + 闲置回收」（§12）。
7. **体积与性能有实测数。** 纯 Go 二进制：hello 1.85 MB；+moejs 12.32 MB；+esbuild 11.65 MB；两者同时 20 MB 量级（strip 后 +7.4 / +7.1 MB）。因此 **esbuild 默认不进发布产物**：编译发生在安装期/开发期（`ada` CLI 或可选 `adaesbuild` 构建标签），发布物只带 JS Artifact（§15）。
8. **迁移不是重写。** v1 的 `load.go`（虚拟模块表 + Link/Resolver）、`staging.go`（贡献事务）、`budget.go`/`breaker.go`（预算与熔断）、`cap_*.go`（十类沙箱）、`signature.go`（Ed25519 目录摘要）可直接搬；要重写的是 `runtime.go`/`context.go`/`app.go` 里的「一次性装配 + 冻结」模型（§附录 E）。

---

## 1. 背景与问题定义

### 1.1 ada_go v1 已经跑通了什么

v1 是一个成熟但**功能化**的插件宿主（`internal/agent_core/plugins/host`，约 60 个文件）：

| 已有能力 | 实现位置 | 对 v2 的意义 |
|---|---|---|
| 清单校验 + 信任分级 + Ed25519 验签 | `pluginapi/manifest.go`、`trust.go`、`signature.go` | 直接移植；签名覆盖「去 sig 清单 + 目录摘要」 |
| 每插件一个 moejs Runtime + 一条 owner goroutine | `platform/host/runtime.go` | 并发模型已验证：调用经 channel 串行，跨插件天然并行 |
| 虚拟模块表 + `Link`/`Resolver`（不读盘、不解析 node_modules） | `platform/host/load.go` | v2 编译层的模块装载可直接复用 |
| 宿主模块表（`node:fs/path/crypto/...` 自实现） | `platform/host/caps.go`、`platform/host/nodemods/*` | 纯计算部分可复用 |
| 十类能力桥（fs/exec/process/config/events/turn/dispatch/catalog/processes/ui） | `platform/host/cap_*.go` | 沙箱边界与限额是最大资产 |
| 预算分档 + 熔断半开探测 | `platform/host/budget.go`、`breaker.go` | 直接复用，v2 再叠「激活配额」 |
| 贡献装配事务（staging → commit → rollback） | `platform/host/staging.go` | v2 升级为 effect 撤销栈 |
| 钩子链（14 点位、5 档动作、超时忽略） | `plugins/hooks.go` | v2 泛化成事件总线的一种分发模式 |

### 1.2 v1 的结构性缺口

1. **没有依赖图**。装配顺序 = 产品声明顺序 + 第三方按 id 排序；没有 `inject`/`provide`，无法表达「A 必须在 B 之后，B 不在则 A 待机」。
2. **没有服务容器**。`host.Context` 是「每调用注入的回调结构体」，不是服务定位器：插件拿不到别的插件，只能通过三条约定的桥（动作绑定、工具发现、投递）间接协作。
3. **没有可撤销的贡献**。撤销粒度是「整插件」；运行期 `agent.register*` 只记 warning。
4. **能力位已退化为词汇**。2026-11-10 决策后「能力不决定装什么」，宿主对全部插件装同一套桥——安全边界只剩宿主内建实现，清单里的 `capabilities` 不再有强制力。
5. **钩子是空转资产**。14 个点位、动作分级、超时策略都实现了，但全仓 23 个插件零使用、零声明。
6. **官方插件其实没编译 TS**。`plugins/official/build.mjs` 用 Node ≥22.13 的 `module.stripTypeScriptTypes` 做类型擦除；第三方插件的 `.ts` 直接当 JS 读。文档里「esbuild 编译」与代码不符。
7. **重载是整装配双代重建**。改一个插件要重建整代装配、排空 30s，没有插件级热插拔。

### 1.3 Cordis 值得学的与不值得学的

| Cordis 机制 | 价值 | Go 可行性 |
|---|---|---|
| Context 派生树（extend/isolate/intercept） | 高：作用域是插件隔离的唯一正确抽象 | 高：值 + 父指针 + 只读覆盖表 |
| Fiber 六态 + epoch 指纹重载 | 高：依赖变化自动重载的判定核心 | 高：atomic 状态 + 指纹比较 |
| Service 注册表 + 单写者 + 依赖方先卸载 | 高：插件协作与安全回收的根基 | 高：RWMutex + dependents 计数 |
| Effect 托管副作用 | 极高：一切对外 API 都返回 disposer | 高：LIFO 栈 + 级联回收 |
| 五种事件分发（emit/parallel/serial/bail/waterfall） | 高：waterfall 是「策略拦截」的通用形态 | 高：中间件链 + `Next()` |
| Config Schema + 默认值 + simplify | 中高：配置是插件的公开契约 | 中：自研 JSON Schema 子集 |
| EntryTree / loader / patch 叠加 | 高：组合包与用户配置的分层 | 高：YAML/JSON 文档 + id 覆盖 |
| `ctx.x` Proxy 属性代理 | 低（DX 糖） | 不可行：改显式 `ada.Service[T](ctx)` |
| 原型链即作用域、Symbol 作隔离键 | 低（实现细节） | 换 `*label` 指针 |
| HMR 靠 Node loader | 低 | 不可行：改为「重编译 + 新 Runtime + 逆序 dispose」 |
| YAML `!!js` 任意表达式 | 低（安全债） | 改为沙箱化求值或受限模板 |

---

## 2. 设计目标、非目标与硬约束

### 2.1 目标（按优先级）

| 编号 | 目标 | 验收口径 |
|---|---|---|
| G1 | **统一插件接口**：Go 插件与 TS 插件同构 | 同一份 `kernel.Plugin`；**平台能力与 agent core 自己也以插件形式装载**（§12.6） |
| G2 | **依赖编排**：`inject`/`provide`/可选依赖/延迟激活/环检测 | 提供者缺失时插件停在 `pending` 而非失败；提供者上线自动激活 |
| G3 | **事务化生命周期**：effect 逆序、依赖方先卸载、失败回滚 | 任一步失败，装配回到上一代；无悬挂监听/定时器/句柄 |
| G4 | **纯 Go 工具链**：编译、执行、安装、校验都不需要 Node | CI 里 `node`/`npm` 不存在也能构建、安装、运行 |
| G5 | **能力=授权，服务=可见性**：双重门禁 | 未授权的 `fs.read` 在调用点被拒并审计；未 inject 的服务拿不到 |
| G6 | **可观测**：fiber 树、effect 树、服务表、装配报告、调用跨度 | `ada inspect` 能回答「谁提供了 X、谁依赖 X、X 为什么被重载」 |
| G7 | **宿主可移植**：桌面（mygo）、headless CLI、服务端同内核 | 内核包不 import `mygo`；mygo 只是 `platform/host/desktop` 适配器 |
| G8 | **编排可声明**：流水线、激活事件、组合 patch 都是数据不是代码 | 用 JSON/YAML 能表达一条跨插件流水线，无需改 Go |

### 2.2 非目标（v2 明确不做）

- **进程级隔离**：v2 沿用「同进程 + 每插件独立 realm」；子进程/沙箱进程作为 v2.1 的 `caps.process.isolated` 扩展点预留（v1 的 `process.supervise` 已给出雏形）。
- **直接 npm 安装**：插件包自带依赖，安装期离线打成一个 Artifact；不解析 `node_modules`、不支持 `.node` 原生扩展。
- **浏览器里跑插件内核**：`GOOS=js` 下内核编译通过但 JS 插件不可用（moejs 依赖自身调度）；Web 前端只消费贡献。
- **插件自绘 UI**：插件产数据，宿主 mygo 画界面（沿用 v1 的 Page/Section/Item 契约与插槽模型）。
- **兼容 Cordis 生态**：只移植语义，不追求 `@cordisjs/*` 包兼容。

### 2.3 硬约束（设计时不可违反）

| 约束 | 来源 | 设计含义 |
|---|---|---|
| C1 | moejs：一个 Runtime 一个全局环境、一次 `Load` 一个模块、单 goroutine 使用 | 热重载 = 新 Runtime；池化 = N 个 Runtime 共用同一 `*Module` |
| C2 | moejs：`*Module` 不可变、可跨运行时并发共享、**字节码不可序列化** | 编译缓存只能是进程内 `*Module`；跨进程缓存只能存 JS/Artifact |
| C3 | moejs：跨运行时不可传值；跨边界只能 Go 值/JSON；无 sourcemap | 服务跨语言一律 JSON；诊断靠 esbuild map 在宿主侧重映射 |
| C4 | moejs：裸 Runtime 无 `setTimeout`/`fetch`/`console`/`fs` | 一切环境能力由宿主显式安装，默认零权限 |
| C5 | esbuild：`Transform` 不做路径解析、不做类型检查、不做 CJS 运行时 | 模块解析是宿主的活；类型检查不承诺；CJS 走离线 Bundle |
| C6 | mygo：UI 在主线程，Go 回调可从任意 goroutine 但 UI 变更需在主线程执行 | 内核不直接碰 UI，只发贡献事件；宿主适配器用 `mygo.RunOnMain` / 类型化事件切回主线程 |
| C7 | 二进制体积敏感（桌面应用 7–30 MB 量级） | esbuild 不进默认发布物；moejs 必须进 |
| C8 | Windows/Linux/macOS 全平台、`CGO_ENABLED=0` 交叉编译 | 内核与依赖全部纯 Go |

---

## 3. 总体架构

### 3.1 分层

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ L6  应用/宿主     mygo 桌面窗口 · ada CLI(headless) · 服务端 HTTP             │
│                   platform/host/desktop   platform/host/cli   platform/host/server                        │
├──────────────────────────────────────────────────────────────────────────────┤
│ L5  组合层（数据）preset（agent 能力组合）· bundle · profile patch            │
│                   「装哪些插件」在这一层决定，不改代码                          │
├──────────────────────────────────────────────────────────────────────────────┤
│ L4  Agent Core（插件）agent · agent-loop · session · system-prompt ·          │
│                   context · compaction · approval · user-questions ·          │
│                   subagent · goal · plan · workflow                           │
├──────────────────────────────────────────────────────────────────────────────┤
│ L3  平台 seam（插件）llm · tools · prompts · fs · shell · sandbox · storage · │
│                   settings · secrets · clock · commands · jobs · telemetry · ui│
│                   每类 = seam（接口）+ provider（实现）+ policy（策略）三段     │
├──────────────────────────────────────────────────────────────────────────────┤
│ L2  装配与执行基础设施（不是插件）assemble · compile(esbuild) · jsrt(moejs) ·  │
│                   plugin(清单/安装/签名/Artifact) · caps(授权与沙箱)           │
├──────────────────────────────────────────────────────────────────────────────┤
│ L1  内核 kernel（不是插件、最小依赖）Context / Fiber / Service / Effect /      │
│                   Event / Config / Registry（贡献目录）/ Provider 接口         │
├──────────────────────────────────────────────────────────────────────────────┤
│ L0  基座         Go 标准库 + mygo（仅 host 适配器可见）                        │
└──────────────────────────────────────────────────────────────────────────────┘
```

**L1 内核与 L2 基础设施都不是插件**，其余全部是插件。内核只懂「作用域、服务、副作用、事件、配置、装配」，不懂「turn、消息、工具调用、模型」这些 agent 词汇——那些全在 L3/L4 的插件里（详见 §12.6）。这与 DSH 一致：Cordis 是框架，`dsh-agent`/`dsh-agent-loop`/`dsh-session`/`dsh-tools`/`dsh-llm` 全是可装载插件。

依赖方向：上层可 import 下层，反向只经接口。L1 不 import L2/L3/L4，也不 import mygo；L2 的 `jsrt` 实现 L1 的 `Provider` 接口上挂：

```go
// kernel 只认这个接口，不知道 moejs/esbuild 的存在
type Provider interface {
    // Open 打开插件实例；返回的 Plugin 由内核按 Cordis 语义装配
    Open(ctx context.Context, spec PluginSpec) (Plugin, error)
}
```

### 3.2 进程与线程模型

```
                     ┌──────────── app 进程 ─────────────┐
 mygo UI 主线程 ────▶ │ platform/host/desktop：RunOnMain / 类型化事件 │
                     │                                    │
                     │  kernel goroutine (#1)              │ ← 装配、服务表、事件分发、配置
                     │    ├─ plugin A loop goroutine       │ ← moejs Runtime A（单线程）
                     │    ├─ plugin B loop goroutine       │ ← moejs Runtime B
                     │    └─ Go 插件 goroutine 池         │ ← 原生插件直接跑
                     │                                    │
 调用方 goroutine ──▶ │  Call(plugin, method, JSON)        │ ← channel 串行 + ctx 取消
                     └────────────────────────────────────┘
```

规则：

- **内核单写者**：服务注册/注销、fiber 状态迁移、事件监听变更都在 kernel goroutine 上串行执行（或加锁的短临界区）。插件回调**永不**在 kernel goroutine 上跑，避免一个卡死插件冻结整个装配。
- **每插件一 loop**：moejs `Runtime` 只能单 goroutine 使用（C1）；`eventloop.Loop.Start()` 起一条专属 goroutine，所有 `Load/Call` 都 `RunOnLoop` 投进去。
- **调用方不阻塞 UI**：UI 线程用 `mygo.RunOnMain` 或类型化事件回主线程；对插件的调用走 `context.Context` + channel，超时/取消由预算层兜底。

### 3.3 一次插件调用的完整链路

```
JS 插件 handler
  └─ ctx.fs.read("/workspace/a.txt")        ← ada:fs 模块里的 NativeFunc
      └─ 宿主 cap: Broker.Authorize(plugin, "fs.read", path)
          ├─ 通过 → caps/fs 沙箱（os.Root）→ 结果
          └─ 拒绝 → *Exception{code:"EACCES"} + audit
  └─ ctx.get("model").complete(req)          ← 内核服务，跨插件
      └─ kernel.ServiceTable 解析 → provider 插件 JS handler
          └─ RunOnLoop(provider) → 结果 JSON
```

---

## 4. 核心抽象 A：Context / Scope / Fiber

### 4.1 设计原则

- **`Context` 是不可变派生值**，不是可变对象：`Extend/Isolate/Intercept` 返回新 `*Context`，父永不被改。这是「作用域可嵌套、可并发读」的前提。
- **`Fiber` 是插件实例的宿主**：一个 fiber 拥有一个 scope、一个 config 快照、一个状态机、一组依赖快照。
- **`ctx.Plugin()` 返回 fiber 句柄**（Cordis 的 fork）：`Ready()` 可等、`Dispose()` 可回收。子 fiber 的生命周期挂在父 fiber 的 scope 上——父卸载自动级联。

### 4.2 Go API

> M1 已冻结的是本节的一个子集,以 [kernel-api.md](kernel-api.md) 为准;`Once` / `Inject(names, fn)` / `Store` 等属于 M3+ 目标形态。

```go
package kernel

// Context 是插件看到的世界。派生操作返回新值，父链只读。
type Context struct {
    app    *App
    parent *Context
    fiber  *Fiber              // nil 表示「平台上下文」（根），如宿主自身
    vars   vars                // entry 元信息：id、来源、隔离标签、拦截配置
}

func (c *Context) App() *App
func (c *Context) Fiber() *Fiber          // 根上下文返回 nil

// 派生 -------------------------------------------------------------------
func (c *Context) Extend(v Vars) *Context              // 如 ctx.extend({ entry, source })
func (c *Context) Isolate(name string, label string) *Context
func (c *Context) Intercept(name string, cfg ServiceConfig) *Context

// 挂载 -------------------------------------------------------------------
func (c *Context) Plugin(p Plugin, cfg Config) (*Fiber, error)
func (c *Context) Dispose(ctx context.Context) error     // 回收本作用域

// 服务 -------------------------------------------------------------------
func (c *Context) Provide(name string, impl any, opts ...ProvideOption) (Dispose, error)
func (c *Context) Service(name string) (any, error)       // 未 inject 且缺失 → ErrServiceMissing
func (c *Context) TryService(name string) (any, bool)     // 绕过 inject 的探测
func (c *Context) Set(name string, value any) error       // 仅 provider 自身可写

// 副作用 -----------------------------------------------------------------
func (c *Context) Effect(label string, fn func() (Dispose, error)) (Dispose, error)

// 事件 -------------------------------------------------------------------
func (c *Context) On(ev string, h Handler, opts ...OnOption) Dispose
func (c *Context) Once(ev string, h Handler, opts ...OnOption) Dispose
func (c *Context) Emit(ev string, args ...any)
func (c *Context) Parallel(ctx context.Context, ev string, args ...any) error
func (c *Context) Serial(ctx context.Context, ev string, args ...any) error
func (c *Context) Bail(ctx context.Context, ev string, args ...any) (any, error)
func (c *Context) Waterfall(ctx context.Context, ev string, input any, args ...any) (any, error)
func (c *Context) Inject(names []string, fn func(*Context))  // 依赖到齐后条件挂载

// 杂项 -------------------------------------------------------------------
func (c *Context) Log() Logger
func (c *Context) Config() Config        // 只读快照
func (c *Context) Entry() EntryMeta
func (c *Context) Store() KV              // 插件私有持久 KV（宿主实现）
```

泛型取服务（让调用点有类型）：

```go
func Service[T any](c *Context, name string) (T, error) {
    v, err := c.Service(name)
    if err != nil { var zero T; return zero, err }
    t, ok := v.(T)
    if !ok { var zero T; return zero, fmt.Errorf("service %q is %T, not %T", name, v, zero) }
    return t, nil
}
```

### 4.3 插件接口（Go 插件与 TS 插件共用一个）

```go
type Plugin interface {
    Name() string
    Inject() Inject              // 必需依赖
    InjectOptional() []string    // 可选依赖（缺失也激活，提供后触发重载）
    Provide() []string           // 本插件要注册的服务名
    Config() *schema.Schema      // 可为 nil
    Apply(ctx *Context, cfg Config) (Dispose, error)
}

type Inject struct { Names []string; Config map[string]ServiceConfig }  // 对象形态 = 每服务拦截配置
```

三种写法都支持（对齐 Cordis）：

```go
// 1) 函数式子插件
func NewCron() kernel.Plugin { return kernel.Func("cron", applyCron) }

// 2) 结构体
type cronPlugin struct{ /* 依赖注入的字段不需要：全部从 ctx 取 */ }
func (cronPlugin) Name() string            { return "cron" }
func (cronPlugin) Inject() kernel.Inject   { return kernel.Inject{Names: []string{"clock"}} }
func (cronPlugin) Provide() []string       { return []string{"cron"} }
func (cronPlugin) Apply(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) { ... }

// 3) JS 插件：jsrt 产出一个实现同一接口的 adapter（§11）
```

### 4.4 Fiber 状态机

```
              ┌──────────┐  deps 未齐        ┌───────────┐
   Plugin() ─▶│  pending │◀─────────────────▶│  waiting  │
              └────┬─────┘  deps 到齐          └───────────┘
                   │ deps satisfied
                   ▼
              ┌──────────┐  apply 失败        ┌──────────┐
              │ loading  │───────────────────▶│  failed  │──── update(cfg) ──┐
              └────┬─────┘                    └──────────┘                    │
                   │ apply ok                                                  │
                   ▼                     epoch 变化 / 依赖变化                │
              ┌──────────┐ ───────────────────────────────────────────────────┘
              │  active  │◀─────────────── 重新 loading
              └────┬─────┘
                   │ Dispose / 父作用域回收 / 服务消失
                   ▼
              ┌──────────┐  disposers 全部完成
              │unloading │──────────────────────▶ disposed
              └──────────┘
```

| 状态 | 含义 | 可接收的调用 |
|---|---|---|
| `pending` | 已创建，依赖未满足 | 无 |
| `loading` | 正在 `Open` + `Apply` | 无（调用方等待 `Ready`） |
| `active` | 服务已注册、贡献已提交 | 全部 |
| `failed` | `Apply` 失败或运行期崩溃熔断 | 只读 `Ready()` 返回错误；`Update` 可复活 |
| `unloading` | 正在逆序执行 disposer | 拒绝新调用，进行中的调用被取消 |
| `disposed` | 已终结 | 全部返回 `ErrFiberDisposed` |

```go
type Fiber struct {
    ID      string        // "<entry-id>#<instance>"
    State   State         // 原子读
    Epoch   uint64        // 依赖指纹（服务实现 uid 序列的哈希）
    Revision uint64       // 配置/代码版本
}

func (f *Fiber) Ready(ctx context.Context) error      // 等 active，返回 failed 的错误
func (f *Fiber) Update(cfg Config) error              // 配置热更新（可能触发 restart）
func (f *Fiber) Reload(reason ReloadReason) error     // 代码重载（新 Runtime）
func (f *Fiber) Dispose(ctx context.Context) error
func (f *Fiber) Effects() EffectNode                  // 诊断：effect 标签树
func (f *Fiber) Deps() []DepInfo                     // 诊断：注入的服务与提供者
```

### 4.5 隔离与拦截（Cordis 三件套的 Go 等价）

| Cordis | Go | 语义 |
|---|---|---|
| `ctx.extend(meta)` | `ctx.Extend(Vars{...})` | 派生新作用域，不改父 |
| `ctx.isolate(name, label?)` | `ctx.Isolate(name, label)` | 让该名字在子作用域指向独立实现槽；同名 label 共享 |
| `ctx.intercept(name, cfg)` | `ctx.Intercept(name, cfg)` | 给下层使用的服务叠「该服务专属配置」，逐祖先合并 |

实现：

```go
type vars struct {
    isolate map[string]*label     // 名字 → 隔离标签（指针即身份）
    intercept map[string]ServiceConfig
    entry *EntryMeta
}

func (c *Context) Isolate(name, label string) *Context {
    l := c.app.labelFor(label)            // 同 label 复用同一 *label
    return c.derive(func(v *vars) { v.isolate[name] = l })
}
func (c *Context) Intercept(name string, cfg ServiceConfig) *Context {
    return c.derive(func(v *vars) { v.intercept[name] = merge(c.vars.intercept[name], cfg) })
}
```

**为什么不用属性代理**：Go 没有 Proxy。`ctx.Service("model")` 显式取值，把「未 inject 就不能取」变成编译期/运行期可见的错误，比隐式陷阱更利于大型工程（IDE 可跳转、可 mock、可 grep）。

---
## 5. 核心抽象 B：Service（服务表、注入、隔离、重载指纹）

### 5.1 数据结构

```go
type ServiceTable struct {
    mu    sync.RWMutex
    impls map[serviceKey]*Impl          // key = (name, isolateLabel)
    byName map[string][]*Impl           // 诊断用
}

type Impl struct {
    UID        uint64        // 单调递增，作为「实现版本」参与 epoch 指纹
    Name       string
    Label      *label        // nil = 全局默认槽
    Spec       ServiceSpec   // {Version int, Kind string}
    Value      any           // 服务实现（Go 值或跨语言 wrapper）
    Provider   *Fiber
    Dependents map[*Fiber]struct{}   // 谁 inject 了我
    release    chan struct{}         // 依赖方清零后关闭
}

type ServiceSpec struct {
    Version int      // 契约版本；消费者声明期望，不匹配 = 依赖未满足
    Kind    string   // "value" | "object" | "stream" | "router"
}
```

### 5.2 注册、读取、覆盖规则

| 操作 | 规则 | 错误 |
|---|---|---|
| `Provide(name, impl)` | 由插件在 `Apply` 中调用；同一 `(name,label)` 已被占用 → 失败 | `ErrServiceRegistered{owner}` |
| `Set(name, v)` | **只有 provider fiber 自己**能改；未 provide 先 set → 失败 | `ErrNotProvider` |
| `Service(name)` | 消费者必须已 `inject` 该名；否则即使存在也拒绝（可见性门禁） | `ErrServiceNotInjected` |
| `TryService(name)` | 绕过可见性探测（用于可选依赖） | 返回 `(nil,false)` |
| provider dispose | 注销 impl；`release` 等到 `Dependents` 清空才真正删除 | — |
| 重复 Provide 同一实例 | 幂等：同 provider 重复注册返回同一 disposer | — |

**单写者**的意义：服务值是接口契约的实现，任何人可改会造成「插件互相踩」。Cordis 的 `ctx.set` 限制在提供者 fiber，Go 版照搬。

### 5.3 注入解析与 epoch（依赖变化自动重载）

```go
// 每次 provide/dispose/update 后，内核重算所有 fiber 的 epoch
func (a *App) recompute() {
    for _, f := range a.fibers { f.refresh() }
}

func (f *Fiber) refresh() {
    epoch := uint64(0)
    for name, want := range f.expected {           // expected: 消费者声明的服务契约版本
        impl := f.ctx.resolveImpl(name)            // 按 isolate 线索找 impl
        if impl == nil || impl.Spec.Version < want {
            f.setEpoch(inactive)                   // 保持 pending
            return
        }
        epoch = mix(epoch, impl.UID)
        impl.Dependents[f] = struct{}{}
    }
    for _, name := range f.plugin.InjectOptional() {
        if impl := f.ctx.resolveImpl(name); impl != nil {
            epoch = mix(epoch, impl.UID)
            impl.Dependents[f] = struct{}{}
        }
    }
    if epoch != f.Epoch {
        f.Epoch = epoch
        f.scheduleReconcile()                      // active → unloading → loading → active
    } else if f.State == pending {
        f.scheduleLoad()
    }
}
```

语义要点：

1. **指纹 = 依赖实现 UID 序列的哈希**，任一提供者换实现（含热重载后 UID 变化）即触发消费者重载。这是 Cordis `epoch` 的等价物，避免「服务偷偷换了实例但消费者还握着旧引用」。
2. **未满足不失败**：`pending` + 等待；提供者上线后自动激活（v1 的「装配失败即跳过」在这里被修正）。
3. **可选依赖**：`InjectOptional` 缺失也激活；提供后触发一次重载（因为 epoch 变了）。需要「不重载只要通知」的场景用 `ctx.TryService` + 事件。
4. **版本契约**：`ServiceSpec.Version` 让 `provider v2` 与 `consumer expecting v1` 不匹配时视为未满足，而不是运行期 panic。
5. **循环依赖**：A inject B 且 B inject A，两者都停在 `pending`；装配报告点名环（§12.1），可用 `injectOptional` 打破。
6. **重载级联**：消费者重载会注销其贡献并重跑 `Apply`；如果消费者自己也是 provider，它的 UID 变，继续级联。内核按拓扑序串行处理，保证不出现「半新半旧」的依赖图。

### 5.4 隔离（同一服务名的多实例）

```go
// 插件 A 与 B 各自要一个独立的 shell 会话
ctxA := root.Isolate("shell", "team-a")
ctxB := root.Isolate("shell", "team-b")
```

- 隔离只影响**该名字解析到哪个槽**，不影响其它服务。
- `Isolate` 可链式：`ctx.Isolate("shell","x").Isolate("db","x")`。
- 组合包（profile）可用 `isolate: { shell: true }` 让每个 entry 独立（loader 展开成 `"entry:<id>"` 标签）。
- 诊断：`ada inspect --services` 显示「名字 → 槽标签 → 提供者 → 依赖者」。

### 5.5 跨语言服务

JS 插件可以 `provide` 一个服务，Go 插件/宿主可以消费；反向亦然。规则：

```go
// JS 侧
ctx.provide('summarizer', {
  version: 1,
  async summarize(text) { return { summary: text.slice(0, 20) } }
})
```

```go
// 内核里注册的是 wrapper，不是 JS 值
type JSObjectService struct {
    fiber *Fiber
    name  string
}

func (s *JSObjectService) Call(ctx context.Context, method string, args json.RawMessage) (json.RawMessage, error) {
    return s.fiber.invoke(ctx, "service.call", map[string]any{"name": s.name, "method": method}, args)
}
```

- **同 runtime 内**（JS 消费者调自己插件的服务）：仍走内核 wrapper，但内核检测到「消费者与提供者同 fiber」时直接 `RunOnLoop` 调用，省一次 JSON 编解码。
- **跨 runtime**：一律 JSON + `context.Context`，受同一预算/熔断管辖。
- **Go 侧消费**：`js, err := kernel.Service[*jsrt.JSObjectService](ctx, "summarizer")`，然后 `js.Call(...)`。
- **禁止**跨边界传函数值、Promise、生成器（moejs 会返回 `ErrForeign`）；回调注册用事件（§7），不用服务方法参数。

### 5.6 内置服务（基座自身也是插件）

内核**只保留**极少数无领域的服务；其它一切（含 agent core）都是插件提供的服务：

| 服务名 | 谁提供 | 说明 |
|---|---|---|
| `log` | 内核（L1） | 命名空间日志，fiber 路径自动推导 |
| `bus` | 内核（L1） | 进程内事件（大多数场景直接用 `ctx.Emit/On`） |
| `registry` | 内核（L1） | 贡献目录（只读视图 + 注册 API） |
| `clock` | `plugins/seam/clock`（Go 插件） | 可注入时钟，测试可替换 |
| `config` | `plugins/seam/config`（Go 插件） | 全局配置读写，独立于插件私有 config |
| `store` | `plugins/provider/store-local`（Go 插件） | 插件私有 KV + Secret（宿主实现文件/SQLite） |
| `llm` | `plugins/seam/llm`（Go 插件） | 模型调用 **seam**：只定义接口与契约版本，实现由 provider 插件注册 |
| `tools` | `plugins/seam/tools`（Go 插件） | 工具目录与执行 **seam**；执行器由 `tool-executor-*` 插件提供 |
| `prompts` | `plugins/seam/prompts`（Go 插件） | 提示词分段注册表 |
| `fs` / `shell` / `workspace` / `sandbox` / `storage` / `settings` / `secrets` / `jobs` / `telemetry` / `ui` | `plugins/seam/*` + `provider/*` + `policy/*` | 见 §12.6 的三段式；workspace 见 [workspace-plugin.md](workspace-plugin.md) |

> 这正是「一切以插件为核心」的落地方式：**基座不为某个功能开后门**。模型调用、工具执行、审批、会话存储都是插件，可被替换、可被隔离、可被禁用；不同产品只是**不同的 preset**。
> **agent core 本身也是插件组合**（`agent`、`agent-loop`、`session`、`system-prompt`、`compaction`…），见 §12.6。

---

## 6. 核心抽象 C：Effect（托管副作用与回收顺序）

### 6.1 为什么必须有

插件的危险不在于「算错」，而在于「留下东西」：事件监听、定时器、文件句柄、服务实现、UI 订阅、goroutine。Cordis 的解法是 **一切注册都返回 disposer，disposer 挂在 fiber 的 effect 栈上，卸载时逆序回收**。Go 版把它变成硬性约定：**内核暴露的所有注册 API 都返回 `Dispose`**。

```go
type Dispose func(context.Context) error

func (c *Context) Effect(label string, fn func() (Dispose, error)) (Dispose, error)
```

- `fn` 立即执行，返回的 disposer 入栈。
- 调用返回的 disposer，或 fiber 卸载，**先发生者为准**，幂等。
- 一个 effect 内可嵌套 effect（`fn` 里再调 `ctx.Effect`），形成树；父 effect 的 disposer 执行时先回收子 effect。
- fiber 已 `disposed` 再注册 effect → `ErrEffectOnDisposed`（防止插件在卸载后偷偷复活）。

### 6.2 回收顺序（精确规则）

```
Fiber.Dispose(ctx)：
  1. 状态 → unloading（拒绝新调用；进行中的调用：先等，超过 GracePeriod 后 Cancel）
  2. 等待「依赖我的 fiber」全部释放（Dependents 清空或超时强制）
  3. 逆序回收子 fiber（它们是本 fiber 的 effect，创建顺序的逆序）
  4. 注销本 fiber 提供的服务（此时已无依赖者）
  5. 逆序执行本 fiber 的 effect disposer（串行、每个有超时）
  6. 释放运行时（JS：关 loop、关 Runtime；Go：关 goroutine 池）
  7. 状态 → disposed，发 internal/fiber/disposed
```

对照 Cordis：Cordis 在 fiber 级 `Promise.all` 并发回收多个 effect，但单个 effect 内逆序串行；Go 版**在 fiber 内串行、跨 fiber 并行**，用 `errgroup` 控制并发上限（默认 4），避免同时关 100 个运行时造成抖动。

```go
func (f *Fiber) runDisposers(ctx context.Context) error {
    var errs []error
    for i := len(f.effects) - 1; i >= 0; i-- {          // 逆序
        e := f.effects[i]
        c, cancel := context.WithTimeout(ctx, e.Timeout) // 默认 5s
        if err := safeDispose(c, e); err != nil { errs = append(errs, fmt.Errorf("%s: %w", e.Label, err)) }
        cancel()
    }
    return errors.Join(errs...)
}
```

### 6.3 内置为 effect 的 API

| API | disposer 做什么 |
|---|---|
| `ctx.On/Once` | 摘除监听器 |
| `ctx.Provide` | 注销服务（等依赖者清零） |
| `ctx.Plugin` | 回收子 fiber（级联） |
| `sched.Every(interval)` / `sched.After(d)` | 停调度、取消 timer |
| `caps.Watch(path)` | 关 watcher |
| `ui.contribute(page/slot)` | 撤贡献，通知宿主重绘 |
| `route.Handle(method, path)` | 摘路由 |
| `tool.Register(def)` | 摘工具 |
| `ctx.Store().Open()` | 关 KV 句柄 |

### 6.4 失败与超时

- disposer **panic**：`recover` + 记 `internal/effect.panic` 事件 + 继续回收其余。
- disposer **超时**：记 warning，强制继续；连续超时的插件进入 `degraded`，下次装配报告里标注。
- 回收错误**聚合**返回（`errors.Join`），不阻断后续步骤；`Fiber.Dispose` 只在「运行时无法关闭」时返回致命错误。
- **强制回收**：`App.Dispose` 有总预算（默认 10s），超时后 `runtime.Kill()`（moejs `Interrupt` + 关闭 loop），并记录泄露的 effect 标签供事后排查。

---

## 7. 核心抽象 D：Events（五种分发与中间件）

### 7.1 事件定义

Go 的事件是**有类型的**：名字 + 载荷类型，注册到一张表，避免 `map[string]any` 满天飞。

```go
package events

type Before struct {           // turn/before
    ThreadID string
    Messages []Message
    Model    string
}
var BeforeTurn = kernel.Event[Before]("turn/before", kernel.Events.Emit)

// 使用
kernel.On(ctx, events.BeforeTurn, func(c *kernel.Context, ev *events.Before) error {
    ev.Messages = append(ev.Messages, systemPrompt)
    return nil
})
```

内部事件（内核保留命名空间 `internal/*`，插件只读；`internal/plugin` 等少量可监听）：

| 事件 | 时机 | 载荷 |
|---|---|---|
| `internal/fiber/*`（loading / active / failed / disposed） | 状态迁移 | fiber ID、原因 |
| `internal/service/provide` / `internal/service/dispose` | 服务注册/注销 | name、impl uid |
| `internal/config/commit` | 配置写回前 | entry、config |
| `internal/config/update` | 配置热更新（waterfall，可否决重启） | new cfg |
| `internal/assembly/phase` | 装配阶段 | phase、报告 |
| `internal/call/slow` / `internal/call/failed` | 调用诊断 | 插件、方法、耗时 |

### 7.2 五种分发

| 模式 | 返回值 | 顺序 | 错误策略 | 用途 |
|---|---|---|---|---|
| `Emit` | 无 | 全部，按优先级 | 记录并继续（fire-and-forget） | 通知、埋点 |
| `Parallel` | 无 | 并发全部 | 收集全部错误（`errors.Join`） | 独立观察者 |
| `Serial` | 无 | 串行 | 首个错误即停 | 有序副作用 |
| `Bail` | `(any, error)` | 串行 | 首个非 nil 结果即停 | 「谁先处理谁负责」 |
| `Waterfall` | `(any, error)` | 串行，`Next()` 传递 | 不调 `Next` 即短路 | **策略/拦截/改写** |

`Waterfall` 是编排的核心原语：

```go
type Next func(input any) (any, error)

kernel.WaterfallOn(ctx, events.ModelRequest, func(hctx context.Context, c *kernel.Context, req *ModelRequest, next Next) (*ModelRequest, error) {
    if req.Tokens > c.Config().Int("maxTokens") {
        req.Tokens = c.Config().Int("maxTokens")
    }
    return next(req)          // 不调 next 就是否决
})
```

### 7.3 顺序、优先级与超时

- 监听器默认按「注册顺序」；`OnOption{Priority: p}` 可调（越大越先，同级按 plugin id 稳定排序——沿用 v1 `HookChain` 的做法）。
- 每个监听器有超时(默认 200ms,可用 `OnOption{Timeout: ...}` 放宽,上限 2s,超出被截断);超时按该模式的错误策略处理。
- 监听器签名带 `context.Context`([ADR 0003](adr/0003-handler-context-and-timeout-gate.md)):它是「父上下文 + 本监听器超时」派生的子上下文,超时即取消,监听器应据此收尾。内核无法杀死 goroutine,只能停止等待。
- **waterfall 超时闸门**:本次派发超时后 `Next` 一律返回 `ETIMEOUT`,下游绝不执行——即使监听器忽略取消信号。waterfall 是策略/拦截的唯一原语,超时后放行下游等于 fail-open(AGENTS P7)。
- `Once` 只触发一次；`global` 监听器（`OnOption{Global: true}`）穿透 `isolate`，用于「全局面板/审计」。

### 7.4 JS 监听器

```ts
ctx.on('turn/before', async (ev) => { ev.messages.push({ role: 'system', content: rules }) })
ctx.waterfall('llm/request', async (req, next) => next({ ...req, temperature: 0.2 }))
```

桥接规则：

- JS 监听器被 Go 侧调用时，走 provider fiber 的 loop（`RunOnLoop`），返回 Promise 时用 `adopt`（v1 `execute.go` 已有的 thenable 处理）等结算。
- `emit/parallel` 对 JS 监听器**异步**（Go 不等待，除非调用方显式 `Wait()`）；`serial/bail/waterfall` 必须等结算，超时按 §7.3。
- JS 监听器抛错 = Go 侧 `*moejs.Exception` → 按模式策略处理，并附插件 ID / 事件名 / 栈。

### 7.5 与 v1 钩子链的关系

v1 的 14 个固定点位、5 档动作（observe/note/replace/abort）在 v2 里退化成 **事件 + 分发模式**：

| v1 点位 | v2 事件 | 模式 |
|---|---|---|
| `user.message` | `message/before` | Waterfall（可改写） |
| `turn.before` | `turn/before` | Waterfall + `Abort(err)` |
| `model.beforeRequest` | `llm/request` | Waterfall |
| `model.afterResponse` | `llm/response` | Emit |
| `tool.afterCall` | `tools/result` | Emit |
| `session.start/end` | `session/start` · `session/end` | Emit |
| `plugin.loaded` | `internal/fiber/active` | Emit |

好处：不再有「点位必须清单声明」的耦合，事件名就是字符串契约；是否 `replace`/`abort` 由分发模式决定，不再需要「动作等级 + 信任等级」矩阵。

---

## 8. 核心抽象 E：Config（Schema、分层、热更新）

### 8.1 Schema：JSON Schema 子集

选 JSON Schema 而不是 Cordis 的 Schemastery，理由：**TS 侧与 Go 侧共用一份**，还能直接给 IDE 补全。

支持子集（自研校验器，不引入重依赖）：

```
type / properties / required / default / enum / const / items / minItems / maxItems
minimum / maximum / minLength / maxLength / pattern / format(date-time, uri)
additionalProperties / oneOf / anyOf / $ref（仅限 #/$defs/...）
```

```jsonc
// plugin.json 内联，或 config.schema.json 引用
"config": {
  "$schema": "https://ada.dev/schema/plugin-config-1.json",
  "type": "object",
  "properties": {
    "maxTokens": { "type": "integer", "default": 4096, "minimum": 128, "maximum": 65536 },
    "endpoint":  { "type": "string", "format": "uri", "default": "https://api.example.com" },
    "features":  { "type": "object", "additionalProperties": { "type": "boolean" }, "default": {} }
  },
  "additionalProperties": false
}
```

校验器行为：

- **默认值补齐**：`apply` 永远拿到完整配置（Cordis 同类语义）。
- **未知字段**：`apiVersion` 主版本一致时忽略并记 warning（v1 已有此策略）；主版本不一致直接拒绝装载。
- **错误聚合**：一次返回全部 issue 与 JSON Pointer（`$.maxTokens: expected integer, got string`）。
- **不校验敏感值**：secret 不进 config，避免明文落盘。
- **取值方式**：`Config` 是只读快照，提供 `Int/Int64/Float/String/Bool/Duration/Strings` 便捷读取，以及 `Decode(&struct)`（先校验再反序列化）。Go 插件用结构体，JS 插件直接拿普通对象。

### 8.2 分层来源（组合 patch）

优先级从低到高：

```
schema default
  ← bundle 默认（插件自带 plugin.default.json，或 bundle 包提供的 patch）
  ← profile patch（组合包 cordis.patch.yml 等价物：ada.patch.yml）
  ← 用户 entry config（用户配置文件里该插件条目的 config）
  ← 环境变量（ADA_PLUGIN_<ID>_<KEY>=...，仅标量，显式白名单前缀）
  ← 运行期 API（ctx.Config().SetVolatile，不落盘）
```

patch 语义（对齐 DSH）：

```yaml
# ada.patch.yml
entries:
  - id: model-router
    plugin: builtin/model-router
    config: { default: "openai" }
  - id: fs-tools
    plugin: official/tools-fs
    disabled: false
  - insert_after: model-router
    id: telemetry
    plugin: '!optional'          # '!' 前缀 = 可选，缺失不阻断装配
    config: { endpoint: "https://otel.local" }
```

- `id` 是增量更新的键；改一个字段只热更新该 entry，不重建整代。
- 后写入的同 id 覆盖（同一行「最后一个写入者胜」）。
- `insert` / `insert_before` / `insert_after` 表达顺序意图，但**最终顺序由依赖决定**（§12.1）。
- `disabled` 支持受限表达式（§12.4），**不执行任意 JS**。

### 8.3 热更新

```
fiber.Update(newCfg)：
  1. resolve（默认值 + 各层合并）
  2. validate（失败则整体拒绝，旧配置继续生效）
  3. internal/config/commit（宿主持久化回调）
  4. internal/config/update（Waterfall：监听器可否决「重启」）
  5. 若配置里「volatile 字段」变了但非 volatile 没变 → 原地更新 ctx.Config() + 发 config.volatile
     否则 → fiber.Reload(config)（卸载重挂，服务 UID 变化，依赖者级联刷新）
```

规则：

- **重启是默认**，因为插件往往把配置固化进闭包/连接；只有显式标 `volatile: true` 的字段享受「原地更新」。
- 配置更新失败不能破坏运行中的实例：校验先于提交，提交失败回滚到旧值。
- `ctx.Config()` 返回只读快照（`Config` 是值类型，内部用 `atomic.Pointer`）。
- 每次 update 递增 `Revision`，装配报告与日志都带它，便于归因。

---
## 9. 插件形态与清单 v2

### 9.1 三种插件，一个接口

| 形态 | 说明 | 谁写 | 运行方式 |
|---|---|---|---|
| `go` | 基座内置插件（tools、model-router、approval） | 内核团队 | 进程内 Go 函数，零桥接开销 |
| `ts` | 第三方/官方 TS 插件 | 插件作者 | esbuild → moejs Runtime |
| `bundle` | 组合包：一组 entry + patch | 发行方 | 只是「装配文档 + 依赖声明」，不产生运行时 |

三者都实现 `kernel.Plugin`：

```go
// Go 插件
var FSTools = kernel.Func("tools-fs", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
    reg, err := kernel.Service[*tools.Registry](ctx, "tools")
    if err != nil { return nil, err }
    d1, err := reg.Register(tools.Def{Name: "read_file", ...})
    if err != nil { return nil, err }
    return kernel.Join(d1), nil
}).WithInject("tools").WithProvide("tools-fs")

// TS 插件：jsrt 在装配时把它包装成同样的 kernel.Plugin
```

### 9.2 `plugin.json`（v2 schema）

```jsonc
{
  "id": "tools-fs",                       // ^[a-z][a-z0-9-]{1,63}$，必须等于目录名
  "name": "文件工具",
  "version": "1.2.0",
  "apiVersion": 2,                         // 内核 API 主版本，不匹配拒绝装载
  "kind": "ts",                            // ts | go | bundle
  "description": "读写工作区文件",
  "trust": "official",                     // builtin | official | verified | third-party

  "entry": "src/index.ts",                 // 插件目录内相对路径；打包产物见 artifact
  "inject": ["tools", "clock"],            // 必需服务（未满足 → pending）
  "injectOptional": ["approval"],          // 可选服务（缺失也激活）
  "provide": [                             // 提供的服务与契约版本
    { "name": "fs", "version": 1, "kind": "object" }
  ],

  "capabilities": [                        // 授权，不是服务可见性
    "fs.read",
    "fs.write.workspace",
    "secrets.TOKEN"
  ],

  "config": "./config.schema.json",         // 也可内联：{ "type": "object", ... }

  "contributes": {
    "tools": [
      { "name": "read_file", "summary": "读取文件",
        "input": "./schemas/read_file.json",
        "readOnly": true, "timeoutMs": 30000 }
    ],
    "pages":  [ { "slug": "fs", "title": "文件", "icon": "folder", "render": "host" } ],
    "slots":  [ { "slot": "settings.section", "component": "FsSettings", "order": 20 } ],
    "routes": [ { "method": "GET", "path": "/fs/tree" } ],
    "commands":[ { "id": "fs.refresh", "title": "刷新文件树" } ],
    "events": [ { "on": "turn/before", "mode": "waterfall", "handler": "beforeTurn" } ]
  },

  "runtime": {
    "pool": 1,                             // 1 = 单 Runtime；>1 仅无状态插件
    "memoryLimit": "64MiB",
    "initTimeoutMs": 5000,
    "callTimeoutMs": 30000,
    "idleTTL": "10m"                       // 闲置回收（调度编排，§12.3）
  },

  "activation": ["onStartup", "onCommand:fs.refresh", "onService:fs"],

  "engines": { "ada": ">=2.0.0 <3.0.0" },

  "artifact": {
    "entry": "dist/plugin.artifact.json",
    "digest": "sha256:…",
    "signature": "ed25519:…"
  }
}
```

字段规则（相对 v1 的变化）：

| 字段 | 变化 | 理由 |
|---|---|---|
| `backend.entry` → `entry` | 拍平 | 前后端不再分两段（前端贡献由宿主渲染） |
| 新增 `inject/injectOptional/provide` | **核心新增** | 依赖编排与延迟激活 |
| `capabilities` | 语义从「词汇」恢复为「授权」 | §13 双重门禁 |
| 新增 `contributes.events` | 声明式事件挂载 | 让贡献可静态审计，不必跑代码 |
| 新增 `runtime` | 池化/预算/闲置 | 调度编排 |
| 新增 `activation` | 懒加载 | 调度编排 |
| 新增 `engines.ada` | 版本兼容 | 取代 v1 的 `minAppVersion` 语义 |
| 删除 `frontend.slots/pages/...` | 统一进 `contributes` | 单一贡献目录 |

校验时机：**install 时**做「形状 + schema + 编译 + 链接 + 签名」五连检（不执行代码，v1 `ValidateScript` 的加强版）；**装配时**做「依赖 + 能力 + 契约版本」检查；**运行期**只在贡献注册处做形状断言。

### 9.3 TS 插件模块形态

对齐 Cordis，三种写法都接受：

```ts
// A. 具名导出（推荐）
export const name = 'tools-fs'
export const inject = ['tools', 'clock']
export const provide = [{ name: 'fs', version: 1 }]
export const Config = { /* JSON Schema，可为空 */ }

export function apply(ctx: Context, config: Config) {
  const tool = ctx.effect('register tool', () =>
    ctx.tools.register({ name: 'read_file', async run(args) { /* … */ } })
  )
  return () => { /* 可选：额外清理 */ }
}

// B. 对象
export default {
  name: 'x',
  inject: ['clock'],
  apply(ctx, config) { … },
}

// C. 类（类似 Cordis Service）
export default class FsService extends ctx.Service('fs', 1) {
  constructor(ctx) { super(ctx); this.#root = … }
  read(path) { … }
}
```

约束（编译期强制，ESLint 规则 + 编译期静态扫描）：

1. **不得使用 CJS**：`require`、`module.exports`、`import = require`、`export =` → 报错并提示「打包成 ESM 或使用离线 Bundle 模式」。
2. **不得导入 `.node` 原生扩展**。
3. **不得 `eval`/`new Function`**（运行时 `DisableDynamicCode` 兜底）。
4. **裸包名**只允许 `ada:*` 宿主模块；其它裸名必须在 Artifact 里内联（离线 Bundle）。
5. **`apply` 必须可重入**：同一进程可能被调用多次（配置变更/热重载），不得依赖模块级单例状态；需要状态用 `ctx.store`。

### 9.4 贡献目录（contributes）

所有贡献都是**可撤销注册**（effect），且全部进一张 `contrib.Registry`，宿主/编排层只读目录。

| Kind | 关键字段 | 消费方 |
|---|---|---|
| `tools` | name、summary、input/output schema、readOnly、timeoutMs、scope | `tools` 服务、模型工具调用 |
| `providers` | id、models[]、endpoint、capabilities | `model` 服务 |
| `prompts` | name、template、variables | `prompts` 服务 |
| `actions` | id、title、input schema、confirm | UI 动作、跨插件调用 |
| `routes` | method、path、sse? | 宿主 HTTP 适配器 |
| `pages` | slug、title、icon、render=host\|custom | mygo 窗口 / 前端路由 |
| `slots` | slot 名（稳定枚举）、component、order | 前端插槽 |
| `commands` | id、title、keybinding? | 命令面板 |
| `events` | on、mode、handler 导出名 | 事件总线静态挂载 |
| `services` | name、version、kind | 服务表（也可运行期 `ctx.provide`） |
| `jobs` | id、schedule(cron)、handler | 调度编排 |
| `settings` | key、type、default、label | 设置页（宿主渲染） |

规则：

- **静态贡献**（清单里写的）在装配时预注册，失败即回滚，便于「不跑代码就能列出所有工具」。
- **动态贡献**（`apply` 里 `ctx.tools.register(...)`）在运行期注册，同样返回 disposer。
- 两者冲突（同名工具）→ 后者失败并报「已由插件 X 注册」，不静默覆盖。

---

## 10. 编译层：esbuild 只做「TS → JS 降级」

### 10.1 责任边界（必须写死，否则会误用）

| 职责 | 归属 | 说明 |
|---|---|---|
| TS 类型擦除、语法降级、装饰器展开、enum/namespace 降级、TSX 转换 | **esbuild `Transform`** | 纯 Go，无 Node，微秒–毫秒级 |
| 类型检查 | **不做**（可选外部 `tsc`） | esbuild 从不报类型错误；文档明确「编译通过 ≠ 类型正确」 |
| 模块解析 / 路径别名 / node_modules | **内核模块表**（不复用 esbuild 的 resolver） | `Transform` 不做路径解析（实测 `tsconfig.paths` 无效） |
| 打包（多文件合并 / CJS / npm 依赖） | **离线 Bundle 逃生通道** | 仅当插件带 npm/CJS 依赖时，安装期用 `api.Build` 打一次 |
| 字节码 / 缓存 | **Artifact**（JS + map 落盘） | moejs 不能序列化字节码（C2），只能缓存 JS |

**esbuild 在这个方案里的第一价值不是「快」，而是「补 moejs 的 TS 短板」**：moejs `CompileTS` 只做类型擦除，遇到有运行时语义的 TS 直接 `SyntaxError`（`enum`、带值 `namespace`、构造器参数属性、`import x = require`、`export =`、`accessor`、装饰器、TSX）。esbuild 把这一整类语法**降级成 ES2022 的普通 JS**，moejs 只吃 JS。

### 10.2 编译管线

```
plugin.json
  │
  ├─(1) 收集源文件：entry 所在目录递归 *.ts/*.tsx/*.mts/*.js/*.mjs/*.json
  │      上限：512 文件 / 单文件 1 MiB / 总计 8 MiB（超限拒绝，v1 install 限额同源）
  │
  ├─(2) 逐文件 Transform（并发，`GOMAXPROCS` 个 worker）
  │      Loader: TS | TSX | JS | JSON（按扩展名）
  │      Format: ESM   Target: ES2022   Platform: Neutral
  │      Sourcemap: inline   Sourcefile: plugin://<id>/<rel>
  │      TsconfigRaw: { experimentalDecorators: true, useDefineForClassFields: true }
  │      Define: { __ADA__: "true", "process.env.NODE_ENV": "\"production\"" }
  │
  ├─(3) 静态检查（不执行代码）
  │      · 产物里出现 `require(` / `module.exports` / `__require` → 拒绝（CJS）
  │      · 源码/产物出现 `.node` → 拒绝
  │      · 裸 specifier 不在 ada:* 表且不在本包文件表 → 链接期报错（点名 npm）
  │      · `eval` / `new Function` → warning（运行期由 DisableDynamicCode 兜底）
  │      · 超过 512 KiB 的模块 → warning（拆包）
  │
  ├─(4) 组装模块表：virtual name → {js, map, hash}
  │      entry 名固定为 plugin://<id>/<entry 相对路径>（保留 .ts 后缀，
  │      这样错误/diagnostic 直接指向源文件名）
  │
  ├─(5) 写 Artifact（dist/plugin.artifact.json）+ 计算 digest（可选签名）
  │
  └─(6) 缓存：$ADA_HOME/cache/ada_jsrt/compile/<key>.artifact.json
         key = sha256(全部源文件内容 + esbuild 版本 + 编译选项 + hostApi + entry)
```

### 10.3 Transform 参数（Go）

```go
func transformOne(virtualName, src string, loader api.Loader) (js, sm []byte, err error) {
    res := api.Transform(src, api.TransformOptions{
        Loader:      loader,
        Format:      api.FormatESModule,
        Target:      api.ES2022,
        Platform:    api.PlatformNeutral,
        Sourcemap:   api.SourceMapInline,
        Sourcefile:  virtualName,
        TsconfigRaw: `{"compilerOptions":{"experimentalDecorators":true,"useDefineForClassFields":true}}`,
        Define:      map[string]string{"__ADA__": "true", "process.env.NODE_ENV": `"production"`},
        LegalComments: api.LegalCommentsNone,
        MinifyWhitespace: false, // dev；Artifact 发布时可置 true
        KeepNames:   true,       // 保留函数名，栈与调试友好
        LogLevel:    api.LogLevelSilent,
    })
    if len(res.Errors) > 0 { return nil, nil, esbuildError(res.Errors) }
    return res.Code, res.Map, nil
}
```

为什么是 **ES2022** 而不是 ESNext/ES5：

- moejs 覆盖现代 ES（ES modules、class、async/await、Proxy、BigInt、TLA），`TODO.md` 明确未实现的只有少数提案（装饰器、`using`、import attributes、Iterator helpers、Temporal 等）。ESNext 也行，但 **ES2022 能让 esbuild 替我们处理几类「引擎差异」**（如 class static blocks、`#private` 的某些形态），代价只是极少 helper。
- 不选 ES5：helper（`__spreadValues`、`__async`…）膨胀 3–5 倍，且降级后的代码对栈/调试不友好。

### 10.4 模块表与解析算法

```go
// 解析顺序（对齐 v1 load.go，但作用在编译产物上）
func (m *ModuleTable) Resolve(referrer moejs.Referrer, spec string) (*moejs.Module, error) {
    switch {
    case spec == "ada:api":       return m.emptyTypes(), nil          // 类型占位：export {}
    case strings.HasPrefix(spec, "ada:"):  return m.hostModule(spec)  // 宿主模块
    case isRelative(spec):        return m.fromBundle(referrer, spec) // plugin://<id>/…
    case m.vendor[spec] != nil:   return m.vendor[spec], nil          // 离线 Bundle 内联
    default:                      return nil, npmSpecifierError(spec) // 点名「请构建期内联」
    }
}
```

- **虚拟名是唯一身份**：`plugin://<id>/src/util.ts`。宿主绝对路径永不出现在 JS 里（`import.meta.url/dirname` 也填虚拟路径，v1 `meta()` 已验证）。
- **扩展名补全**：`.js/.mjs/.ts/.tsx/.jsx/.json` 按顺序试；`.json` 被编译成 `export default …`（绕开 moejs 不支持 JSON modules）。
- **目录导入**（`./dir` → `./dir/index.ts`）支持；`../` 越出插件根 → 拒绝（防目录穿越）。
- **循环依赖**交给 moejs `Link`（ESM 语义）。**已预验**（2026-11-19）：双向 `import` + 函数声明提升，`Link` 与调用均正常；仍保留 TDZ 时序的回归用例。
- **动态 `import()`**：走 `moejs.Importer`，规格化规则同上；Artifact 会记录「可能的动态目标」（静态扫 `import("…")` 字面量），非字面量目标运行期拒绝并给明确错误。

### 10.5 Artifact 格式

```jsonc
// dist/plugin.artifact.json
{
  "artifactVersion": 1,
  "pluginId": "tools-fs",
  "apiVersion": 2,
  "entry": "plugin://tools-fs/src/index.ts",
  "builtBy": "ada/0.1.0 esbuild/0.28.2 go1.27.1",
  "files": [
    {
      "name": "plugin://tools-fs/src/index.ts",
      "loader": "ts",
      "js": "…",              // 编译产物（可直接 Compile）
      "map": "…",             // inline sourcemap 的 map 部分（或独立字段）
      "hash": "sha256:…",     // 源文件内容哈希
      "bytes": 4096
    }
  ],
  "vendor": { "lodash": "plugin://tools-fs/vendor/lodash.js" },
  "digest": "sha256:…",       // 所有 files.hash 的有序摘要，签名对象
  "signature": "ed25519:…"
}
```

- `dist/plugin.artifact.json` 是**发布物**；`src/` 可随包（便于审计），但运行不读源。
- 运行时只读 Artifact：`moejs.Compile(name, file.js)` → `Link`。**默认不在加载时调 esbuild**，因此发布物不需要 esbuild（C7）。
- 三种编译发生点：

| 场景 | 谁编译 | esbuild 在进程内？ |
|---|---|---|
| `ada pack` / `ada install`（CLI） | CLI | 是（CLI 内） |
| 桌面应用内安装第三方 TS 插件 | 应用（`adaesbuild` 构建标签） | 是（+7.1 MB strip） |
| `ada dev`（开发热重载） | 开发进程 | 是 |
| 应用启动 | 不编译，读 Artifact | 否 |
| 缺失 Artifact 且 `ADA_ALLOW_RUNTIME_COMPILE=1` | 应用（未带标签则失败） | 视构建 |

### 10.6 Sourcemap 与诊断

moejs 报错只有 `File/Line/Column`（指向**编译后**文本），且**不支持 sourcemap**（实测 `//# sourceMappingURL` 被忽略）。方案：

1. 编译器把 esbuild 的 map 存进 Artifact，并对每个模块建 `lineMap`（编译后行 → 原始行/列）。
2. JS 侧异常冒泡到 Go 时，`jsrt` 用 `moejs.StackTrace(exc)` 取出 `at plugin://…:line:col`，再用 `lineMap` 反查源文件位置。
3. 错误对象统一包一层：

```go
type PluginError struct {
    PluginID string
    File     string   // 源文件虚拟名
    Line, Col int
    Code     string   // EACCES / ENOENT / ECANCELED / EPLUGINFAULT …
    Message  string
    Stack    string   // 已重映射
}
```

4. `ada inspect --errors` 展示最近 N 条错误及源码片段（从 `src/` 读，Artifact 里带原始行内容）。

### 10.7 编译期性能（实测）

| 项 | 数值 | 来源 |
|---|---|---|
| `Transform` 524 B TSX | 0.77 ms | 本机实测（Go 1.27.1 / esbuild 0.28.2） |
| `Transform` 5.2 KB TS | 0.88 ms | 同上 |
| 16 goroutine × 50 次并发 | 259 ms / 800 次，0 错误 | 同上 |
| `api.Build` 增量 `Rebuild` | ≈0.51 ms | 同上（虚拟文件系统） |
| 20 文件插件一次全量编译 | ≈15–20 ms | 推算（0.8 ms × 20，串行） |

结论：编译成本对「安装期/启动期一次性」完全可接受；**不应**在做「每次调用都编译」的设计。

---

## 11. 运行时层：moejs 的边界与桥

### 11.1 每个 JS 插件一个 Runtime

```go
type Entry struct {
    art   *plugin.Artifact
    spec  kernel.PluginSpec
    rt    *moejs.Runtime
    loop  *eventloop.Loop
    api   *webapi.API
    mod   *moejs.Module      // Link 后的入口
    q     chan *call          // 容量 = runtime.queue（默认 64）
    state atomic.Int32
}

func (e *Entry) Apply(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
    rt := moejs.NewRuntime(moejs.Options{
        MemoryLimit:        cfg.Int64("runtime.memoryLimit", 64<<20),
        DisableDynamicCode: true,
        Locale:             e.app.locale,
        TimeZone:           e.app.tz,
        Importer:           e.loader.importer(),
    })
    loop, err := eventloop.New(rt, eventloop.Options{OnError: e.onLoopError})
    if err != nil { return nil, err }
    api, err := webapi.Install(loop, e.webOptions(cfg))   // fetch 白名单
    if err != nil { return nil, err }
    if err := e.installGlobals(rt, ctx); err != nil { return nil, err }
    // 模块必须在 loop 回调里 Load（v1 实测约束 D-7）
    if err := loop.Run(func(rt *moejs.Runtime) error { return rt.Load(e.mod) }); err != nil { return nil, err }
    if err := loop.Start(); err != nil { return nil, err }
    // 调 JS apply(ctx, cfg)，返回的 disposer 挂到 Kernel effect
    disposeJS, err := e.callApply(ctx, cfg)
    if err != nil { return nil, err }
    return e.makeDispose(disposeJS), nil
}
```

关键点：

| 项 | 决策 | 依据 |
|---|---|---|
| 一个插件一个 Runtime | 是（默认） | 隔离全局、隔离内存额度、隔离崩溃 |
| 一个插件一条 loop goroutine | 是 | moejs Runtime 必须单 goroutine（C1） |
| `Load` 时机 | 必须在 loop 内 | v1 注释「D-7 实测」 |
| 模块链接 | 装配前 `Link` 完成 | 链接期失败早于运行期 undefined |
| `apply` 调用 | 一次，传 `(ctxJS, config)` | Cordis 单次 apply；重载=新 Runtime 再 apply |
| 池化 | `runtime.pool > 1` 时 N 个 Runtime 共享同一 `*Module` | `*Module` 可跨 Runtime 并发（C2）；月状态必须外置 |

### 11.2 池化插件（`pool: N`）

```
调用 → 选择空闲 slot（round-robin / least-queue）
     → RunOnLoop(slot) → 执行 → 归还
```

- 仅允许 `"stateless": true` 的插件声明 `pool > 1`（清单校验时检查：无 `ctx.store` 写、无 `jobs`、无可变模块级状态——静态扫描 + 作者承诺）。
- 池化不改变 `*Module` 共享；每个 slot 自己的全局与堆。
- 收益：无状态工具类插件（格式化、解析、签名）在并发调用下线性扩展；有状态插件（会话、调度）保持 1。
- **已预验**：12 个 Runtime 并发加载同一 `*Module`、各自 200 次调用，0 错误（spike S6，2026-11-19）。

### 11.3 调用协议

```go
type call struct {
    id       string
    kind     CallKind        // tool | action | route | provider | event | service | init
    method   string
    args     json.RawMessage
    ctx      CallContext     // threadId/runId/signal/deadline
    done     chan outcome
}
type outcome struct { data json.RawMessage; err error }
```

- **唯一数据格式是 JSON**：`json.RawMessage` 进、`json.RawMessage` 出，避免二次解码；工具结果上限 1 MiB、路由响应 64 KiB（v1 同款限额）。
- **取消**：`CallContext.signal` 到 JS 侧是只读 AbortSignal 形状对象（v1 `objects.go`）；取消时 `loop.Interrupt(errInterruptCancel)`。
- **同步/异步统一**：JS handler 返回 Promise 时走 thenable 结算（`then(onOk, onErr)`），Go 侧只等 `done`。
- **宿主长调用**（fetch、确认框、模型流）：`hostAsync` → 新 goroutine + `loop.NewPromise()`，期间暂停预算计时（v1 `budget.pause/resume` 语义），避免「等待用户 3 分钟」被算作插件超时。

### 11.4 预算、熔断、内存与中断

| 机制 | 参数（默认） | 行为 |
|---|---|---|
| 初始化超时 | 5 s（清单 `initTimeoutMs`，上限 60 s） | `apply` 未结算 → 装配失败 |
| 调用超时 | 30 s（`callTimeoutMs`，工具描述符可覆盖） | 超时先探活，1 s 内不响应才丢弃 Runtime |
| 内存上限 | 64 MiB（`runtime.memoryLimit`） | moejs `MemoryLimit` → `ErrMemoryLimit`，脚本不可 catch |
| 队列 | 64 | 超出返回 `EBUSY`，由调用方退避 |
| 熔断 | 连续 3 次失败 → 禁用，冷却 5 min，指数至 10 min | 半开探测重建 Runtime；取消不计失败（v1 `breaker.go`） |
| 动态代码 | 默认关闭（`DisableDynamicCode`） | `eval`/`Function` 抛 `EvalError` |
| 栈深 | moejs 常量 512 | 「Maximum call stack size exceeded」 |
| 强制回收 | 10 s | `Interrupt` + 关闭 loop，记泄露 effect |

### 11.5 热重载（这是与 Cordis 最大的差异点）

Cordis 靠 Node 的模块缓存替换实现 HMR；moejs **没有进程内代码替换**，且一个 Runtime 只能 `Load` 一个模块（C1）。因此 v2 的重载模型是：

```
Reload(fiber)：
  1. 重新编译（内容哈希命中则用缓存 Artifact）
  2. 用新 *Module 建新 Runtime/Loop（旧的不动）
  3. 逆序执行旧 fiber 的 effect disposer
  4. 把旧实现的服务标记为「退役」，等依赖者全部迁移
  5. 新 fiber 进入 loading → apply → active
  6. 旧 Runtime 关闭
```

保证：

- **服务不中断**：旧服务在新实例 `active` 之后才注销（如果新实例失败，旧实例继续服务，fiber 保持 active 并记录 `reloadFailed`）。
- **状态迁移**：需要跨重载保留的状态必须放 `ctx.store`（插件私有持久 KV）或以事件 `internal/reload` 交接；内存状态一律丢弃（这是显式契约，写进 SDK 文档）。
- **级联**：provider 重载 → UID 变 → 依赖者 epoch 变 → 依赖者重载（拓扑序，§6.2）。
- **开发期**：`ada dev` 监听源文件（去抖 200 ms），只重编译变更模块（按内容哈希），只重载受影响的 fiber 及其依赖者。

### 11.6 JS 侧 API 面

| 模块 | 内容 | 权限 |
|---|---|---|
| `ada:api` | 纯类型（`export {}`），编译期擦除 | 无 |
| `ada:fs` | `read/write/list/stat/watch`，路径限定挂载点 | `fs.read` / `fs.write` |
| `ada:exec` | `run`（无 shell、程序级 allowlist） | `exec` |
| `ada:proc` | 长驻进程 start/write/read/kill | `process` |
| `ada:clock` | `now/after/timer` | 无 |
| `ada:secrets` | `get(name)`（仅清单声明过的） | `secrets.<name>` |
| `ada:ui` | `notify/openURL/confirm/page/contribute` | `ui.*` |
| `ada:store` | `get/set/delete/list`（插件私有） | 无 |
| `node:path` `node:url` `node:util` `node:events` `node:buffer` `node:crypto` `node:zlib` `node:os` `node:string_decoder` | 纯计算兼容层（v1 `nodemods` 移植） | 无 |
| `node:fs` / `node:fs/promises` | **仅当授权**，走 `os.Root` 沙箱 | `fs.*` |
| `node:child_process` | **仅当授权**，无 shell | `exec` |

全局:`setTimeout`/`setInterval`/`setImmediate`(事件循环,**调用边界内**生效,见
[event-loop.md](event-loop.md) 与 ADR 0014)、`structuredClone`(引擎自带);
`console`、`fetch`(webapi)仍是未实现项——裸 Runtime 没有它们,宿主也尚未安装。

**数据面规则**（继承 v1 的核心经验）：大对象不过边界。图片/音频/大文件由 Go 侧持有，JS 只拿 URL 或一次性小句柄；模型流式增量以 `emit` 回调推送（每帧小 JSON），不累积成大数组跨边界。

### 11.7 崩溃隔离

- JS 抛异常：只影响该次调用（`*Exception` → `PluginError`），fiber 保持 active。
- JS 卡死/超时：丢弃 Runtime（该插件后续调用自动重建），fiber 记一次失败；连续 3 次进入熔断禁用。
- 宿主侧 panic（`*InternalError`）：丢弃 Runtime 并记录，绝不吞掉（v1 同款）。
- 一个插件崩溃不波及其它插件：因为每个插件独立 Runtime/loop，内核只看到「该 fiber 失败」。

---
## 12. 编排层（Orchestration）

编排不是「多写几个管理器」，而是三个有明确输入输出产物的子系统（外加一条决定「agent core 自己怎么组合」的规则，见 §12.6）：

| 编排 | 回答的问题 | 产物 | 触发时机 |
|---|---|---|---|
| **装配编排** | 谁先加载、谁等谁、失败了怎么办 | `Plan`（数据）+ `Assembly`（运行态）+ 报告 | 启动、安装、配置变更、重载 |
| **交互编排** | 插件之间怎么协作、策略怎么拦截、跨插件流程怎么跑 | 服务调用 + 事件链 + `Flow` 流水线 | 每次业务调用 |
| **调度编排** | 什么时候加载、什么时候回收、配额怎么分 | 激活状态机 + 配额账本 + 任务计划 | 运行期持续 |

### 12.1 装配编排（Assembly）

#### 输入

```go
type PlanInput struct {
    Sources   []plugin.Source   // builtin 集合 + 已安装插件 + 组合包
    Patch     *profile.Document // 分层 patch（§12.4）
    Env       map[string]string // 环境覆盖
    HostCaps  caps.HostCaps     // 宿主能提供的能力（含未授权项）
}
```

#### 算法（六步）

```
1. 归一化    patch 分层合并 → entries（id 唯一、disabled 求值、isolate/intercept 展开）
2. 预检      每个 entry：manifest schema / apiVersion / engines.ada / trust / 签名 /
             能力授权 / Artifact 存在 / Compile+Link 通过（不执行代码）
3. 建图      provider(name,version) → consumers；含 injectOptional 的虚线边
4. 排序      拓扑序 + 同层按 priority/id；环 → 报告给出环路径（不崩溃）
5. 装载      按拓扑序逐 fiber：Go 直接 Apply；TS 建 Runtime → Load → apply
             （互不依赖的 entry 可并行装载，服务注册串行化到内核）
6. 提交      静态贡献预注册 + 运行期贡献 staged → 原子 commit；失败 unwind
```

伪代码：

```go
func (a *Assembler) Apply(ctx context.Context, in PlanInput) (*Assembly, error) {
    plan, err := a.Plan(in)          // 纯函数：可 diff、可快照、可测试
    if err != nil { return nil, err }

    gen := a.app.newGeneration()
    var loaded []*Fiber
    for _, node := range plan.Order {        // 拓扑序
        f, err := a.loadOne(ctx, node, gen)
        if err != nil {
            if node.Critical { return nil, a.rollback(gen, loaded, err) }
            gen.report.Fail(node, err)        // 非关键：记录并继续
            continue
        }
        loaded = append(loaded, f)
    }
    if err := gen.commit(ctx); err != nil { return nil, a.rollback(gen, loaded, err) }
    return gen.assembly, nil
}
```

#### 失败策略

| 情况 | 处理 |
|---|---|
| 依赖未满足 | fiber `pending`，**不算失败**；报告列「等待谁」 |
| 依赖成环 | 全部参与者 `pending`；报告给环路径；可用 `injectOptional` 或 `disabled` 破环 |
| 预检失败（schema/签名/能力） | 该 entry 跳过，状态 `failed`，出报告 |
| `apply` 失败 | 该 fiber 回滚贡献 → `failed`；`critical` 则整代失败 |
| 提交中途失败 | 该 fiber 贡献 unwind；已装载的其它 fiber 不受影响（除 `critical`） |
| 整代失败 | **保留上一代装配**（v1 双代模型的细粒度版），旧 Runtime 不关 |

#### `Assembly` 对象

```go
type Assembly struct {
    Gen      uint64
    Fibers   map[string]*Fiber
    Report   *Report            // 每 entry：阶段耗时、状态、依赖、警告
    Services *ServiceTable
    Started  time.Time
}

type Report struct {
    Phases []PhaseTiming        // normalize/preflight/graph/order/load/commit
    Entries []EntryReport       // id、状态、错误、依赖、警告、耗时
    Cycles  [][]string
    Warnings []string
}
```

CLI 直接消费：`ada plan`（不执行，打印 Plan + 图）、`ada up`（执行并打印 Report）、`ada graph --dot`（导出依赖图）。

#### 增量装配

- 改一个 entry 的 config → 只 `fiber.Update`，不重建整代（§8.3）。
- 装一个新插件 → 不整代重建：`Assembler.Add(entry)` 走同一套预检 + 图增量插入 + 拓扑校验（如果引入环，拒绝安装并提示）。
- 卸载 → 只 `Fiber.Dispose`，依赖它的消费者回落到 `pending`（而不是失败）。

### 12.2 交互编排

#### (1) 服务调用（同步协作）

```go
tools, err := kernel.Service[*tools.Registry](ctx, "tools")
res, err := tools.Run(ctx, "read_file", args)   // 内核自动加 timeout/span/熔断
```

治理统一在服务层的 decorator（内核在 `Provide` 时包一层）：

```
call span → 授权检查（若服务声明了 caps）→ 预算（timeout/deadline）
         → 熔断计数 → 审计（成功/失败/字节数）→ 实际实现
```

#### (2) 事件中间件（策略协作）

典型链（都是插件，可插拔）：

```
message/before  ──(waterfall)──▶ [system-prompt] ▶ [safety-filter] ▶ [context-budget] ▶ …
turn/before     ──(waterfall)──▶ [approval-gate] ▶ [tool-policy] ▶ …
llm/request     ──(waterfall)──▶ [token-cap] ▶ [router] ▶ [retry] ▶ provider
tools/result    ──(emit)───────▶ [telemetry] [audit] [cost]
```

`approval-gate` 就是「调用 `approval` 服务，不通过就 `return nil, ErrAborted` 且不调 `next`」——**策略编排不需要内核知道审批的存在**。

#### (3) 流水线（`flow`，跨插件编排的声明式形态）

Cordis/DSH 里没有独立流水线原语，但它的 `waterfall + service` 天然构成链。v2 把它提升为一等公民，因为「插件编排」最实用的落点就是**让应用作者用数据描述一条跨插件流程**。

```go
f := flow.New("review").
    Step("read",   flow.Use("fs", "read")).                    // 调服务
    Step("scan",   flow.Event("message.scan", flow.Waterfall)). // 走事件链
    Step("model",  flow.Use("model", "complete")).
    Step("post",   flow.If(`$.scan.verdict == "block"`, flow.Abort("blocked"))).
    OnFailure("notify", flow.Event("turn/failed", flow.Emit)).
    Commit()
```

等价的声明式文件（非 Go 作者也能写）：

```jsonc
{
  "flow": "review",
  "input": { "$ref": "#/schemas/review-input.json" },
  "steps": [
    { "id": "read",  "use": { "service": "fs",    "method": "read" } },
    { "id": "scan",  "event": { "name": "message.scan", "mode": "waterfall" } },
    { "id": "model", "use": { "service": "model", "method": "complete" },
      "retry": { "max": 2, "backoff": "500ms" }, "timeout": "120s" },
    { "id": "post",  "when": "$.scan.verdict == 'block'", "abort": "blocked" }
  ],
  "onFailure": [ { "id": "notify", "event": { "name": "turn/failed", "mode": "emit" } } ]
}
```

能力清单：

| 能力 | 说明 |
|---|---|
| `use` / `event` / `plugin` | 步骤三种来源：服务方法、事件链、直接指定插件方法 |
| `when` | 受限表达式（与 `disabled` 同一求值器，§12.4） |
| `parallel` / `fanout` | 并发分支 + `join`；`errgroup` 语义，任一失败可 `abort` 或 `collect` |
| `forEach` | 对数组逐项执行子步骤，带并发上限 |
| `retry` / `timeout` | 每步独立；`retryable` 由错误码决定 |
| `checkpoint` | 落 `ctx.store`；崩溃后可 `Resume(flowRunID)` 从最后检查点继续 |
| `subflow` | 嵌套子流程；共享 trace ID |
| `OnFailure` / `Compensate` | 失败补偿（尽力而为、逆序） |

流水线**不引入新的并发模型**：每步仍是「调用某插件」，受同一预算/熔断/审计管辖；`Flow` 只是把调用顺序变成数据，从而可被配置、可被替换、可被审计。

#### (4) 跨插件直接调用（不推荐，但允许）

清单里不写依赖时，插件 A 不能拿到 B 的服务（可见性门禁）。若确需「按名字调用某个插件的工具」，走 `tools` 服务：

```go
res, err := kernel.Service[*tools.Registry](ctx, "tools").Invoke(ctx, "other-plugin/some_tool", args)
```

这条路径受工具 schema、超时、审计约束，且不会绕过能力授权——比 v1 的「动作绑定 + 清单声明 backend」更通用。

### 12.3 调度编排（Scheduling）

#### 激活状态与事件

```
cold ──onService/onCommand/onRoute/onPage/onCron/onStartup──▶ loading ──▶ active
  ▲                                                                    │
  └──────────────────── idleTTL 到期 / 手动 suspend ───────────────────┘
```

| 激活方式 | 触发 | 适用 |
|---|---|---|
| `onStartup` | 装配时立即 | 基座、必需服务提供者 |
| `onService:name` | 该服务首次被请求 | 大多数工具插件 |
| `onCommand:id` | 命令面板/快捷键 | 命令类插件 |
| `onRoute:GET /x` | 首次请求该路由 | HTTP 适配器 |
| `onPage:slug` | 首次打开页面 | UI 插件 |
| `onCron:expr` | 到点 | 定时任务 |
| `onDemand` | 显式 `App.Ensure(id)` | 测试/编排 |

**延迟激活**与 Cordis 的 `inject` 门控互补：Cordis 解决「依赖齐了才能跑」，这里解决「依赖齐了也别急着跑」。

#### 闲置回收

```
if fiber.State == active && no calls for idleTTL && no dependents {
    fiber.Dispose()   // 回到 cold；Artifact 与贡献声明仍在目录里
}
```

约束：`critical`、被 `Pin` 的、有 `jobs` 的、提供 `model`/`tools` 这类基座服务的插件不回收。

#### 配额与回压

```go
type Quota struct {
    MaxCallsPerMinute int
    MaxCPUTime        time.Duration   // 每 5 分钟窗口
    MaxMemory         int64
    MaxConcurrent     int
}
```

- 内核在调用前记账（毫秒级原子计数），超限返回 `EQUOTA` 并记审计。
- **回压**：插件队列满 → `EBUSY`；调用方可退避或降级（把「插件不可用」当成正常分支处理）。
- 全局 governor：所有 JS 插件总内存/总并发有上限（默认 `min(4*GOMAXPROCS, 64)` 个并发调用）。

#### 任务（jobs）

- `contributes.jobs` 声明 cron；`sched` 用纯 Go cron 解析器（5 段）计算下一次触发。
- 错过补跑策略：`skip` / `once` / `all`（默认 `once`），落 `store` 记录。
- 单机单实例：`store` 里用租约（lease）防多窗口重复执行。

### 12.4 组合编排（Bundle / Profile / Patch）

> 实现口径的 preset 字段(`name` / `plugins` / `enable` / `disable` / `patch`)见
> [m3-assembly.md](m3-assembly.md) §4。`enable` 是**候选白名单**:目录可以指到统一的
> `plugins/`,装配集合由 preset 收口;同一个 id 同时出现在 `enable` 与 `disable` 表示
> 「收下但默认不启用」(宿主运行期可打开备选实现)。

DSH 的分层：bundle 自带 `cordis.patch.yml`，profile 再叠 patch，用户能按 id 覆盖。v2 照搬结构，换名字：

```
$ADA_HOME/
  bundles/
    ada-base/ada.bundle.json          # 基座组合（官方）
    ada-agent/ada.bundle.json         # agent 组合
  profiles/
    default/
      ada.patch.yml                   # 用户配置
      plugins/                        # 已安装插件（每插件一目录）
  cache/
```

`ada.bundle.json`：

```jsonc
{
  "name": "ada-agent",
  "version": "2.0.0",
  "engine": ">=2.0.0",
  "entries": [
    { "id": "model-router", "plugin": "builtin/model-router",
      "config": { "default": "openai" } },
    { "id": "tools-fs", "plugin": "official/tools-fs" },
    { "id": "telemetry", "plugin": "community/telemetry", "disabled": "!features.telemetry" }
  ]
}
```

叠加规则：

1. **同 id 整体替换 config**（不做深合并——深合并是配置事故的高发区；需要合并时用 schema 默认值）。
2. `insert_before/after` 只表达**提示顺序**，最终顺序由依赖图决定；与依赖冲突时以依赖为准并记 warning。
3. `disabled` / `when` 用受限表达式求值（见下）。
4. `isolate` / `intercept` 可在 entry 上声明，展开成 §5.4 的标签。

#### 受限表达式求值（替代 `!!js`）

不允许在 YAML 里执行任意 JS（供应链 + 不可复现）。两个候选：

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| 自研表达式解析（`!`, `&&`, `\|\|`, 比较、字面量、路径访问） | 无执行风险、可静态检查 | 表达力弱 | **采用** |
| 用 moejs 在只读 realm 里求值 | 表达力强 | 需要冻结全局、限制时间、仍是 JS | 作为 `expr.lang: "js"` 显式开启的逃逸（默认关） |

默认求值器只允许访问 `env`、`os`、`profile`、`features` 四个只读命名空间，禁止赋值/函数调用/属性写入；10 微秒级求值，无 I/O。

### 12.5 编排产物与可观测

| 产物 | 内容 | 消费者 |
|---|---|---|
| `Plan` | 归一化 entries + 图 + 顺序 + 预检结果 | `ada plan`、CI diff、测试黄金文件 |
| `Assembly` | 运行时 fiber 集合 + 服务表 | 宿主、`ada inspect` |
| `Report` | 六阶段耗时、每 entry 状态/错误/警告、环路径 | 启动面板、错误上报 |
| `Trace` | 每次调用的 span 树（插件/服务/流水线步骤） | 诊断、性能 |
| `Graph` | 依赖/服务/贡献三张图（JSON + DOT） | 文档、评审 |

---

### 12.6 Agent Core 也是插件组合（preset）

前五节描述的是「怎么装配插件」；这一节回答一个更容易被忽略的问题：**agent core 自己是不是插件**。在 DSH 里答案是**是**，而本方案初稿把它留在了宿主里（只把 model/tools 写成「内置服务」），这是一处必须修正的偏差。

#### DSH 的实证

`references/packages.md` 的 `core` / `llm` / `compaction` / `interaction` / `subagent` 组里，agent 的每一块都是可装载插件：

| 包 | 描述（原文摘译） | 对应本方案 |
|---|---|---|
| `dsh-agent` | Agent 接口、注册表、initiator scope 与事件词汇 | `plugins/core/agent` |
| `dsh-agent-loop` | **The concrete agent loop plugin** | `plugins/core/agent-loop` |
| `dsh-session` | 事件溯源 session store | `plugins/core/session` |
| `dsh-system-prompt` | 系统提示组装注册表 | `plugins/core/system-prompt` |
| `dsh-tools` / `dsh-tool-executor-native` / `-ptc` | 工具注册表、执行管线、执行器 | `plugins/seam/tools` + `executor/*` |
| `dsh-llm` / `dsh-llm-deepseek-*` / `dsh-llm-retry` | LLM **seam** + provider + 策略 | `plugins/seam/llm` + `provider/*` + `policy/*` |
| `dsh-compaction-basic` / `-tool-result-pruner` / `-image-offload` | 压缩策略 / 工具结果裁剪 / 图片卸载 | `plugins/core/compaction-*` |
| `dsh-user-approval` / `dsh-user-questions` | 审批与提问 seam | `plugins/core/approval` |
| `dsh-subagent` + 各 backend | 子代理 seam + 进程内/进程外实现 | `plugins/core/subagent` + `backend/*` |
| `dsh-sandbox-policy` / `dsh-sandbox-local` | 沙箱策略 + 后端 | `plugins/policy/sandbox` + `provider/sandbox-*` |
| `dsh-schedule` / `dsh-workflow-ptc` | 调度、工作流引擎 | `plugins/core/schedule`、`plugins/core/workflow` |

组合方式也全是数据：`dsh-agent-preset` 是一个**插件**，它的 config 就是 `plugins` 列表；preset 由 bundle patch 声明，`presets/standard.patch.yml`、`ptc`、`minimal`、`cordis` 只是四份不同的列表（`references/compositions.md`）。文档原话：「Host plugins supply shared services: **tools and prompt registries, the Agent loop, sessions, persistence, settings, sandbox policy, model routes, and subagent backends.**」

结论：**DSH 中只有 Cordis 内核不是插件**，其余全是；agent core 是「一组核心插件 + 一个 preset 声明」。

#### 修正后的边界

| 层 | 是否插件 | 内容 | 判据 |
|---|---|---|---|
| **内核** | ✗ | Context/Fiber/Service/Effect/Event/Config/Registry/Provider | 「不知道 agent 是什么」的机制 |
| **基础设施** | ✗ | assemble/compile/jsrt/plugin/caps/observe | 装载与执行机制，本身无领域语义 |
| **平台 seam** | ✓ | llm/tools/fs/shell/sandbox/storage/settings/secrets/jobs/schedule/telemetry/ui | 定义「能力接口」，provider/policy 可换 |
| **Agent Core** | ✓ | agent/agent-loop/session/system-prompt/context/compaction/approval/questions/subagent/goal/plan/team/workflow | 定义「agent 干什么」 |
| **组合** | 数据 | preset/bundle/patch | 定义「装哪些」 |

判据一句话：**如果删掉它，内核仍能装配别的插件、跑别的产品，那它就是插件**。

#### 三段式：seam / provider / policy

DSH 把每个平台能力拆成三层，这是比「内置服务」更精确的结构：

| 段 | 职责 | 例子 | 本方案包名 |
|---|---|---|---|
| **seam** | 只声明服务接口、契约版本、错误分类；**无实现** | `ctx.llm`、`ctx.tools`、`ctx.fs` | `plugins/seam/llm` |
| **provider** | 具体实现（本地文件、DeepSeek、SQLite…） | `dsh-fs-local`、`dsh-llm-deepseek` | `plugins/provider/fs-local` |
| **policy** | 以 waterfall/guard 叠在 seam 上的策略 | `dsh-llm-retry`、`dsh-fs-sandbox`、`dsh-tool-call-timeout-policy` | `plugins/policy/llm-retry` |

好处：

1. **seam 可空转**：没有 provider 时 seam 存在但调用返回 `EUNIMPLEMENTED`，界面显示「未配置后端」，而不是整个产品起不来。
2. **策略可叠加、可撤销**：换审批策略、加超时、加沙箱都是加一个 policy 插件，不改 provider。
3. **权限与信任挂在 seam 上**：能力授权检查在 seam 的实现入口做一次，provider 无需重复实现（§13.3 的强制点因此更简单）。
4. **测试友好**：seam 可注入 fake provider（`dsh-llm-replay` 就是这个思路：用录制的会话 JSONL 回放模型块）。

#### ada v2 的插件清单（v2.0 目标形态）

```
plugins/
  seam/            llm · tools · prompts · fs · shell · sandbox · storage · settings
                   secrets · clock · commands · jobs · schedule · telemetry · ui
  provider/        llm-openai · llm-anthropic · llm-local(replay) · fs-local · shell-local
                   sandbox-none · storage-json · storage-sqlite · store-local
  policy/          llm-retry · llm-token-cap · fs-sandbox · fs-observation
                   tool-timeout · tool-repeat-guard · approval-gate · context-budget
  core/            agent · agent-loop · session · system-prompt · context-assembly
                   compaction-basic · compaction-pruner · approval · user-questions
                   subagent · goal · plan · team · workflow
  tool/            fs · fs-search · shell · web · todo · skill · office · job
  ui/              pages · slots · panels（宿主渲染，§14.3）
presets/
  base.preset.json     最小可用：agent + loop + session + system-prompt + seam/llm + seam/tools
  coder.preset.json    加 fs/shell/web/todo/plan
  designer.preset.json 加 ui/pages + design 工具
  headless.preset.json 无 ui，供 CLI/SDK
```

preset 的形态（数据，不是代码）：

```jsonc
// presets/coder.preset.json
{
  "preset": "coder",
  "name": "Coding Agent",
  "engine": ">=2.0.0",
  "plugins": [
    { "id": "agent",        "plugin": "core/agent" },
    { "id": "agent-loop",   "plugin": "core/agent-loop", "config": { "maxParallelTools": 4 } },
    { "id": "session",      "plugin": "core/session", "config": { "persist": "jsonl" } },
    { "id": "prompt",       "plugin": "core/system-prompt" },
    { "id": "llm",          "plugin": "seam/llm" },
    { "id": "llm-deepseek", "plugin": "provider/llm-deepseek" },
    { "id": "llm-retry",    "plugin": "policy/llm-retry" },
    { "id": "tools",        "plugin": "seam/tools" },
    { "id": "fs",           "plugin": "seam/fs" },
    { "id": "fs-local",     "plugin": "provider/fs-local" },
    { "id": "fs-sandbox",   "plugin": "policy/fs-sandbox" },
    { "id": "text-helper",  "plugin": "tool/text-helper" },
    { "id": "shell-helper", "plugin": "tool/shell-helper" },
    { "id": "compaction",   "plugin": "core/compaction-basic" },
    { "id": "approval",     "plugin": "core/approval", "config": { "mode": "ask" } }
  ]
}
```

装配顺序**不写在 preset 里**：preset 只声明集合，顺序由 `inject`/`provide` 的拓扑序推导（§12.1）。这是 DSH 的原则（「列表顺序不决定加载顺序」），也是本方案 §12.1 的设计。

#### 对实现的四个具体要求

1. **agent-loop 用 Go 插件实现（默认）**。循环在热路径上：每一步都要读 session、组装 context、调 llm、跑工具；用 JS 实现会把每次消息/增量过 JSON 桥（~7 µs + 序列化），并让 token 流经过两次边界。Go 插件零桥接开销；TS 插件仍可写一个「实验循环」，通过同一 seam 替换。
2. **内核不出现 agent 词汇**。`kernel` 包里不允许出现 `Turn`、`Message`、`ToolCall`、`Model` 这类类型；一旦出现，说明该能力应该下沉成插件或 seam。CI 用 grep 守护。
3. **seam 契约版本化**。每个 seam 声明 `ServiceSpec{Version}`（§5.1）；provider 声明它实现的版本；preset 校验「seam 与 provider 版本匹配」；不匹配 = 依赖未满足（`pending`），不是运行期 panic。**首批四个 seam（llm / tools / session / prompts）的接口草案已单独成文：[seam-api.md](seam-api.md)**。
4. **preset 即产品**。桌面、headless、server 的差异只允许两种来源：preset 列表 + host 适配器。**不允许**为某个产品在 platform/kernel/core 里加分支。

#### 代价与对策

| 代价 | 表现 | 对策 |
|---|---|---|
| 多一跳间接 | 每步多几次接口调用/事件分发 | Go 接口调用 ns 级，相比 LLM 延迟可忽略；内部事件用 typed 直调，不走反射 |
| 启动更碎 | 需要装配 20–40 个插件 | 懒激活（§12.3）+ Artifact 缓存；50 插件冷启动 ≤150 ms（§15.2） |
| 调试更绕 | 「这行为是哪个插件加的」 | `ada why`、`ada inspect` 的 fiber/服务/effect 树；事件链可视化 |
| 契约漂移 | seam 改了，provider 没跟上 | seam 版本 + 契约测试（每个 seam 一组 provider 兼容测试） |
| 空转风险 | seam 有、provider 无 | seam 调用返回 `EUNIMPLEMENTED` 并在 UI/CLI 明示「未配置」 |

---

## 13. 能力、安全与信任

### 13.1 威胁模型

| 威胁 | 场景 | 对策 |
|---|---|---|
| 恶意插件读文件/外传 | 第三方插件偷凭据 | `fs.*` 限 `os.Root`；secret 按名授权；网络出站**不做**宿主侧白名单（ADR 0011/0012，见下条说明） |
| 恶意插件执行命令 | 下载器/挖矿 | `exec` 程序级 allowlist + 无 shell + 进程组 kill |
| 资源耗尽 | 死循环/内存炸弹 | 预算 + `MemoryLimit` + 熔断 + 配额 |
| 供应链投毒 | 安装包被替换 | Artifact digest + Ed25519 签名 + 固定版本 + 隔离安装目录 |
| 配置注入 | `!!js` 执行任意代码 | 受限表达式求值（§12.4） |
| 越权协作 | 拿到别人的服务 | 服务可见性 = `inject` 白名单（不是运行时判断） |
| 权限升级 | 声明 builtin 自抬身价 | 信任由**安装来源**决定，磁盘自声明降级（v1 `CapTrust` 已有） |

**一个被低估的收益**：纯 Go 工具链意味着 **npm 生命周期脚本（postinstall）根本不存在**——这是 Node 插件生态最大的供应链攻击面，v2 天然消掉。

**实现优先（AGENTS §0 红线 6）**：威胁模型是用来**给已实现的能力加边界**的，不是用来否决需求的。任何「出于安全考虑不提供 X」的结论必须走 ADR（说明威胁模型、被否决的具体需求、替代路径、复议日期）；更常见的正确做法是实现 X，把边界做成可关闭的 `policy/*` 或 preset 字段。默认值可以保守，但「默认拒绝」不等于「没有实现」——前者是策略，后者是能力缺失。本节余下各条都按这个顺序读：**先有能力，再有强制点**。

**网络不在强制点范围内**：ADR 0011 决定 Node 网络模块直连标准库，ADR 0012 进一步移除了 `seam/net`、`net-guard`、`net-observe`。因此本表与 §13.3 都不再列 `net.fetch`；需要「能读文件但不能外传」的产品，应靠 `secrets` 按名授权 + 不在 preset 里启用网络相关工具，而不是假设出站被门禁拦住。

### 13.2 信任等级与能力上限

| trust | 来源 | 能力上限 | 签名 |
|---|---|---|---|
| `builtin` | 随二进制编译 | 全部 | 不需要（代码在二进制里） |
| `official` | 官方仓库发布 | 全部（`exec`/`process` 仍逐项授权） | 必须 |
| `verified` | 第三方 + 官方审核签名 | 除 `process.isolated` 外 | 必须 |
| `third-party` | 任意来源 | 只读文件、白名单网络、无 exec | 可选（无签名则不可晋升） |

规则：

- **信任来自安装来源**，不来自清单自述；清单写 `builtin` 而来源是磁盘 → 降为 `third-party` 并记 warning（v1 `CapTrust`）。
- 能力必须 `MinTrust ≤ trust`，否则清单校验失败（v1 `ValidateCapabilityList`）。
- **能力=授权**在调用点强制（v2 恢复 v1 丢掉的东西）：

```go
func (b *Broker) Authorize(ctx context.Context, p PluginRef, cap caps.Cap, target string) error {
    if !b.grants(p.ID).Has(cap, target) {
        b.audit.Deny(p.ID, cap, target)
        return &caps.DeniedError{Cap: cap, Target: target, Plugin: p.ID}
    }
    return nil
}
```

### 13.3 强制点（每个能力在代码里的唯一入口）

**文件、沙箱、执行三者的具体契约见 [seam-api-security.md](seam-api-security.md)**：强制类策略用**装饰器**（确定性、可排序、同步可拒），观察类用事件；`fs`/`shell` 都必须先经 `sandbox.Resolve`。

| 能力 | 强制点 | 实现要点 |
|---|---|---|
| `fs.read` / `fs.write` | `caps/fs` | 虚拟路径重写 → `os.Root`；`..`/符号链接逐段 `EvalSymlinks`；写白名单=工作区+插件自己目录 |
| `exec` | `caps/exec` | 无 shell；程序级 allowlist；超时 120s；输出 4 MB；杀进程树 |
| `process` | `caps/process` | 生命周期 30 min；每插件 8 个；env 只给清单声明 |
| `secrets.<name>` | `caps/secrets` | 只返回清单声明过的名字；不落日志（正则脱敏） |
| `ui.*` | `caps/ui` | 必须由宿主 UI 线程执行；确认框回传结构化结果 |
| `config.<ns>` | `caps/config` | 只能读写声明的命名空间 |

所有强制点**共享一个 audit 出口**（JSONL：`ts / plugin / cap / target / bytes / result / traceID`），日志脱敏规则与 v1 相同。

### 13.4 签名与安装

```
安装流程：
  1. 拉 zip / 目录 → 临时目录 .install-<rand>（不覆盖已装版本）
  2. 校验路径安全：IsLocal 逐条、拒绝 symlink 越界、文件数/大小上限
  3. 读 plugin.json → schema 校验 → apiVersion/engines 检查
  4. 校验 Artifact digest（若带 dist/）或就地编译生成 Artifact
  5. 验签：ed25519(publicKey, "ada-plugin-v2\n" + manifestJSON + "\n" + digest)
  6. 能力清单 → 用户同意（CLI 显式 flag；桌面弹「安装确认」）
  7. 原子 rename 到 plugins/<id>@<version>/，写 index.json
  8. 失败 → 删除临时目录，旧版本不受影响
```

- **签名覆盖 Artifact digest 而非目录**：运行的是 Artifact，签目录没有意义（v1 签目录是历史包袱）。
- 公钥来自 `$ADA_HOME/keys/*.json` + 官方 keyring；无公钥时签名视为「未验证」，只能停在 `third-party`。
- 已装插件不可自我更新代码：更新 = 走同一安装流程；运行中的 fiber 不受影响直到 `Update`。

### 13.5 运行期安全默认值

| 默认 | 值 | 理由 |
|---|---|---|
| 网络 | 关闭（无 `fetch`） | 与 v1 一致：`Client == nil` 时每次 `fetch` 报 TypeError |
| 文件 | 只读工作区 | 写需显式 `fs.write` |
| exec/process | 关闭 | 需 `verified` 及以上 + 显式同意 |
| 动态代码 | 关闭 | 防 `eval` 绕过静态审计 |
| 内存 | 64 MiB | 超限即中断，脚本不可 catch |
| 调用超时 | 30 s | 长任务用 `hostAsync` 暂停计时，而不是放松上限 |
| 队列 | 64 | 防雪崩 |

---
## 14. 可观测性、工具链与宿主集成

### 14.1 日志、跨度、指标、健康

| 信号 | 形态 | 内容 |
|---|---|---|
| 日志 | 结构化 JSON（`slog`） | `ts, level, ns=<plugin>.<component>, msg, fiber, entry, gen` |
| 跨度 | 调用树 | `span{kind=call\|service\|flow.step\|event\|assemble, plugin, method, dur, bytes, err}` |
| 指标 | 计数器/直方图 | 调用数、失败率、p50/p99、内存、队列深度、重载次数、装配耗时 |
| 健康 | `Health` 结构 | 每插件：state、最近错误、熔断状态、预算余量、依赖满足情况 |
| 审计 | JSONL 追加 | 能力调用、拒绝、安装、配置变更 |

约定：

- 所有跨边界调用都开 span，`traceID` 一路带到 JS 的 `ctx.traceId`，便于「一次用户操作 → 哪些插件 → 哪些能力」。
- 日志命名空间由 fiber 路径推导（`profile/plugins/tools-fs`），与 Cordis `ctx.logger(name)` 同构。
- 错误分级：`plugin.fault`（插件 bug）/ `plugin.violation`（越权）/ `host.fault`（宿主 bug）/ `transient`（网络/超时）。

### 14.2 `ada` CLI

| 命令 | 作用 | 关键输出 |
|---|---|---|
| `ada new <id>` | 生成脚手架（plugin.json + src/index.ts + config schema + tsconfig + 测试） | 目录 |
| `ada check [dir]` | 形状 + schema + 编译 + 链接 + 静态规则（不执行） | 诊断列表 |
| `ada plan` | 只算装配计划 | Plan JSON + 图 |
| `ada up` | 启动内核（headless），打印装配报告 | Report + 常驻 |
| `ada graph` | 导出三张图 | DOT / JSON |
| `ada dev` | 监听 + 增量编译 + 热重载 | 热重载日志 |
| `ada pack` | 生成 Artifact + digest + 签名 | `dist/plugin.artifact.json` |
| `ada install <id\|path\|repo>` | 安装（含同意流程） | 安装记录 |
| `ada list` / `ada inspect` | 已装插件 / 运行态诊断（fiber/服务/effect/贡献/健康） | 表格 |
| `ada why <service\|id>` | 解释「谁提供、谁依赖、为什么重载」 | 依赖链 |
| `ada doctor` | 环境自检（版本、路径、并发、权限、缓存） | 检查清单 |
| `ada clean` | 清编译缓存 / 失效 Artifact | — |

`ada inspect --json` 是给面板和 CI 用的稳定契约。

### 14.3 mygo 桌面宿主集成

内核是 headless 的，mygo 只做「窗口 + UI 渲染 + 系统能力」。两者在 `platform/host/desktop` 一个适配器里会合：

```go
func main() {
    app := ada.New(ada.Options{Home: home})
    kernel := app.Kernel()

    // 1) 内核在后台跑，UI 不等它
    go func() { _ = kernel.Start(context.Background()) }()

    // 2) 插件静态资源：自定义协议，零拷贝、可流式（mygo.Protocol.HandleFunc 原生支持）
    _ = mygo.Protocol.HandleFunc("ada-plugin", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !kernel.CanServe(r.URL.Path) { http.NotFound(w, r); return }
        kernel.ServeAsset(w, r)                       // 只服务已装插件目录
    }))

    // 3) 页面调用插件：类型化绑定，只给可信页面；方法内部再走内核权限
    mygo.Bind(adaService{kernel})

    // 4) 贡献变化 → mygo 类型化事件 → UI
    contribChanged := mygo.NewEvent[ada.ContribEvent]("ada:contrib")
    kernel.OnContribChanged(func(ev ada.ContribEvent) {
        if err := contribChanged.Broadcast(ev); err != nil { /* log */ }
    })

    // 5) 需要直接操作原生 UI 时，切回主线程（阻塞式，勿在热路径调用）
    mygo.RunOnMain(func() { panel.Refresh(kernel.Snapshot()) })

    mygo.NewWindow(mygo.WindowOptions{URL: "/"})
    mygo.Run()
}
```

规则：

| 关注点 | 做法 | 理由 |
|---|---|---|
| 线程 | 内核 goroutine 与 UI 主线程分离；UI 变更走 `mygo.RunOnMain`（阻塞）或 `mygo.NewEvent[T].Broadcast`（异步） | mygo 的 UI 只能在主线程改（C6） |
| 插件页面 | `contributes.pages` 由宿主渲染成 mygo native UI（list/detail/form 组件），或在前端路由里挂 `slots` | v1 已验证：插件不画 UI，宿主决定展示 |
| 插件窗口 | 只有显式 `ui.openWindow` 能力才允许；默认「面板」形态 | 防止插件乱开窗 |
| 事件 | 内核事件 → mygo typed event（`mygo.NewEvent[T]`）→ 前端订阅 | 类型化 IPC，避免字符串协议 |
| 大对象 | 图片/音频走 `ada-plugin://` URL 或字节流，不进 JSON | 数据面留在 Go（v1 核心经验） |
| 开发 | `mygo dev` 跑前端热更；`ada dev` 跑插件热更；两者独立 | 关注点分离 |
| 打包 | 内置插件 Artifact 用 `embed` 随二进制；第三方装到 `PathUserData/plugins` | 单二进制 + 可选扩展 |
| 退出 | `mygo` 退出前 `kernel.Stop(10s)`，逆序回收 | 防止悬挂 goroutine/句柄 |

**与 mygo 编译期插件（`mygo.Use`）的关系**：继续保留。mygo 的 `mygo.Plugin` 是「Go 服务 + npm 包」的**受信编译期能力**（fetch/sqlite/terminal），内核插件是**可安装的运行期扩展**。两者不互相替代：前者给页面用，后者给整个应用（含 headless）用。内核不依赖 mygo，桌面适配器才依赖。

### 14.4 测试策略

| 层次 | 方法 | 门禁 |
|---|---|---|
| 内核单测 | Context/Service/Effect/Event/Config 纯逻辑，无 moejs | 覆盖率 ≥ 80%，竞态 `-race` 全绿 |
| 编译层 | golden Artifact（输入 TS → 期望 JS/map 快照） | 冻结产物，diff 即失败 |
| 运行时契约 | moejs 真实 Runtime + 12 个 fixture 插件（正常/抛错/卡死/超时/OOM/CJS/缺模块/循环依赖…） | 每类错误码必须命中 |
| 编排 | `Plan`/`Report` 黄金文件；随机拓扑（20–200 节点）模糊测试 | 无环、顺序稳定、失败可回滚 |
| 安全 | 拒绝矩阵（能力 × 目标 × 信任），路径穿越/符号链接/重定向 host 用例 | 全部拒绝且有审计 |
| 性能 | 基准：装配 50 插件、调用 p99、热重载 100 次、内存增长 | 回归阈值 ±15% |
| 端到端 | mygo 桌面 + 3 个真实插件（fs、cron、provider） | 手工/自动化脚本 |

`plugintest` 包给插件作者：

```go
func TestReadFile(t *testing.T) {
    h := plugintest.New(t, plugintest.WithCaps("fs.read"))
    h.FS().Write("a.txt", "hello")
    h.Load("./")                                  // 编译 + 装配 + apply
    res, err := h.Tool("read_file").Call(ctx, `{"path":"a.txt"}`)
    ...
}
```

---

## 15. 性能与体积预算

### 15.1 实测基线（本机 Go 1.27.1 / windows/amd64，`CGO_ENABLED=0`）

| 二进制 | 未 strip | strip |
|---|---|---|
| 空程序（hello） | 1.85 MB | 1.22 MB |
| + `moejs`（Compile+Runtime+Call 冒烟） | **12.32 MB**（+10.5） | **8.63 MB**（+7.4） |
| + `esbuild/pkg/api`（Transform 冒烟） | **11.65 MB**（+9.8） | **8.33 MB**（+7.1） |
| 三者同进程（推算） | ≈ 22 MB | ≈ 16 MB |

> 结论：**moejs 是刚需，esbuild 不是**。发布物只带 moejs；esbuild 只进 CLI/开发构建（`-tags adaesbuild` 才进桌面应用）。

| 运行时开销 | 数值 | 来源 |
|---|---|---|
| 新建 Runtime | 1.39 µs | moejs 基准 |
| 新建 + 加载最大插件 | 71 µs | 同上 |
| 单次跨边界调用（含参数/结果转换） | 6.86 µs | 同上 |
| 每 Runtime 常驻 | 80.5 KiB | 同上 |
| 编译最大插件（进程内一次） | 2.2 ms | 同上 |
| `Transform` 单文件 | 0.77–0.88 ms | 本机实测 |
| 20 文件插件全量编译 | 15–20 ms | 推算 |

### 15.2 目标预算

| 场景 | 目标 | 依据 |
|---|---|---|
| 冷启动（50 个 JS 插件，Artifact 已缓存） | ≤ 150 ms 到最后一个 fiber active | 71 µs × 50 ≈ 3.6 ms + apply 逻辑 + 服务注册 |
| 单插件首次激活（延迟加载） | ≤ 5 ms | 建 Runtime + Load + apply |
| 一次工具调用额外开销（不含业务） | ≤ 20 µs p50 | moejs 6.86 µs + 服务/预算/审计 |
| 热重载一个插件 | ≤ 100 ms | 编译缓存命中 + 新 Runtime + 新旧交替 |
| 内存（100 个活跃 JS 插件） | ≤ 120 MB | 80.5 KiB × 100 ≈ 8 MB 引擎 + 插件自身堆 |
| 装配 200 entry 的 Plan 计算 | ≤ 20 ms | 纯图算法 |

### 15.3 优化手段（按收益排序）

1. **懒激活**（默认 `onService`）：启动只跑必需的 5–10 个插件。
2. **Artifact 缓存 + 内容哈希**：跨进程命中，避免重复编译。
3. **`*Module` 进程内共享**：同一插件多个 Runtime 只编译一次（池化插件受益最大）。
4. **JSON 边界最小化**：大对象走句柄/URL；工具参数直接 `json.RawMessage` 透传，不二次解码。
5. **数据面留在 Go**：图片/音频/流式增量不跨边界（v1 已验证这是最大收益点）。
6. **idle 回收**：长尾插件回到 cold，释放 Runtime 与堆。
7. **批量编译并发**：`Transform` 并发安全（实测 800 次 0 错误）。
8. **不做的事**：不为「省 6 µs」把服务搬进 JS；不把字节码落盘（moejs 不支持）。

---
## 16. 目录结构与代码骨架

### 16.1 仓库布局

```
ada_go_v2/
  go.mod                      module github.com/issueye/ada_go_v2
  platform/kernel/                     ← 纯 Go，不依赖 moejs / mygo
    app.go                    App：根 Context、代际、生命周期
    context.go                Context 派生（Extend/Isolate/Intercept）
    fiber.go                  Fiber 状态机、epoch、Ready/Update/Reload/Dispose
    scope.go                  作用域与 effect 栈（逆序回收）
    service.go                服务表、Impl、单写者、dependents
    event.go                  五种分发 + 监听器排序/超时
    config.go                 Config 快照、Decode、volatile
    schema/                   JSON Schema 子集（校验 + 默认值 + simplify）
    errors.go                 错误码与类型
  platform/plugin/                     清单、发现、Artifact、安装、信任、签名
  ../ada_jsrt/                        同级独立库(github.com/issueye/ada_jsrt)
    compile/                  esbuild Transform 管线、模块表、缓存、sourcemap
    jsrt/                     moejs 适配：Entry/Runtime/Loop/Loader/Bridge/Budget/Breaker
      api/                    宿主模块实现（ada:fs / ada:exec / ada:clock …）
  platform/caps/                       能力授权与沙箱（fs/net/exec/proc/secrets/ui/config/audit）
  contrib/                    贡献目录（tools/providers/prompts/actions/routes/pages/slots/jobs）
  platform/assemble/                   Plan/Graph/Assembler/Report/Generation
  platform/flow/                       流水线：DSL、执行、检查点、受限表达式
  platform/sched/                      激活、配额、闲置回收、cron
  profile/                    bundle/patch 分层与合并
  observe/                    日志、span、指标、健康、inspect
  platform/host/
    desktop/                  mygo 适配器（窗口、协议、绑定、面板）
    cli/                      headless：up/dev/install
    server/                   HTTP 适配器（可选）
  plugins/                    基座插件（Go 或 TS，均走 kernel.Plugin）
    seam/                     llm/tools/prompts/fs/shell/sandbox/storage/settings/
                              secrets/clock/commands/jobs/schedule/telemetry/ui
    provider/                 llm-openai · llm-anthropic · llm-replay · fs-local ·
                              shell-local · sandbox-none · storage-json/sqlite
    policy/                   llm-retry · llm-token-cap · fs-sandbox · tool-timeout ·
                              tool-repeat-guard · approval-gate · context-budget
    core/                     agent · agent-loop · session · system-prompt ·
                              context-assembly · compaction-* · approval ·
                              user-questions · subagent · goal · plan · team · workflow
    tool/                     fs · fs-search · shell · web · todo · skill · office · job
  presets/                    base / coder / designer / headless（JSON，见 §12.6）
  sdk/
    ts/                       @ada/sdk：类型声明 + 辅助函数（无运行时代码）
    dts/                      由 Go 生成（单一事实来源）
  cmd/ada/                    CLI
  testdata/                   fixtures / golden / chaos
```

依赖禁令（CI 用 `go list` 检查）：

```
platform/kernel   ✗→ ada_jsrt/jsrt, ada_jsrt/compile, platform/assemble, platform/flow, platform/sched, platform/host/*, plugins/*
                  ✗→ 出现 agent 词汇（Turn/Message/ToolCall/Model/Session）
ada_jsrt/compile  ✗→ ada_jsrt/jsrt, platform/assemble
ada_jsrt/jsrt     ✗→ platform/*（独立库只依赖标准库、moejs 与注入的 Host 能力）
platform/assemble ✓→ platform/kernel, platform/plugin, platform/caps
plugins/*         ✓→ platform/kernel, platform/caps（seam 插件之间只经服务与事件）
platform/host/*   ✓→ 全部
```

> 第三条禁令（`kernel` 里不得出现 agent 词汇）是 §12.6 的守护线：一旦 `kernel` 需要认识「turn」，就说明这项能力该做成 core 插件而不是内核机制。

### 16.2 骨架（把设计落成可编译的形状）

```go
// platform/kernel/app.go
type App struct {
    opts    Options
    root    *Context
    svc     *ServiceTable
    events  *EventHub
    fibers  sync.Map          // id → *Fiber
    gen     atomic.Uint64
    observe *observe.Hub
}

func New(opts Options) *App
func (a *App) Start(ctx context.Context) error          // 归零：建根作用域、装内置服务
func (a *App) Load(ctx context.Context, entries []Entry) (*Assembly, error)
func (a *App) Fiber(id string) (*Fiber, bool)
func (a *App) Ensure(ctx context.Context, id string) (*Fiber, error)  // onDemand 激活
func (a *App) Inspect() InspectSnapshot
func (a *App) Stop(ctx context.Context) error
```

```go
// platform/assemble/assembler.go
type Assembler struct {
    app  *kernel.App
    disc plugin.Discoverer
    comp compile.Compiler
    prov kernel.Provider        // jsrt.EntryProvider 实现它；内核不知道 moejs 的存在
}

func (a *Assembler) Plan(in PlanInput) (*Plan, error)
func (a *Assembler) Apply(ctx context.Context, in PlanInput) (*kernel.Assembly, error)
func (a *Assembler) Add(ctx context.Context, e profile.Entry) error    // 增量安装
func (a *Assembler) Remove(ctx context.Context, id string) error
```

```go
// ada_jsrt/jsrt/entry.go —— TS 插件实现 kernel.Plugin
type EntryPlugin struct {
    art  *plugin.Artifact
    json Manifest
}

func (p *EntryPlugin) Name() string          { return p.json.ID }
func (p *EntryPlugin) Inject() kernel.Inject { return p.json.Inject }
func (p *EntryPlugin) Provide() []string     { return p.json.ProvideNames() }
func (p *EntryPlugin) Config() *schema.Schema { return p.json.ConfigSchema() }

func (p *EntryPlugin) Apply(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
    e := newEntry(p)
    if err := e.open(ctx, cfg); err != nil { return nil, err }
    d, err := e.callApply(ctx, cfg)           // 调 JS apply(ctx, config)
    if err != nil { e.close(); return nil, err }
    return func(c context.Context) error { return e.close() }, nil
}
```

```ts
// sdk/ts/index.d.ts（节选，由 Go 生成保证与实现一致）
export interface Context {
  readonly id: string
  readonly config: Config
  provide(name: string, impl: unknown, version?: number): () => Promise<void>
  service<T = unknown>(name: string): T
  tryService<T = unknown>(name: string): T | undefined
  effect(label: string, fn: () => Disposer | Promise<Disposer>): Disposer
  on(name: string, handler: (ev: any) => void | Promise<void>): Disposer
  once(name: string, handler: (ev: any) => void | Promise<void>): Disposer
  emit(name: string, ...args: unknown[]): void
  waterfall<T>(name: string, input: T, ...args: unknown[]): Promise<T>
  serial(name: string, ...args: unknown[]): Promise<void>
  bail<T>(name: string, ...args: unknown[]): Promise<T | undefined>
  inject(names: string[], fn: (ctx: Context) => void): Disposer
  log: Logger
  traceId: string
}
```

---

## 17. 里程碑与验收

| 里程碑 | 内容 | 交付物 | 验收（DoD） | 估算 |
|---|---|---|---|---|
| **M0 验证周** | 跑通 12 个 spike（附录 D；S1/S2/S6/S8/S9 已预验），冻结内核 API 草案 | spike 报告 + 一页 API 摘要 | 每个 spike 有明确通过/失败结论；失败项有替代方案 | 4–5 天（**gate 已关闭**） |
| **M1 内核骨架** | Context/Fiber/Scope/Effect/Service/Event/Config + 单测 | `platform/kernel/` 可独立跑 | 20 个 Go 插件按拓扑序装配；effect 逆序回收无泄漏；`-race` 全绿；`kernel` 无 agent 词汇（CI 检查） | 2 周 |
| **M2 执行层** | compile + jsrt + bridge + 宿主模块 | TS 插件能跑 | 12 类 fixture（正常/抛错/超时/OOM/卡死/CJS/缺模块/循环/装饰器/enum/TSX/多文件）全部按预期 | 3 周 |
| **M3 装配与配置** | Plan/Graph/Assembler/Report/Profile | `ada plan/up/install` | 装/卸/改配置不重建整代；失败保留旧代；热重载级联正确 | 2 周 |
| **M4 编排** | flow / sched / 配额 / cron | 一条跨插件流水线示例 | 崩溃后从检查点恢复；闲置回收后再激活成功；配额拒绝有审计 | 3 周 |
| **M4.5 Agent Core 插件化** | seam/provider/policy 三段式 + `core/agent`·`agent-loop`·`session`·`system-prompt` + `presets/base`（任务拆分见 [seam-api.md 附录 C](seam-api.md)） | 能跑通一次「用户消息 → 模型 → 工具 → 结果」的最小 agent | 删掉任一插件产品仍能装配；seam 无 provider 时返回 `EUNIMPLEMENTED`；换成 `headless` preset 行为一致 | 3 周 |
| **M5 安全与宿主** | caps 强制点、信任/签名、CLI、mygo 适配器、inspect；fs/sandbox/shell 契约见 [seam-api-security.md](seam-api-security.md) | 桌面 demo + CLI | 拒绝矩阵全绿；安装签名校验；`ada inspect` 展示 fiber/服务/effect 树与装饰器链 | 3 周 |

本路线图**不含 v1 插件迁移**,不提供 `ada migrate`;v1 仅作为行为参照保留在 §11。
累计约 **15–17 周**（单人全职，含验证周）；若两人并行（内核/执行 vs 编排/agent core）可压到 12–14 周。

**M0 的 gate 已关闭（2026-11-19）**：S2（1000 次 Runtime 重载，goroutine 0 泄漏、堆增量 358 KB 且不随轮次线性增长、单次 create+load+call+stop 平均 11.4 µs）与 S8（20 文件插件全量编译 13.3 ms；改 1 个文件的增量 transform+compile+link+load+call 平均 0.66 ms）均通过，无需启用备选方案。

---

## 18. 风险与对策

| # | 风险 | 影响 | 对策 |
|---|---|---|---|
| R1 | moejs 仍是 alpha，API 可能变 | 执行层返工 | 版本钉死（`v0.1.0-alpha.11`）；`jsrt` 单点适配；对 moejs 行为写契约测试，升级时跑全量 |
| R2 | 字节码不可落盘（无序列化 API） | 冷启动只能靠 Artifact + 懒激活 | 已按此设计；不承诺「毫秒级冷启动」，用预算管理预期 |
| R3 | moejs 无 sourcemap | 诊断体验差 | 自建 `lineMap` 重映射；golden 测试覆盖 50 个错误位置 |
| R4 | 一个 Runtime 只能 Load 一个模块 | 热重载必须新建 Runtime | 重载 = 新 Runtime + 服务交接；状态走 `store` |
| R5 | JS 服务跨边界只能 JSON | 服务调用有固定开销（~7 µs + 序列化） | 同 fiber 走 loop 直调；批量接口；禁止把热路径放进 JS |
| R6 | Go 没有属性代理 | Cordis 的 `ctx.x` 语法糖不可得 | 显式 `Service[T]` + 生成的 TS 类型；用 `ada.Service("name")` 统一写法 |
| R7 | esbuild 版本 <1.0，minor 可能破坏 | 编译产物漂移 | 钉版本；golden Artifact 冻结；升级即跑全量编译测试 |
| R8 | esbuild 体积 +7 MB（strip） | 桌面安装包变大 | 默认不进发布物；`adaesbuild` 标签按需开启 |
| R9 | 循环依赖的 ESM 时序（TDZ） | 链接/调用异常 | 已预验可链接可调用（S9）；保留 TDZ 回归用例，编译期仍禁止「类型专用环」 |
| R10 | Windows 路径/符号链接沙箱差异 | 越权读文件 | 三平台测试矩阵；`EvalSymlinks` 逐段校验；`os.Root` 为主 |
| R11 | 重载丢失内存状态 | 用户可感知的中断 | 契约写死：状态必须进 `store`；提供 `internal/reload` 事件供交接；UI 显示「重载中」 |
| R12 | M4.5 agent 未闭环 | 平台没有真实产品验证，seam 形状可能返工 | 先按 C1–C6 做最小 agent 与 golden；不以更多 platform/host/安全插件替代闭环 |
| R13 | 文档与代码漂移（v1 的老问题） | 信任受损 | 清单 schema 与 d.ts 由 Go 结构生成；文档示例进 CI 编译；`ada check` 校验示例 |
| R14 | loop goroutine panic 导致进程退出 | 稳定性 | `eventloop.Options.OnError` + 外层 `recover`；panic 视为该插件致命并熔断 |
| R15 | 预算/熔断参数拍脑袋 | 误杀或漏杀 | 先用 v1 实测参数（budget.go/breaker.go），上线后按 p99 调 |
| R16 | 内核 API 过早冻结 | 后续改动成本 | M0 后冻结「插件作者可见面」，内部接口允许演进 |

---

## 附录 A：Cordis → ada kernel 全表

| Cordis（`@deepseek-ai/cordis@4.1.0`） | ada kernel | 说明 |
|---|---|---|
| `ctx`（Proxy） | `*kernel.Context`（显式值） | 服务用 `Service[T](ctx, name)` 取 |
| `ctx.extend(meta)` | `ctx.Extend(Vars)` | 不可变派生 |
| `ctx.isolate(name, label?)` | `ctx.Isolate(name, label)` | 标签指针即身份 |
| `ctx.intercept(name, cfg)` | `ctx.Intercept(name, ServiceConfig)` | 逐祖先合并 |
| `ctx.plugin(p, cfg)` → `Fiber & PromiseLike` | `ctx.Plugin(p, cfg)` → `*Fiber` | `Ready(ctx)` 等激活 |
| `fiber.dispose()` | `fiber.Dispose(ctx)` | 可取消、有超时 |
| `fiber.update(cfg)` | `fiber.Update(cfg)` | 校验先于提交 |
| `ctx.provide(name, v)` | `ctx.Provide(name, v, opts)` | 返回 disposer，等 dependents |
| `ctx.set(name, v)` | `ctx.Set(name, v)` | 仅 provider 可写 |
| `ctx.get(name, strict)` | `Service` / `TryService` | 可见性由 inject 决定 |
| `inject: ['a']` | `Inject{Names: []string{"a"}}` | 未满足 → pending |
| `inject: {a: cfg}` | `Inject{Config: {...}}` | 服务专属配置 |
| `ctx.inject(['a'], cb)` | `ctx.Inject([]string{"a"}, fn)` | 条件挂载 |
| `ctx.effect(fn, label)` | `ctx.Effect(label, fn)` | 逆序、可嵌套、幂等 |
| `ctx.on/once` | `ctx.On/Once` + `OnOption` | 返回 disposer |
| `emit/parallel/serial/bail/waterfall` | 同名五个方法 | 见 §7.2 |
| `ctx.logger(name)` | `ctx.Log()`（命名空间自动） | `slog` 输出 |
| `Config = Schema.object(...)` | JSON Schema 子集 | 两侧共用 |
| `fiber.config / update` | `ctx.Config()` / `fiber.Update` | volatile 字段原地更新 |
| loader `Entry` / `EntryGroup` | `profile.Entry` / `bundle` | id + patch 叠层 |
| `disabled` / `!!js` | `disabled` + 受限表达式 | 拒绝任意 JS |
| HMR | `fiber.Reload(reason)` | 新 Runtime，非进程内替换 |
| `ctx.registry` 内省 | `App.Inspect()` + `ada inspect` | fiber/服务/effect 树 |

---

## 附录 B：错误码

| 码 | 含义 | 典型来源 |
|---|---|---|
| `EPLUGINFAULT` | 插件自身抛错 | JS exception / Go error |
| `EPLUGINVIOLATION` | 越权（能力拒绝） | `caps.Broker` |
| `ESERVICE` | 服务缺失/版本不符 | `ctx.Service` |
| `ESERVICEINUSE` | 服务仍有依赖者 | dispose 超时 |
| `EEFFECT` | effect 注册失败/已卸载后注册 | scope |
| `ECONFIG` | 配置校验失败 | schema |
| `EMANIFEST` | 清单非法 | platform/install/preflight |
| `ESIGN` | 签名缺失/无效 | install |
| `EARTIFACT` | Artifact 缺失/摘要不符 | load |
| `ECOMPILE` | 编译失败（语法/CJS/.node） | compile |
| `ELINK` | 模块链接失败（缺模块） | moejs `Link` |
| `ETIMEOUT` | 调用超时 | budget |
| `EMEMLIMIT` | 内存超限 | moejs |
| `EINTERRUPT` | 被取消 | signal |
| `EQUOTA` | 配额用尽 | sched |
| `EBUSY` | 队列满 | runtime |
| `EBREAKER` | 熔断中 | breaker |
| `EDEP` | 依赖未满足（pending 原因） | assemble |
| `ECYCLE` | 依赖成环 | assemble |
| `EFLOW` | 流水线步骤失败 | flow |
| `EUNIMPLEMENTED` | seam 已装载但无 provider | seam 调用 |

---

## 附录 C：v1 → v2 迁移映射

| v1 | v2 | 处理 |
|---|---|---|
| `pluginapi.Manifest`（ID/Name/Version/APIVersion/Trust/Sig/Backend/Frontend/Settings） | `plugin.Manifest` | 拍平 backend/frontend；`entry` 取代 `backend.entry`；新增 inject/provide/runtime/activation |
| `backend.capabilities` / `frontend.capabilities` | `capabilities` | 统一；恢复调用点强制 |
| `backend.hooks`（14 点位 + 动作等级） | `contributes.events` + 事件分发模式 | 见 §7.5 |
| `frontend.slots/pages/blocks/nav/dashboard/actions/styles/assets` | `contributes.pages/slots/actions` | 渲染契约保留（Page/Section/Item） |
| `host.Context`（每调用回调结构体） | 服务 + `CallContext` | 成员拆成服务：`ctx.fs` → `fs` 服务、`ctx.exec` → `exec` 服务、`ctx.state` → `store` |
| `agent.registerTool/Provider/Prompt/Action/Route/Timer/Hook` | `contrib/*.Register` | 全部返回 disposer |
| `platform/host/staging.go`（Targets.commit/undo） | `assemble` 的事务 + effect 栈 | 原子提交与回滚保留，粒度到贡献 |
| `platform/host/runtime.go`（Load + frozen + 双代重载） | `jsrt.Entry` + `fiber.Reload` | 细粒度重载，双代保留为「整代回退」兜底 |
| `platform/host/load.go`（Virtual module + Link/Resolver） | `compile` + `jsrt.Loader` | 结构照搬，输入从 `.ts` 源码改为 Artifact |
| `plugins/official/build.mjs`（Node 类型擦除） | `compile`（esbuild Go） | 去掉 Node 依赖 |
| `platform/host/budget.go` / `breaker.go` / `limits.go` | `jsrt.Budget/Breaker` | 参数沿用 |
| `platform/host/cap_*.go` | `platform/caps/*` | 沙箱逻辑与限额照搬，加授权门禁与审计 |
| `pluginapi/signature.go`（签目录） | `plugin.Sign`（签 Artifact digest） | 签名对象改为实际执行物 |
| `app/app.go` 的组合根（七个 registry 硬编码） | `assemble` + 服务表 | 组合根只装基座服务 |
| `cli/session_assembly.go`（整代重建 + 排空 30s） | `assemble.Apply/Add/Remove` | 增量装配 |
| `platform/host/skills.go`（扫描 SKILL.md） | 保留为 `contrib` 的一个 kind | 触发时机改为 lazy |

---

## 附录 D：M0 必做的 12 个 spike（带通过标准）

| # | 验证 | 通过标准 | 失败备选 |
|---|---|---|---|
| S1 | esbuild Transform（TS/TSX/enum/装饰器/参数属性）→ moejs `Compile`+`Link` 多模块 | 5 种语法全部跑通；`plugin://` 相对解析正确 | 改用 esbuild `Build` 打成单文件 |
| S2 | 热重载 100 次（新 Runtime 替换旧） | 内存回落到基线 ±5%；无 goroutine 泄漏；服务不中断 | 退化为整代重建 |
| S3 | Go + JS 监听器混跑 waterfall（含取消/超时） | 顺序稳定；短路生效；超时按策略处理 | 只允许同类监听器混跑 |
| S4 | 服务 epoch 级联（20 插件 A→B→C 链） | 提供者重载后消费者按拓扑序重载一次，无死锁 | 退化为「消费者不自动重载」 |
| S5 | 内存上限语义 | 超限报 `EMEMLIMIT`，脚本不可 catch；`Stats` 可用 | 叠加宿主层 watchdog |
| S6 | `pool: 4` 共享同一 `*Module` 并发调用 | 无 `ErrForeign`；吞吐线性 | 只支持 pool=1 |
| S7 | sourcemap 反查 | 错误行号与原 TS 行一致率 ≥ 95% | 显示「编译后行号 + 源码片段」 |
| S8 | 文件监听 + 增量编译 | 改 1 个文件到重载完成 ≤ 200 ms | 全量重编（仍 ≤ 2 s） |
| S9 | ESM 循环依赖 | `Link` 行为符合 ESM 语义，或能被静态拒绝 | 编译期禁止循环 |
| S10 | 闲置回收后再激活 | 5 次循环后无泄漏；贡献恢复正确 | 只回收无贡献插件 |
| S11 | 拒绝矩阵 + 审计 | 20 个越权用例全部拒绝且落审计 | 缩小能力面 |
| S12 | Windows 路径沙箱 | `..`/符号链接/大小写/UNC 全部拦住 | 收紧为「仅工作区相对路径」 |

> **已预验（2026-11-19，临时 spike 工程）**：S1（enum + 构造器参数属性 + TS 注释 + 两模块相对导入，`Link` 后调用返回正确）、S2（1000 次重载无 goroutine 泄漏、堆增量 358 KB 不随轮次增长）、S6（12 个 Runtime 并发加载同一 `*Module`、每 Runtime 200 次调用，0 错误）、S8（20 文件全量 13.3 ms；单文件增量 0.66 ms）、S9（双向 import 链接并调用成功）。S1 的通过意味着「esbuild 补 moejs 的 TS 运行时语义」这条核心假设成立；S2/S8 通过意味着 M0 gate 关闭，可以进入 M1。

---

## 附录 E：待决问题（评审时定）

1. **内核 API 冻结范围**：插件作者可见面（ctx/清单/事件）冻结，内部接口（Assembler/Provider）允许演进——是否接受？
2. **流水线（`flow`）是否属于 v2.0**：它是「编排」最直观的产物，但也会推迟 M4；建议保留但允许 M4 后期降级为「Go API only」。
3. **`ada:api` 的 d.ts 生成方式**：从 Go 结构生成（单一事实来源）vs 手写（灵活）。建议生成 + 手写补充注释。
4. **池化插件的静态检查强度**：编译器扫描「模块级可变状态」不可靠，是否需要作者显式声明 `stateless: true` 并承担后果。
5. **受限表达式求值器**：自研（安全、弱）vs moejs 沙箱（强、需额外加固）。建议默认自研，`expr.lang: "js"` 显式开启。
6. **能力授权模型**：桌面端「按插件 + 按能力」的同意 UI 粒度；CLI 安装是否需要逐能力 flag。
7. **`moejs` 升级策略**：锁 alpha.11 直到对方发布稳定版，还是允许 minor 跟进（需要执行层契约测试兜底）。





