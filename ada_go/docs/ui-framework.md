# ui-framework.md — Ada UI：无 Node 的插件化 SPA 前端框架

> 状态：**提案（RFC）**，尚未落地。评审通过后按 §16 的 U0–U3 推进；清单扩展需同步主设计 §9，能力点需同步 §13 与 `caps.md`。
>
> 关联：[plugin-kernel-design.md](plugin-kernel-design.md)（§2 原则、§9 清单、§13 安全、§14 宿主）、[host-server.md](host-server.md)、[host-desktop.md](host-desktop.md)、[install.md](install.md)、[caps.md](caps.md)、[seam-approval.md](seam-approval.md)、[ADR 0005](adr/0005-ui-infrastructure.md)、[ADR 0006](adr/0006-custom-ui-components.md)、[ADR 0007](adr/0007-host-web-budget.md)、[ADR 0008](adr/0008-desktop-webview-shell.md)、AGENTS.md。

## 0. 结论摘要

**一句话**：用 Go 编译组件/组件包，浏览器只装不编；SPA 由插件拼出来，插件可在运行时从远程装入，风险被签名与信任等级挡住。

| # | 决策 | 替代方案 | 理由 |
|---|---|---|---|
| D1 | **模板与组件在 Go 里编译**，浏览器不做编译 | Vue/Svelte 的 Node 构建；浏览器内运行时编译 | 无 Node；无 `eval`/`new Function`；产物可 golden、可签名、可复现 |
| D2 | 编译产物是**可序列化的 RenderPlan（JSON AST）+ ESM 模块 + 作用域 CSS** | 编译成 JS 渲染函数（Svelte 式） | Plan 是数据，Go 可直接生成/审计/缓存；生成 JS 渲染函数需要另一套 JS 编译器，且与无 Node 目标冲突 |
| D3 | 运行时用**细粒度响应式 + Plan 解释器**直接操作 DOM，不做虚拟 DOM | 虚拟 DOM diff；整树重渲染 | 无编译也能精确更新；包体小；性能可预算 |
| D4 | **一项功能 = 一个组件或一个组件包**：单组件是退化情形，多组件包是常态 | `.vue` 模板 + 独立 `.ts` + `.css` + 构建配置；一个插件塞多个功能 | 交付粒度与功能边界一致；无构建配置；评审时能看清「一个功能到底包含什么」 |
| D5 | 资产**同源供给 + Go 验签**，SPA 只消费已验证产物 | 浏览器直接 `import` 远程 URL；每个插件单独打包 | CSP 可收紧到 `'self'`；信任逻辑只写一遍；可审计、可离线 |
| D6 | **同域运行 = 完全可信**；未签名第三方只能声明式页面或隔离 iframe | 在 JS 里做沙箱/能力检查 | 同域 JS 能读 DOM 与 CSRF token，JS 侧门禁是装饰；真边界是信任等级 |
| D7 | 运行时内核（装载/渲染/路由/服务/作用域）**不是插件**；Shell、页面、插槽、主题全是插件 | 把路由/Shell 也插件化 | 内核只懂 UI 机制、不懂产品；避免「没有它起不来」的插件 |
| D8 | **声明式页面优先**，自定义组件只给 `verified` 及以上 | 所有插件自由画 UI | 保留 AGENTS #12 的主路线（插件产数据）；自定义是显式反转，见 [ADR 0006](adr/0006-custom-ui-components.md) |
| D9 | 产品差异只来自 **preset 的 `ui` 段**（shell/home/theme/locale） | 代码里判断产品 | P4；换 preset 即换产品 |
| D10 | 运行时测试用 **Go + moejs + 操作序列适配器（op trace）**，真实 DOM 只做少量浏览器冒烟 | Vitest/Playwright（需要 Node） | 渲染核心输出确定性操作日志，CI 只跑 Go；DOM 适配器同时服务浏览器与测试 |

**不做什么**：SSR/水合、Node 工具链、浏览器内 TS/SFC 编译、任意 npm 生命周期脚本、PWA/Service Worker（U3 之后再议）、iframe 隔离（U2 原型，不阻塞主线）。

---

## 1. 约束翻译与归属

### 1.1 需求逐条翻译

| 需求 | 不是 | 是 | 验收 |
|---|---|---|---|
| 不要 Node 环境 | 不是「浏览器里跑 Node」 | 开发、构建、测试、运行都不需要 `node`/`npm`/`npx`；只有 Go 与浏览器 | 干净机器（无 Node）能 `go test ./...` 并通过；仓库脚本 grep 不到 npm 调用 |
| SPA | 不是多页应用、不是 SSR | 一个 HTML 入口、客户端路由、深链回退、无服务端渲染 | 任意非资产路径返回同一 `index.html`；前进/后退/刷新保持路由 |
| 插件化 | 不是「一个框架加一堆扩展」 | 运行时内核之上，页面、插槽、命令、主题、Shell 都是可撤销注册的插件 | 禁用任一 UI 插件，SPA 仍能启动并显示 shell 降级页 |
| 可以远程加载 | 不是「只能随二进制发布」 | 运行时安装/更新插件，不重启、不重新打包；产物签名、同源缓存 | 安装一个远程插件后 10 秒内出现在导航；断网时已加载页面仍可交互（冷启动离线不在本条） |
| 组件/组件包即功能 | 不是「一个插件装很多功能」 | 一项功能 = 一个**组件**（单个 `.aui`）或一个**组件包**（多个 `.aui` + 资源，统一编译/签名/安装）；包内组件是私有实现单元 | 新增「文件树」功能：`plugin.json` + 一个 `ui/` 包；单组件功能只有 `plugin.json` + 一个 `.aui` |

**零 Node 的口径**：构建、插件编译/执行、Web 宿主都不需要 Node。桌面若选 Electron，Node 只在桌面壳（main/preload）里，renderer 不启用 Node；要求绝对零 Node 时选 mygo WebView2（§11.3）。

### 1.2 归属（按 AGENTS §3 决策树）

| 部分 | 归属 | 判据 |
|---|---|---|
| `ui/`（Go：解析、编译、产物、校验） | **L2 基础设施**（不是插件） | 是「装载/编译/执行」机制，没有产品语义；等价于 `compile` 的前端分支 |
| `ui/runtime/`（浏览器 JS：内核） | **L2 基础设施**（前端半边） | 只认组件、信号、路由、作用域，不认识 agent/产品；可被另一实现整体替换 |
| `plugins/ui/*`（Shell、设置、检查器、页面） | **UI 功能包**（一个包一项功能） | 删掉它 SPA 仍能启动；贡献可撤销 |
| `host/web`（Go HTTP/SSE/深链/动作桥） | **宿主适配器** | 只做协议、同源资产、认证映射，不做业务 |
| 远程仓库、签名、安装 | 复用 L2 `plugin`/`install`/`caps` | 已有机制，UI 只是新增一类产物 |

**为什么不是第五类**：前端运行时做的是「装载组件、执行渲染、派发路由」，与 `assemble`/`jsrt` 同性质；它不读业务配置、不判断产品。真正有业务语义的 Shell、页面、设置都在 `plugins/ui/*`。把 `ui/` 纳入 L2 的边界与回滚方式见 [ADR 0005](adr/0005-ui-infrastructure.md)；评审通过后同步 AGENTS §2.2/§5.3/§5.4。

**三个名字不要混**：`plugins/seam/ui` 是 Go 侧「通知/确认/打开窗口」的能力 seam（主设计 §3.1、§13.3）；`ui/` 是前端编译器与运行时资产；`ada:surface` 是浏览器运行时模块名。前者是插件，后两者是基础设施；目录与文档必须写全路径。

**术语**：**插件**是安装/签名/后端的单位；**功能包**是一项功能的 UI 交付单位（一个插件最多一个）；**组件**是功能包内的实现单元（私有，不单独安装）。所以「一项功能 = 一个组件（单组件包）或一个组件包」。

### 1.3 非目标

1. 不做 SSR、水合、SEO 预渲染；SPA 是明确前提。
2. 不做浏览器内编译器；`eval`/`new Function` 一律禁止（AGENTS §5.6）。
3. 不做「任意 npm 包直接可跑」；依赖必须由 Go 在安装期解析并内联（§5.4）。
4. 不做插件自绘全局界面（导航、窗口 chrome、全局弹层）；自定义组件只画自己的内容区。
5. 不做 Node 生态工具（ESLint/Prettier/Vitest/Playwright）；质量门禁走 Go。
6. U1 不做 iframe 隔离与离线 SW；先保证可信路径端到端。
7. 不做跨功能共享组件库：包内组件私有，跨功能复用走 `ada:components`、UI 服务或 slots；否则「一个包 = 一项功能」会退化回巨石。

---

## 2. 为什么不用现成框架

| 方案 | 致命点 | 我们借什么 |
|---|---|---|
| Vue 3 SFC + Vite | 构建链要 Node；`@vue/compiler-dom` 运行时编译走 `new Function`，CSP 需 `unsafe-eval`；远程加载无法在验签后才执行 | SFC 的形态、模板表达式的心智 |
| Svelte | 编译器是 JS，必须 Node；产物是 JS 渲染函数，无法在 Go 里生成 | 「编译期把模板变成精确更新」的目标 |
| React + htm/JSX | JSX 要构建；运行时 `htm` 用 tagged template 仍有解析成本；无组件包约定 | 组件边界与单向数据流 |
| Web Components 原生 | 没有模板/作用域样式/响应式；每个组件手写 DOM；跨插件样式难隔离 | 自定义元素可作为未来输出格式 |
| 纯 Go WASM 运行时 | 首屏多 1–3 MB；DOM 访问经 syscall/js 昂贵；调试复杂 | 无（仅作为反例） |

结论：**借 SFC 的形，不借它的构建链**。编译放 Go，运行放浏览器，二者用可序列化的 Plan 通信。

---

## 3. 总体架构

### 3.1 分层

```
┌───────────────────────────── 浏览器 / WebView ─────────────────────────────┐
│  UI 功能包（可禁用、可替换；一个包一项功能）                               │
│  ui-shell · ui-settings · ui-inspector · 业务页面 · 插槽 · 命令             │
│                              │ 贡献（数据）                                  │
│  ┌───────────────────────────▼───────────────────────────────────────────┐ │
│  │ Ada UI 运行时内核（基础设施，不可禁用）                                 │ │
│  │ loader · service table · effect scope · plan interpreter ·             │ │
│  │ reactive scheduler · router · DOM adapter · bridge client · i18n       │ │
│  │ 纯 ESM，无 eval，无 Node                                                │ │
│  └───────────────────────────┬───────────────────────────────────────────┘ │
└──────────────────────────────┼─────────────────────────────────────────────┘
                               │ 同源 HTTP + SSE（web）
                               │ WebView2 Bind / Electron IPC（desktop）
┌──────────────────────────────▼─────────────────────────────────────────────┐
│  host/web（宿主适配器，只做协议映射）                                       │
│  bootstrap · 静态资产 · 深链回退 · 动作 RPC · 确认对话框（UX） · SSE · 审计          │
└──────────────────────────────┬─────────────────────────────────────────────┘
                               │
┌──────────────────────────────▼─────────────────────────────────────────────┐
│  Go 内核与既有基础设施                                                      │
│  plugin/install · ui 编译器 · caps.Broker · assemble · registry · observe    │
└────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 一次点击的链路

```
<button a-on:click="inc">  （Plan 里的 Event 节点）
  → 运行时：表达式求值 → setup 方法 inc()
  → 响应式写入 state.count
  → 微任务批处理 → 绑定 effect → DOM patch（只改文本节点）
  → 若动作需要宿主：ctx.api.action('files.refresh', input)
      → POST /api/ui/action（CSRF；服务端按 action id 反查属主）
      → Go：caps.Broker.Authorize(plugin, "ui") + 属主/调用方白名单（防误用）→ 执行
      → audit JSONL（plugin/cap/target/result/traceID）→ JSON 结果
```

宿主主动请求 UI：

```
Go 工具需要确认 → seam/ui.Request(id, ...)
  → SSE event: ui/request {id, kind, title, options}
  → SPA 弹 Dialog → POST /api/ui/response {id, choice}
  → Go future 唤醒；超时/断连 = 拒绝。它只是 UX 通道；安全审批仍由 Go 的 approval seam 决定。
```

### 3.3 启动时序

```
1. GET /                 → index.html（无缓存；最小骨架 + 加载占位）
2. GET /api/ui/bootstrap → UI Plan：preset、locale、theme、插件表、贡献表、CSRF
3. 运行时校验 ui.apiVersion / planVersion / 资产摘要
4. 按 inject 拓扑序：加载内置 → 加载插件 → apply → 提交贡献
5. 首路由渲染（深链直接进入目标路由）
6. 建立 SSE；收到 ui/plugin-* 后增量装配
```

启动期间任何插件失败都**不阻断 shell**：错误进边界与 Inspector，失败插件标 `failed`/`quarantined`。

---

## 4. 组件与组件包 `.aui`

### 4.1 文件结构

一个 `.aui` 最多四段 + 一个可选 i18n 段，顺序无关：

```aui
<meta>
{ "name": "Hello" }
</meta>

<template>
  <button class="hello" a-on:click="inc">
    {{ $t('clicks', state.count) }}（{{ doubled }}）
  </button>
</template>

<script>
import { reactive, computed } from 'ada:surface'

export default {
  name: 'Hello',
  props: { label: { type: String, default: '按钮' } },
  emits: ['greet'],
  setup(props, ctx) {
    const state = reactive({ count: 0 })
    const doubled = computed(() => state.count * 2)
    function inc() {
      state.count++
      if (state.count === 3) ctx.emit('greet', { count: state.count })
    }
    return { state, doubled, inc }
  },
}
</script>

<style scoped>
.hello {
  padding: var(--ada-space-2) var(--ada-space-3);
  border-radius: var(--ada-radius-md);
  background: var(--ada-color-primary);
  color: var(--ada-color-on-primary);
}
</style>

<i18n>
{
  "zh-CN": { "clicks": "点了 {0} 次" },
  "en":    { "clicks": "Clicked {0} times" }
}
</i18n>
```

硬规则：

| 规则 | 理由 |
|---|---|
| 根节点必须是**单个元素**（不能是 fragment/文本） | 作用域属性 `data-aui` 要落在根上；也便于布局与错误边界 |
| `<style>` 默认且只能 scoped；禁止 `:root`/`html`/`body`/`*`、`@import`、`@font-face`、远程 `url()`；允许 `@media`/`@supports`/`@keyframes`，内部选择器同样作用域化，关键帧名加组件前缀；不支持 CSS 嵌套 | 不能污染宿主与其他插件；字体由宿主提供；CSP 不放宽 |
| `<script>` 是 ESM，只允许 `ada:*` 与相对导入；禁止 CJS/`require`/`.node` | 无 Node；与 compile 层边界一致 |
| 禁止 `eval`/`new Function`/`innerHTML`/`document.write` | 无动态代码；CSP 与 Trusted Types 可开 |
| 模板只允许白名单标签与属性；禁止 `script/style/iframe/object/embed/base/link/meta` 与 `on*` 属性 | 防注入与绕过；事件必须走 `a-on:` |
| 文件 ≤ 256 KiB，元素 ≤ 2000，样式选择器 ≤ 400 | 保证编译与首屏预算 |
| 图片/字体只能来自宿主图标、`data:`（图片）或 `ui.assets`（U2，同源内容寻址）；禁止远程 `url()` | CSP 与供应链：不让插件把请求引到任意域 |

### 4.2 模板语法

| 写法 | 含义 | 编译为 |
|---|---|---|
| `{{ expr }}` | 文本插值（默认 HTML 转义） | `text` 节点 + 表达式槽 |
| `a-if` / `a-else-if` / `a-else` | 条件分支，必须相邻兄弟 | `if` 节点与分支表 |
| `a-for="item, i in state.items"` | 列表；`a-key` 必填（对象项） | `for` 节点 + 键表达式 |
| `a-on:click="inc"` | 事件；`inc` 解析为组件方法 | `event` + 动作 AST |
| `a-on:click="select(item)"` | 带参动作；`$event` 可用 | 调用 AST，只允许作用域内函数 |
| `a-on:keydown.enter.prevent` | 修饰符：`prevent/stop/once/self/enter/esc` | 事件选项 |
| `a-model="state.query"` | 双向绑定（input/select/checkbox） | `model` 槽，读写响应式属性 |
| `a-bind:title="state.title"` | 动态属性 | `attr` 槽 |
| `a-class:active="state.active"` | 动态 class | `class` 槽 |
| `a-ref="input"` | 模板引用 | `ref`，挂载后写入 `ctx.refs` |
| `a-prop:label="state.label"` | 组件动态 prop；裸属性是静态字符串 | `prop` 槽 |
| `a-slot="footer"` | 父组件向子组件具名插槽提供内容 | 插槽内容块 |
| `<slot>` / `<slot name="footer">` | 子组件插槽出口；可带默认内容 | `slot` 节点 |
| `<Hello a-on:greet="onGreet" />` | 大写标签 = 组件；解析 `components` 注册表 | `comp` 节点 |

不提供 `a-html`：需要富文本时用结构化数据 + 标准组件，不能把字符串变成 DOM。也不提供 `a-bind:style`，并禁止静态 `style` 属性：内联样式会迫使 `style-src` 放宽，动态样式只走 class 或 CSS 变量。

**表达式**（`ui/expr`，与 flow 的 `expr` 刻意分开）：

- 值：字符串/数字/布尔/`null`、数组字面量、路径、成员/下标、一元 `!`/`-`、比较、逻辑、`??`、三元、括号。
- 调用：只允许「作用域内函数」与内置 `$t(key, ...args)`；禁止 `new`、赋值、`await`、`this`、全局对象（`window/document/globalThis`）、`__proto__/constructor/prototype`。
- 求值：运行时 AST 解释，不拼字符串、不 `eval`；属性读取只走 own property；单次求值 ≤ 10 万步。
- `$t` 只做查表与占位替换；缺失 key 返回 `key` 本身并上报 warning（不静默）。
- 根表达式是 `ref`/`computed` 时自动取 `.value`（一层）；二元表达式里需要显式 `.value`。

### 4.3 脚本与组件模型

```js
// 组件契约（运行时按这些字段归一化；defineComponent 只是类型辅助，不是必需）
export default {
  name: 'X',
  props: { ... },        // 只读；父传子；类型检查仅 dev
  emits: ['change'],     // 未声明的事件在 dev 报警
  setup(props, ctx) {
    // ctx: emit / t / service / refs / router / api / onCleanup / slots
    return { ... }       // 必须返回模板可见的绑定（对象/ref/函数）
  },
}
```

- **状态**：`reactive(obj)` 深代理，按属性/key 追踪；`ref(v)` 用于基本类型；`computed(fn)` 缓存；`watch(source, cb, {immediate})`；`effect(fn)` 自动追踪并返回 stop。
- **更新**：写入后在微任务合并；同一次 flush 内每个绑定最多执行一次；`flushSync()` 只给测试与少数同步场景。
- **生命周期**：`onMount`/`onUnmount`/`onError`；`ctx.onCleanup` 在组件卸载与插件停用两处都执行。
- **Props**：父更新触发子绑定；子不得改写 props（dev 报错，生产忽略）。
- **组件注册**：插件 `apply` 里 `ctx.components.register(name, comp, plan)`；也可由清单静态声明 `components`，运行时按名字懒加载。组件名只在**功能包内**唯一，运行时键是 `<plugin-id>:<name>`；**不支持跨包组件引用**，跨功能复用走 slots、UI 服务或 `ada:components`。
- **异步组件**：`ctx.components.registerLazy(name)`；组件由清单声明，首次挂载时才下载/校验/挂载。
- **模板作用域**：`a-for` 引入 `item`/`i`，嵌套 `a-if`/`a-on` 动作可引用；离开子树即失效。内置 `$event`、`$t`、`$refs` 只在表达式里可见，不泄漏到 `setup`。
- **样式隔离**：每个元素加 `data-aui="<组件 id>"`，选择器统一改写为 `sel[data-aui=...]`；插槽内容保留父作用域，子组件样式不穿透。

### 4.4 i18n 与设计令牌

- 文案进 `<i18n>` 段，按插件 id 命名空间合并；`$t(key, ...args)` 用 `{0}`、`{1}` 位置占位；先查插件包，再查宿主包，再回退 `zh-CN`，最后返回 key 并上报 warning。
- 可见文案应走 `$t`；硬编码只报 warning（`<code>`/`<pre>` 与 `aria-label` 可用字面量），不阻断构建。
- 颜色/间距/圆角/阴影只用 `var(--ada-*)`；原始色值 lint warning。主题由 `ui-theme` 插件提供，切换只改根节点 `data-ada-theme`。
- 主题产物是 `theme.css`（允许 `:root`/`[data-ada-theme]` 等全局选择器），只允许 `builtin`/`official` 的 `ui-theme` 提供；组件样式仍严格 scoped。主题切换不写内联 `style`，避免 CSP 放宽。
- 宿主提供 `prefers-color-scheme`、`prefers-reduced-motion` 的默认响应；插件不得覆盖全局 `@media` 行为。

### 4.5 编译产物

每个组件产出 3–4 个文件（路径带内容摘要，便于同源缓存与验签）：

```
dist/ui/
  hello.plan.json        # RenderPlan（数据）
  hello.module.js        # ESM：只导出组件对象与（可选）apply；import 只来自 ada:*
  hello.css              # 已作用域化
  hello.i18n.json        # 可选
  entry.module.js        # 可选：导出 apply(ctx, config)
  ui.manifest.json       # 包清单：entry + components[] + files[] + sha256 + planVersion
```

`hello.module.js` 形如（Plan 与模块**分开**：Plan 是可读 JSON，模块不含模板；loader 把两者一起交给运行时）：

```js
import { reactive, computed } from 'ada:surface'
export default {
  name: 'Hello',
  props: { /* … */ },
  setup,   // 编译器不改写作者代码，只做 TS→ESM
}
```

加载时：`import(moduleURL)` 取默认导出，`fetch(planURL)` 取 Plan，`components.register(name, comp, plan)`。两者都先过摘要校验；Plan 不合法则拒绝挂载，不进入组件代码。

Plan 示例（对应 §4.1 的按钮）：

```json
{
  "planVersion": 1,
  "id": "hello",
  "root": {
    "k": "el",
    "tag": "button",
    "attrs": [{ "name": "class", "value": "hello" }],
    "on": [{ "event": "click", "action": { "t": "path", "p": ["inc"] }, "mods": [] }],
    "children": [
      { "k": "text", "expr": { "t": "call", "fn": { "t": "path", "p": ["$t"] },
        "args": [{ "t": "lit", "v": "clicks" }, { "t": "path", "p": ["state", "count"] }] } },
      { "k": "text", "value": "（" },
      { "k": "text", "expr": { "t": "path", "p": ["doubled"], "unwrap": true } },
      { "k": "text", "value": "）" }
    ]
  },
  "style": "hello.7f3a.css"
}
```

节点类型：`el` / `text` / `if` / `for` / `comp` / `slot` / `frag`（仅内部）。静态文本在编译期合并；动态槽带 `expr` 与诊断用 `bid`。

### 4.6 组件包：一项功能的完整交付

**功能包**是交付单位：一个组件是退化情形，多个组件组成的包是常态。包内所有 `.aui` 共享一个入口、一个 i18n 命名空间、一次编译/签名/安装；它们不是多个独立功能。

```
plugins/ui/files/
  plugin.json          # ui.package = "ui"，ui.entry = "ui/Feature.aui"
  ui/
    Feature.aui        # 入口组件（页面/面板本体）
    Tree.aui           # 私有子组件
    Toolbar.aui        # 私有子组件
    index.js           # 可选：apply(ctx, config)
    assets/icon.svg    # 可选（U2）
```

| 规则 | 说明 |
|---|---|
| 一项功能 = 一个包 | 一个插件最多一个 UI 功能包；要多功能就拆成多个插件（每个插件一个包） |
| 单组件功能 | 包内只有一个 `.aui`，`ui.package="."`、`entry` 指向它；`plugin.json` + 一个文件即可 |
| 包内组件私有 | 不做 `other-package:Comp` 跨包引用；复用走 `ada:components`、UI 服务或 slots |
| 一个顶层 page | 一个功能包最多贡献一个顶层 page；详情/子视图用包内 `router.add` 子路由 |
| 共享命名空间 | 包级 i18n、样式作用域、组件名都在包内唯一；包内组件不单独发版 |
| 整体生命周期 | 编译/摘要/签名/安装/停用/隔离都以包为单位；包内组件不单独启用 |
| 路径约束 | `entry` 必须是 `components` 之一；所有路径必须落在 `ui.package` 目录内，越界 → 拒绝 |
| 声明式包 | 没有 `.aui` 的功能包（§10）仍是同一个包模型，只是 `components=[]` |

`ui.manifest.json` 是**包清单**：记录 `entry`、`components`、`files`、`digest`、`planVersion`；`plugin.json` 的 `ui.package/entry/components` 是源声明。二者不一致 → 安装拒绝。

---

## 5. 编译管线（Go，无 Node）

### 5.1 流程

```
plugin.json（ui 段）
  → 1. 读清单、校验 ui.apiVersion/唯一功能包/组件表/文件上限
  → 2. 切块（meta/template/script/style/i18n）
  → 3. 模板 tokenizer + parser（受控 HTML 子集）→ 模板 AST
  → 4. 指令归一（if/for/on/model/bind/class/ref/slot）
  → 5. 表达式 parser（ui/expr）→ 表达式 AST
  → 6. 静态检查：标签/属性白名单、事件名、key、根节点、导入白名单
  → 7. 脚本：compile.Transform（TS→ESM，复用 esbuild Go API）
  → 8. 样式：CSS parser → 作用域改写 + 规则白名单
  → 9. a11y/文案/令牌 lint
  → 10. 生成 Plan/CSS/i18n/module；算 sha256；写 ui.manifest.json
```

Go API 草图：

```go
package ui

const (
    PlanVersion = 1
    APIVersion  = 1
)

type Options struct {
    Minify    bool
    SourceMap bool
    Target    string // 默认 es2020
}

type Package struct {
    Plugin     string   // 插件 id
    APIVersion int
    Entry      string   // 入口组件名（声明式包为空）
    Components []string // 包内组件名（私有）
    Files      []File   // dist/ui 下的文件（含摘要）
    Digest     string   // 覆盖 Files 的 sha256
}

func CompilePackage(dir string, opts Options) (*Package, error)
func (p *Package) Verify() error
```

一个插件最多一个 `ui` 功能包；声明里出现多个包/多个入口 → `EUISTRUCT`。包内组件不作为独立安装/启用单位。

约束：编译**不执行**组件代码；不解析运行时值；不做数据流分析。所有输出确定（同输入同字节），可直接 golden。

### 5.2 表达式

`ui/expr` 是独立包，不扩展现有 `expr`：后者是 flow 的「数据不允许带执行」契约，加函数调用会破坏它。

| 关注点 | 规则 |
|---|---|
| 语法 | 见 §4.2；递归下降，优先级与 JS 一致（除 `==` 仅严格比较） |
| 编译期检查 | 拒绝全局名/保留字/`new`；调用目标是否真是函数在求值时判定（不是函数 → `EUIEXPR`），编译期不做跨文件类型推断 |
| 求值安全 | 只查 own property；拒绝 `__proto__` 等键；步数上限；异常带位置 |
| 可测试 | Go 只测解析/诊断与 AST 结构；求值语义在 JS harness 用**同一份 fixture**测，避免两套实现 |

### 5.3 esbuild 的边界

- 只做 `Transform`（TS/JS → ESM）与可选的 `Bundle`（把相对导入合成单文件）；**不碰模板**。
- 与 AGENTS §11 的「esbuild 只做 TS→JS 降级」一致；不引入 bundler 的插件生态与 Node 解析语义。
- 产物 target 固定 `es2020`（import map、Proxy、async 都可用）；不支持 IE/旧 WebView。
- SourceMap 进 `ui.manifest.json` 的 `sourcemap` 字段，仅 dev 供给。

### 5.4 依赖

| 导入 | 处理 |
|---|---|
| `ada:surface` / `ada:components` | 外部化，由 import map 指向运行时；禁止插件自带副本 |
| `./x.js` / `./nested/x.js` | Go `Bundle` 内联；禁止逃出功能包目录 |
| 裸包名（`lodash`） | **U1/U2 拒绝**并给出码；U3 若评估通过，由 Go 从 registry 拉 npm 兼容 tarball、校验 integrity、esbuild 内联，**不跑任何生命周期脚本** |
| Node 内置（`fs`/`path`…） | 编译期拒绝 |
| `.node`/WASM | 不支持 |

这保留了「npm 的内容、无 Node 的运行时」：安装期解析与打包，运行期只有 ESM。

### 5.5 校验与诊断

错误一律带码、文件、行、列；`ada ui lint` 与 `ada ui build` 共用同一套检查：

| 类别 | 例子 | 码 |
|---|---|---|
| 结构 | 缺 template、重复块、根不是元素 | `EUISTRUCT` |
| 模板 | 未知指令、`a-else` 无 `a-if`、缺 `a-key` | `EUITEMPLATE` |
| 表达式 | 语法错、调全局、`new` | `EUIEXPR` |
| 样式 | 全局选择器、`@import`、远程 `url()` | `EUISTYLE` |
| 安全 | 禁用标签/属性、`on*`、`eval` | `EUISECURITY` |
| 导入 | 裸包、越界相对路径、Node 内置 | `EUIIMPORT` |
| 可访问性 | `img` 无 alt、按钮无文本、正 tabindex | `EUIA11Y` |
| 产物 | 摘要不符、planVersion 不支持 | `EUIARTIFACT` |

### 5.6 产物与签名

- `ui.manifest.json` 是**功能包清单**：`planVersion`、`entry`、`components`、`pages`（声明式）、文件表、摘要、字节数。
- **发布端**算 `ui.digest = sha256(规范化的 ui.manifest.json)`，写进 `plugin.json`，再按现有格式签名 `manifestJSON + "\n" + sigDigest`；`sigDigest` 对纯 UI 插件就是 `ui.digest`，对全栈插件是 `sha256(backendDigest + ui.digest)`。签名覆盖清单与两份产物，不是源码。
- **安装端**只校验，不补签；`kind=ui` 的插件也要走同一 digest/trust 分支，不能因为「没有后端 entry」跳过。
- **全栈插件的后端 Artifact 也必须预编译并签名**：`backendDigest` 来自发布端的 Artifact，不来自安装端的 `compile.Build`。`engines` 可记录 esbuild 版本用于复现，**不参与验签**；安装端不编译后再签。
- **远程安装只接受已编译且在签名范围内的产物**：发布包必须带后端 Artifact（全栈/ts）与 `dist/ui`（有组件则含 module/plan/css，声明式包只有页面数据文件）。安装期可以重编译做一致性校验，但结果必须与签名摘要逐字节一致，否则拒绝；本地开发目录安装才允许现场编译（信任来自本地）。
- **禁止对已装目录执行 `ada ui build` 产出未签产物**；`ada ui build` 只用于发布端与本地开发目录。
- 浏览器（hosted 模式）仍校验每个文件的 sha256：只做缓存完整性与防篡改，**不是信任锚**；信任锚是安装期的 Go 验签。

---

## 6. 运行时

### 6.1 内核边界

| 内核负责 | 内核**不**负责 |
|---|---|
| 激活/停用作用域、服务表、effect 回收 | 具体页面、导航结构、品牌文案 |
| Plan → DOM 的创建/更新/销毁 | 业务数据获取与缓存策略 |
| 响应式追踪与批处理 | 权限判定（在 Go） |
| 路由匹配、深链、滚动恢复 | 产品开关（在 preset） |
| 资产加载、摘要校验、模块导入 | 远程仓库协议（在 host/registry） |
| i18n 合并与回退、主题变量应用 | 设计令牌的具体值（在 `ui-theme`/preset） |
| 错误边界、Inspector 采集 | 日志落盘（在 Go observe） |

内核**不提供**：`eval`、`new Function`、`innerHTML`、`document.write`、`localStorage`、`window.postMessage`、任意 `import()`（只有 loader 导入已校验 URL）。同域代码当然能绕过这些「不提供」——这正是 §9.2 的信任边界；lint 会拒绝常见写法，但不把它当沙箱。

### 6.2 响应式

- `reactive` 用 Proxy 深代理：读追踪属性，写触发依赖；数组方法（push/splice/sort）批量标脏。
- `computed` 惰性 + 缓存；依赖变化标脏，下一次读取重算（不在写入时同步重算）。
- 调度：微任务队列；一次 flush 内按拓扑执行绑定 effect；组件卸载前先停 effect。
- 防抖/节流不进内核：需要时用 `watch` + 插件自带工具。
- 循环依赖检测：computed 求值栈深度 > 100 → `EUIREACTIVE`。

### 6.3 渲染（Plan 解释器 + Op 序列）

渲染核心**不直接碰 DOM**，只生成有序 Op 序列，交给 `DomAdapter` 执行；浏览器适配器落 DOM，测试适配器只记录。这样同一份核心不需要 DOM shim，也不需要两套实现。

Op 词表是契约（新增 op 要更新本节与 §14）：

| Op | 语义 |
|---|---|
| `create(tag, key)` / `createText()` | 建元素/文本节点 |
| `setText(id, text)` | 设文本（转义后字符串） |
| `setAttr(id, name, value)` / `removeAttr(id, name)` | 属性 |
| `setClass(id, name, on)` | class 差量 |
| `addEvent(id, type, options)` / `removeEvent(id, type)` | 监听（修饰符已在 options） |
| `insert(parent, child, before)` | 插入 |
| `move(parent, child, before)` | 移动（keyed reconcile） |
| `remove(id)` | 删除子树 |
| `focus(id)` / `scroll(id, x, y)` | 焦点/滚动 |
| `dispose(id)` | 释放实例/监听/引用 |

规则：

- **挂载**：按 Plan 递归生成 create/insert；动态槽各自建一个 effect，首次运行即生成 op。挂载前预取 CSS 并等待可用，避免 FOUC；失败按「无样式 + 报错」降级。
- **更新**：只重跑依赖变化的槽，生成 setText/setAttr/setClass 等最小 op。
- **条件**：分支切换生成 remove + 新子树 create/insert；refs/组件实例同步 dispose。
- **列表**：keyed reconcile 建 key→id 表，复用/移动/插入/删除；移动按需做 LIS 减少 op；无 key 的原始项按索引复用并警告（dev）。
- **组件**：`comp` 节点创建子实例，props 通过响应式对象传递；子 `emit` 调父动作；卸载递归 dispose。
- **插槽**：父提供的内容块在子组件内按 name 插入；插槽内容的作用域与样式归父组件。
- **事件**：编译期把修饰符变成监听选项；动作表达式在事件触发时求值，异常进错误边界。
- **表单**：`a-model` 处理 input/textarea/select/checkbox/radio；IME 在 `compositionend` 后同步。
- **无障碍**：路由切换后焦点回到主区域标题；Dialog 焦点陷阱在标准组件里；`aria-live` 用于异步状态。

### 6.4 路由器（SPA）

```js
ctx.router.add({
  path: '/files/:id',
  name: 'file',
  component: 'FileView',      // 组件注册名
  layout: 'AppLayout',
  title: () => ctx.t('files.title'),
  guard: (to, from) => (to.params.id ? true : '/files'), // 同步、可拒绝
})
```

| 规则 | 说明 |
|---|---|
| 匹配顺序 | 静态 > 参数 > 通配；同名/同路径重复注册 → 激活失败 |
| 嵌套 | `layout` 提供外框；子路由挂到默认 outlet；最多 4 层 |
| 守卫 | **只允许同步**：返回 `true`/`false`/重定向字符串；异步逻辑放组件 `onMount` |
| 导航 | `push/replace/back`、`Link` 组件；保留 query/hash；滚动恢复按路由记录 |
| 深链 | 宿主对非 `/api`、`/app`、`/plugins` 的 GET 返回 index.html；basePath 由 bootstrap 给定 |
| 保留段 | `api`/`app`/`plugins`/`static` 不能作为页面 slug；编译期拒绝，避免与资产/接口路由冲突 |
| 404/错误 | 内置 404 与路由级错误边界；插件路由失败不影响其他路由 |
| 预取 | `Link` hover/focus 预取组件模块；只预取，不激活插件 |

### 6.5 激活与服务

状态机（对齐 kernel 的 fiber 语义）：

```
pending（inject 未满足） → loading（取资产） → verifying（摘要/版本）
  → activating（apply） → active
        ↘ failed（可重试） → quarantined（连续失败 3 次，持久化）
active → updating（新版本 staging） → active（原子切换）
active → disposing → removed
```

- `ctx.effect(fn)`：`fn` 返回 disposer；激活作用域卸载时逆序执行；重复注册同名贡献 → 失败并回滚，不静默覆盖。
- 动态贡献（`apply` 里注册的路由/插槽/命令/服务）提交后发内部 `contrib/changed`；`ui-shell` 与命令面板订阅它，bootstrap 只包含静态贡献，不假装知道运行期结果。
- `inject`：前端依赖来自 `ui.inject`（不是顶层 Go `inject`）；必需服务缺失 → `pending`，服务表变化后重试；可选缺失 → 照常激活并记录 warning。禁止 `if (!svc) return` 的静默跳过。
- `provide`：UI 插件通过 `ui.provide` 声明服务（如 `theme`）；Go 在 bootstrap 里按 `ui.inject`/`ui.provide` 拓扑排序；运行期动态 `ctx.provide` 仍按 pending 语义处理。同名多实例按作用域隔离，默认后注册覆盖先注册并记 warning。
- 服务表：`router`/`i18n`/`components`/`api`/`storage`/`commands` 由内核提供；`overlays` 由 `ui-shell` 提供（没有 shell 时依赖它的插件保持 `pending`）；`theme` 由 `ui-theme` 提供；`inspector` 仅 dev。
- `ctx.components.load(name)`：按清单预取组件（module + plan + css），返回 Promise；只能加载清单内组件，不能加载任意 URL。
- 插件 `apply` 必须可重入、无模块级状态；状态进 `ctx.store`（按插件 id 命名空间，持久化可选）。

### 6.6 错误边界与降级

| 失败 | 表现 |
|---|---|
| 组件渲染抛错 | 边界显示错误卡片（插件名 + 追踪 id），其余页面继续 |
| 路由组件加载失败 | 路由级错误页 + 重试；shell 不受影响 |
| `apply` 抛错 | 插件回滚，标记 `failed`；导航里隐藏其页面 |
| 远程资产摘要不符 | 拒绝激活，`EUIINTEGRITY`，旧版本继续服务 |
| 插件连续崩溃 | `quarantined`，下次启动（`?safe=1` 或宿主安全模式）跳过；用户可手动清隔离 |
| shell 自身失败 | 内核渲染最小错误页（无插件、无路由），仍能重载或访问 `/api/ui/health`；不依赖任何 UI 插件 |
| 全局未捕获 | 上报 Go（带 traceID），显示「重载」按钮；不白屏 |

### 6.7 运行时 API 面

| 导出 | 用途 |
|---|---|
| `reactive/ref/computed/watch/effect/batch/flushSync` | 状态 |
| `onMount/onUnmount/onError` | 生命周期 |
| `defineComponent` | 可选的类型/编辑器辅助，运行时接受普通对象 |
| `Link` / `useRoute` / `useRouter` | 路由 |
| `Dialog/Toast` 等标准组件 | 经 `ada:components`，不直接从 `ada:surface` 导出 |
| `ctx`（apply 参数） | 注册、服务、事件、桥、配置、日志 |

`ctx` 的注册面（全部返回 `Dispose`，必须包在 `ctx.effect` 里）：

| 成员 | 作用 |
|---|---|
| `ctx.effect(fn)` | 托管副作用；卸载逆序执行 |
| `ctx.service(name)` | 取前端服务；缺失按 `ui.inject` → `pending`，运行期抛 `EUISERVICE` |
| `ctx.components.register/registerLazy/load` | 注册、懒注册、预取组件 |
| `ctx.router.add` | 注册路由 |
| `ctx.slots.add` | 注册插槽贡献 |
| `ctx.commands.add` | 注册命令 |
| `ctx.i18n.add(key, messages)` / `ctx.t(key, ...args)` | 文案 |
| `ctx.api.action(id, input)` / `ctx.api.call(method, payload)` / `ctx.api.on(event, fn)` | 桥 |
| `ctx.storage.get/set` | 插件命名空间存储 |
| `ctx.store` | 插件级状态（可持久化） |
| `ctx.log` | 结构化日志（自动带插件 id/traceID） |

`ada:surface` 与 `ada:components` 是两个允许的运行时导入，都由 import map 指到同源资产；`ada:router` 不单独存在，路由 API 从 `ada:surface` 导出。

---

## 7. 插件模型

### 7.1 清单扩展

```jsonc
{
  "id": "hello-panel",
  "name": "你好面板",
  "version": "0.1.0",
  "kind": "ui",                    // 新增：纯前端插件；ts 插件也可带 ui 段
  "ui": {
    "apiVersion": 1,
    "package": "ui",               // 功能包目录；单组件功能写 "."；一个插件最多一个
    "digest": "sha256:…",          // 发布时写入：ui.manifest.json 的规范化摘要
    "inject": { "required": ["i18n"], "optional": ["theme"] }, // 前端服务依赖；与顶层 Go inject 分开
    "provide": ["hello"],                                     // 可选：前端服务，供其他功能包注入
    "components": [
      { "name": "Feature", "path": "ui/Feature.aui" },        // 入口组件
      { "name": "Settings", "path": "ui/Settings.aui" }        // 包内私有组件
    ],
    "entry": "ui/Feature.aui",     // 入口组件；声明式包为空。该文件可另导出 apply(ctx, config)
    "actions": ["hello.greet"]     // 允许这个功能包调用的动作（必须由动作属主声明 ui:true）
  },
  "inject": { "required": [], "optional": [] },  // Go 服务依赖；kind=ui 时为空
  "contributes": {
    "pages": [{ "slug": "hello", "title": "你好", "component": "Feature", "render": "custom", "order": 50 }],
    "slots": [{ "slot": "settings.section", "component": "Settings", "order": 10 }],
    "commands": [{ "id": "hello.greet", "title": "打招呼" }],
    "actions": [{ "id": "hello.greet", "title": "打招呼", "ui": true }]
  },
  "caps": ["ui"],
  "trust": "official",
  "engines": { "ada": ">=2.0.0 <3.0.0" }
}
```

- `kind: "ui"`：无 Go/TS 后端；安装只校验/编译一个功能包；顶层 `inject` 必须为空。它是**枚举扩展**：旧宿主会拒绝该 kind，发布时必须用 `engines`/`ui.apiVersion` 做版本门禁。
- `kind: "ts"` 可同时带后端 `entry` 与一个 `ui` 功能包，一个插件 id、两套运行时；Go 依赖走顶层 `inject`，前端依赖走 `ui.inject`。
- **一个插件最多一个功能包**：`ui.package` 唯一、`ui.entry` 唯一；要多个功能就拆成多个插件（每个插件一个包），不允许「一个包塞多个不相关功能」。
- `contributes.pages` 增加 `component`（仅 `render: "custom"` 有效）与 `order`；`render: "host"` 使用声明式页面（§10）。`slug` 即路由段，`title` 可以是 i18n key；**`component` 只能引用本功能包内的组件**，`slot.component` 同理；**一个功能包最多一个顶层 page**，子视图用包内子路由。
- `ui.actions` 是**防误用/错配**的调用白名单；动作属主必须在其 `contributes.actions` 里标 `ui: true`。它不防同域恶意（同域插件可冒充），真正的防线是签名与安装信任（§9.2）。能力名仍只用 `caps.md` 已有的 `ui`，不新增子命名空间。
- 静态贡献（清单里写的）在激活前预注册，失败即回滚；动态贡献（`apply` 里注册）同事务。冲突 → 报「已由插件 X 注册」。

### 7.2 功能包形态

| 形态 | 组成 | 适用 |
|---|---|---|
| **单组件功能包** | `plugin.json` + 一个 `.aui` | 小功能、单面板 |
| **多组件功能包** | `plugin.json` + `ui/` 下多个 `.aui`（入口 + 私有子组件）+ 可选 `index.js`/资源 | 文件树、编辑器、时间线 |
| **声明式功能包** | `plugin.json` + `contributes.pages` 数据，无 `.aui` | 第三方/只读页面 |
| **全栈功能包** | 上述任一 + 后端 `entry` | 需要 Go/TS 数据面、文件、模型调用 |
| **内置功能包** | 随二进制 `embed` 的产物 + 同一套清单 | Shell、设置、检查器、默认页面 |

所有形态都走同一安装/编译/激活管线；内置只是来源为 `builtin`，**没有特权代码路径**（代码在二进制里，签名豁免）。**一个功能包对应一项功能**，包内组件是私有实现，不是独立功能。

稳定插槽（新增要更新本文，不能随手起名）：`settings.section`（设置页区块）、`page.header`/`page.footer`（内容区上下）、`sidebar.footer`（侧栏底部）、`empty.state`（空态补充）、`route.error`（路由错误页补充）。插件只能往这些位置加内容，不能替换宿主 chrome。

### 7.3 `apply` 示例

功能包的 `ui/index.js`（或入口组件）导出 `apply(ctx, config)`；一个功能包最多一次激活：

```js
export function apply(ctx, config) {
  // 每个注册都进作用域；卸载时自动撤销
  ctx.effect(ctx.components.registerLazy('FileTree')) // 包内组件由清单声明
  ctx.effect(ctx.router.add({ path: '/files', component: 'FileTree', title: ctx.t('files.title') }))
  ctx.effect(ctx.slots.add('settings.section', { component: 'FileTreeSettings', order: 20 }))
  ctx.effect(ctx.i18n.add('zh-CN', { files: '文件' }))
  ctx.effect(ctx.commands.add({ id: 'files.refresh', title: '刷新文件', run: refresh }))
  ctx.effect(ctx.api.on('files/changed', refresh))
}
```

- `apply` 不得直接改 DOM、不得 `fetch` 任意地址（走 `ctx.api` / `ctx.components.load`）。
- 所有注册返回 `Dispose`；`ctx.effect` 之外的注册视为泄漏，dev 报错。
- 配置来自 Go 的已验证 config；**secret 字段不下发**（`x-secret` 在 bootstrap 前剔除）。

### 7.4 preset 与产品差异

```jsonc
{
  "ui": {
    "shell": "ui-shell",
    "home": "chat",
    "theme": "ada-light",
    "locale": "zh-CN"
  }
}
```

- 装哪些 UI 功能包仍由 preset 的插件列表决定；`ui` 段只选壳、首页、主题、语言。
- `ui-shell` 的职责：导航（pages → 菜单）、命令面板（commands）、设置入口、主题/语言切换、toasts/dialogs、全局错误边界；它只是插件，可被 `ui-shell-minimal` 整体替换。
- 设置页由宿主按 `contributes.settings` 的 schema 渲染；插件只声明字段与默认值，不画全局设置页；需要自定义区块时用 `settings.section` 插槽，仍受令牌与布局约束。
- 没有 `if product == "designer"`；没有硬编码插件列表。
- 两个产品 = 两份 preset + 可能不同的 host 适配器（web/desktop）。

### 7.5 最小功能包

单组件功能（包内只有一个组件）：

```
plugins/ui/hello/
  plugin.json      # ui.package=".", ui.entry="Hello.aui"
  Hello.aui        # 视图/状态/样式/文案全部在这一个文件里
```

多组件功能（一个功能包，入口 + 私有子组件）：

```
plugins/ui/files/
  plugin.json      # ui.package="ui", ui.entry="ui/Feature.aui"
  ui/
    Feature.aui    # 入口：页面/面板本体
    Tree.aui       # 私有子组件
    Toolbar.aui    # 私有子组件
    index.js       # 可选：apply(ctx, config)
```

两种都是「一项功能 = 一个包」：单组件是退化情形，多组件是常态。`plugin.json` 只做静态声明，不承载 UI 逻辑；包内子组件不是独立功能，不单独安装/发版。若要求「绝对一个文件」，可以由 `ada ui build` 从 `<meta>` 生成清单，但签名与静态审计仍以独立包清单为准——不推荐为了少一个文件牺牲可审计性。

---

## 8. 远程加载与信任

### 8.1 部署模式

| 模式 | 资产来源 | 验签方 | 能力 | 适用 |
|---|---|---|---|---|
| **hosted**（主线） | Go 宿主同源 `/plugins/…` | Go 安装期验签 + 浏览器校验摘要 | 动作/审计完整；审批仅 CLI/宿主原生通道，Web 内需审批的动作 fail-closed | 桌面、内网、单机 |
| static（非目标，U3 之后单独立项） | 同源预编译资产 | 部署时验签，运行期不验 | 只读、无 actions/审批/审计 | 演示、只读站点 |

**本设计只承诺 hosted**。远程加载 = 运行时从远程安装插件，不是浏览器直接 `import` 任意 URL。static 若将来要做，必须单独 spec（同源、无 `blob:`、无运行期验签、无 actions）；在那之前不写进验收。

hosted 链路：远程 zip 由 CLI/仓库服务下载到临时目录 → `ada install <目录>` → 验签/摘要/trust → 产物进内容寻址资产库 → 切换索引 → `host/web` 通知 SPA 增量装配。信任不交给浏览器：浏览器只校验摘要，验签由 Go 完成。

### 8.2 加载管线（hosted）

```
1. 发布端：后端（若有）`compile.Build` → Artifact；`ada ui build` → dist/ui + ui.manifest.json（含 ui.digest）
   plugin.json 写入 ui.digest（全栈再写 backend digest）；签名覆盖 manifestJSON + "\n" + sigDigest
2. 远程包下载到临时目录 → ada install <目录>
   路径安全检查 → 读 plugin.json → schema/ui.apiVersion → 验签（含 ui.digest）
   → 校验 dist/ui 文件摘要 → trust；远程包拒绝现场编译
3. install 在同一事务里：复制 UI 文件到内容寻址资产库 assets/<sha256>/<file>，再原子写 index.json；
   任一步失败 → 回滚复制，索引不变
4. install 通知长驻宿主 ada web（同进程事件或带 admin token 的 reload API；不 sleep 轮询）
   → SSE ui/plugin-added
5. SPA：GET 资产（同源、immutable）→ 按 ui.manifest 校验 sha256 → import(module)
6. 新作用域 apply → 贡献提交 → 路由/导航/插槽生效
7. 旧版本实例继续服务到最后一个引用卸载 → dispose；资产由宿主按保留期回收
```

本地开发目录（无签名）可以在 `ada ui dev` 下现场编译；生产安装不做这件事。

### 8.3 原子更新与回滚

- **资产与插件目录解耦**：内容寻址资产库保存运行中的旧版本，`ada install` 的旧目录清理不再让旧 URL 立刻 404。
- 新版本先以独立摘要 URL 与独立作用域 **staging**；全部成功后才切换索引（blue-green）。
- 组件注册带引用计数：已挂载的旧实例保留，直到路由离开/组件卸载；新实例用新版本。
- 失败：丢弃 staging，旧版本继续；错误上报并在 Inspector 标红。
- 卸载：从注册表移除 → 停用作用域 → 移除样式/路由/监听 → 页面回退到 404 或 shell 占位；资产进 GC 队列。
- 连续失败 3 次 → `quarantined`（持久化到宿主 storage，不落 cookie）。
- `host/web` 必须是**长驻宿主**（新命令 `ada web`，不复用只读的 `ada serve`；desktop 用 host/desktop），由它持有资产库与 SSE；没有长驻宿主就没有「不重启安装」。
### 8.4 缓存

- 资产 URL 含摘要；`Cache-Control: public, max-age=31536000, immutable`。
- 运行时按摘要写入 Cache Storage；启动时先命中缓存，再按 `index.json`（唯一版本真相）+ bootstrap 的 `lock.digest` 校验快照。
- 远程索引只用于**显式更新**，不自动跟随 `latest`；版本锁定与安装索引是同一份 `index.json`，不再另设 `ui-lock.json`。
- 离线：已加载页面在断网时可继续交互；**冷启动离线**要等 PWA/Service Worker（U3+，非目标），不在 U0–U2 验收。


### 8.5 版本与兼容

| 版本 | 位置 | 规则 |
|---|---|---|
| `apiVersion`（内核） | plugin.json | 主设计 §9.2 有此字段；当前 `plugin.Manifest` 未实现，U0 前置补齐后再按版本拒绝 |
| `ui.apiVersion` | ui 段 | 与运行时主版本不等 → `EUIAPIVERSION` |
| `planVersion` | ui.manifest.json | 运行时只接受已知清单；未知 → 拒绝，不回退猜测 |
| `engines.ada` | plugin.json | 按宿主版本判断；没有宿主时不适用 |

### 8.6 供应链

- 签名覆盖「清单 + 后端 Artifact digest + `ui.digest`」，公钥来自本机 keyring；无公钥 = 未验证 = 只能 `third-party`。
- 远程发布包必须含预编译后端 Artifact（若有）与 `dist/ui`；签名针对产物，不是源码。安装期重编译仅作一致性校验，不能替代签名；已装目录禁止再 build。
- 不自动更新；更新必须显式安装，且走同一验签流程。
- npm 依赖（U2 评估、U3 落地，若做）按 tarball integrity 校验后内联；**不执行 postinstall**。

### 8.7 SSE 协议

```
event: ui/plugin-added
data: {"id":"hello-panel","version":"0.1.0",
       "entry":"/plugins/…/entry.module.js",
       "components":[{"name":"Hello","module":"…","plan":"…","css":"…"}],
       "contributions":{"pages":[…],"slots":[…],"commands":[…]}}

event: ui/plugin-updated   data: {…同上，新版本…}
event: ui/plugin-removed   data: {"id":"hello-panel"}
event: ui/plugin-error     data: {"id":"…","code":"EUIPLUGIN","message":"…"}
event: ui/request          data: {"id":"req-1","kind":"confirm","title":"…","options":[…]}
event: ui/theme-changed    data: {"theme":"ada-dark"}
```

- 心跳 15 s；`Last-Event-ID` 断线重连；单事件 ≤ 256 KiB，超限改为「引用 + GET」。
- 事件是**通知**，不是门禁：装配是否允许仍由 Go 的授权与签名决定（P7）。

---

## 9. 安全模型

### 9.1 威胁

| 威胁 | 场景 | 对策 |
|---|---|---|
| 恶意 UI 代码读 DOM/令牌 | 远程插件偷会话 | 同域 = 完全可信，只允许签名/审核过的插件；未签名只能声明式或隔离 |
| XSS（数据变代码） | 用户输入进模板 | 文本默认转义；无 `a-html`；表达式是静态 AST，不拼接；CSP（Trusted Types 在 U2 验证后启用） |
| 供应链投毒 | 远程包被替换 | 清单签名 + 产物摘要 + 锁定版本 + 公钥固定 |
| 降级攻击 | 回滚到有漏洞旧版 | `index.json` 单调版本；安装审计；不自动更新 |
| 越权动作 | 插件调用别人的 action | 服务端按 action id 反查属主 + 调用方 `ui.actions` 白名单 + 审计；caps 只做粗粒度 `ui` 授权；同域信任仍是前提 |
| 样式/点击劫持 | 覆盖宿主 UI | scoped CSS + 令牌 + 禁止全局层；`frame-ancestors 'none'` |
| 资源耗尽 | 巨大 Plan/无限渲染 | 文件/节点/选择器上限；渲染预算；Inspector 可停用插件 |
| 原型污染 | 表达式访问 `__proto__` | own property + 键黑名单；reactive 只代理普通对象/数组 |

### 9.2 信任边界（先读这一段）

同源主 realm 里的 JS 能读 DOM、CSRF token、其他插件内存，也能直接 `fetch('/api/...')`。这不是可以靠 caps 修好的问题：

- **caps 不是 UI 沙箱**。Go 的 `caps.Broker` 只做「插件 + 能力名 + 信任等级」的粗粒度授权；现有实现**不使用 target**，`ui` 也没有调用点。文档不再声称 `target=action:<id>` 能拦住越权。
- `builtin`/`official`/`verified` 在主 realm 运行时**同权**；差别是来源与审核，不是运行时权限。
- 同域插件可以冒充任何插件 id、读任何 bootstrap token、代答 `ui/request`。所以：
  - 动作归属由**服务端按 action id 反查属主**，不信任请求体里的 plugin id；
  - bootstrap 为每个激活插件发一个 `activationToken`（绑定插件 id + 组件摘要 + nonce），请求带它做归因；同域脚本仍能读到它，所以它**不防恶意**，只防误用与错配；
  - UI 确认框不能作为安全审批；安全审批仍走 Go 的 `approval` seam（CLI/宿主原生/独立通道），`ui/request` 只做 UX，且超时/断连即拒绝；
  - **Web 宿主没有独立审批通道时，需要审批的动作一律 fail-closed 拒绝**；不得用同域 Dialog 冒充审批；
  - SSE 命名空间过滤只降噪，不是隔离。
- `third-party`（含签名但未审核）不得进主 realm 执行自定义功能包；只能 `render: "host"` 的声明式功能包（宿主渲染），或 iframe 隔离岛（U2）。
- 自定义功能包是对主设计 §2.2「插件不画 UI」的**有意反转**，必须走 [ADR 0006](adr/0006-custom-ui-components.md)；默认路线仍是声明式功能包。

### 9.3 信任矩阵

| trust | 自定义功能包 | 声明式功能包 | 平台 UI 调用 | Go 后端 caps |
|---|---|---|---|---|
| `builtin` | 是 | 是 | 按 `ui` | 按 caps |
| `official` | 是 | 是 | 按 `ui` | 按 caps |
| `verified` | 是 | 是 | 按 `ui` | 按 caps（exec/process 仍受信任等级限制） |
| `third-party`（签名） | **否** | 是 | 无（`ui` 最低信任是 `verified`） | 按 caps（fs.read/secrets） |
| `third-party`（未签名） | **否** | 是 | 无 | 无（只能声明式） |

声明式功能包由**宿主**执行动作并渲染，功能包本身不获得 `ui` 桥；所以第三方仍能提供只读页面，但不能自己驱动 UI 或调用动作。U2 的 iframe 隔离岛是第三方的第三条路径（默认关闭，不进主 realm），矩阵里的「否」指主 realm 自定义功能包（`.aui`）。**矩阵中的「平台 UI 调用 / Go 后端 caps」只描述服务端检查，不构成对主 realm 的沙箱**（§9.2）。**自定义 SFC 的 trust 只读 install 索引/keyring，不读清单自述**；如 copy 一个目录并改 `trust:builtin`，仍按 `third-party` 处理。

**开发例外**：`ada ui dev` 可以在 loopback 上以 `--allow-local-ui` 临时把未签名本地插件当 `verified` 运行，仅用于开发；启动横幅、Inspector 与审计都标记「未签名 UI」，`ada serve`/生产宿主不提供该开关。它不改变安装信任等级，也不写回清单。

### 9.4 隔离模式（U2 原型）

- `<iframe sandbox="allow-scripts">` + 独立 origin（不复用宿主 origin），只通过结构化消息 RPC。
- 宿主给岛注入：主题变量、语言、只读上下文；岛不能读宿主 DOM、cookie、CSRF。
- 父侧对每条 RPC 做 caps 检查与审计；岛的消息必须带一次性会话 key（父生成，iframe 无法读其他岛）。
- 代价：无法共享弹层/焦点/尺寸；组件只能渲染在自己的矩形内。因此它是**降级路径**，不是默认。

### 9.5 能力桥

| 方向 | 入口 | 服务端检查 |
|---|---|---|
| UI → Go 动作 | `POST /api/ui/action` | 按 action id 查属主（`ui:true`）+ 调用方 `ui.actions`（防误用）+ caps `ui` + schema + 审计；**不信任请求里的 plugin id** |
| UI → 平台 UI | `POST /api/ui/call`（notify/dialog/clipboard/openExternal） | caps `ui` + 宿主线程执行；同域插件可绕过，只做归因 |
| Go → UI 请求 | SSE `ui/request` + `POST /api/ui/response` | 一次性 id、30 s 超时、断连即拒绝；**UX 通道，不是审批门禁** |
| Go → UI 事件 | SSE `ui/event` | 默认按插件命名空间推送；过滤只降噪，不是隔离 |
| UI → 存储 | `ctx.storage`（命名空间 = 插件 id） | 命名空间强制；Go 侧限制键前缀、单值大小、总量；不落 cookie |

新增能力点必须同时更新主设计 §13 与 `caps.md`（AGENTS §7）。主设计 §13.3 里的 `ui.*` 是「平台 UI 方法族」的展示，不是 caps 语法；caps.md 只认 `ui` 这一项。这里也**不新增** `ui.action.*` 子能力名：UI 是「一个能力 + 清单白名单」，动作的细粒度由服务端的 action 属主表与审计承担，而不是 caps 的 target。

动作与事件载荷 ≤ 256 KiB（AGENTS §5.6）；大对象（图片、文件、日志包）返回 URL/句柄，由宿主按权限供给，不塞进 JSON。

### 9.6 CSP 与响应头

hosted 模式（最严）：

```
default-src 'self';
script-src 'self' 'nonce-<per-request>';
style-src 'self';
img-src 'self' data:;
font-src 'self';
connect-src 'self';
frame-src 'none';
frame-ancestors 'none';
base-uri 'none';
object-src 'none';
form-action 'self';
```

- 无 `'unsafe-inline'`、无 `'unsafe-eval'`：样式走 `<link>` 文件，文本走 `textContent`，事件走 `addEventListener`。
- **import map 是内联脚本**：用每请求 nonce 放行（`script-src 'self' 'nonce-<random>'`），index.html 本身 `no-store`。任何时候都不用 `'unsafe-inline'`。
- `require-trusted-types-for 'script'` 作为 U2 目标：先验证动态 `import()`、import map 与 Trusted Types 的兼容性；不兼容就不启用，不为了「更安全」的声明牺牲可运行性。
- 其他头：`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、`Cross-Origin-Opener-Policy: same-origin`、`Cross-Origin-Resource-Policy: same-origin`。
- U2 启用 iframe 隔离岛时，把 `frame-src` 放宽到岛的独立 origin（仍保持 `frame-ancestors 'none'`）；这是一次单独的安全评审，不随插件安装自动放宽。

### 9.7 输入与样式

- 文本：转义 `& < > " '`；属性值转义；URL 属性只允许相对路径与 `http(s)`/`mailto`；拒绝 `javascript:`/`data:`（图片除外，且校验 MIME）。
- 事件名白名单；`a-on:` 之外的 `on*` 属性编译期拒绝。
- CSS：禁止 `@import`、外部 `url()`、`behavior`、`expression`；选择器必须带作用域；`position: fixed` 只允许标准组件（lint）。模板禁止 `style` 属性与 `a-bind:style`，避免内联样式逼迫 CSP 放宽。
- 表单/粘贴内容不自动变成 HTML；富文本走声明式结构化节点（U3 再议）。

### 9.8 secret 与配置

- 前端拿到的 config 只含公开字段；Go 在 bootstrap 前按 `x-secret` 剔除，Inspector 也不显示。
- 日志/事件/错误上报统一走 `fs-redact`/`shell-redact` 同类规则，禁止 token 出现在 Plan、URL、DOM。
- CSRF token 只用于同源写请求；不放 localStorage；刷新页面重新发放。

---

## 10. 声明式功能包（宿主渲染）

一个功能包可以没有 `.aui`，只声明页面数据；这仍是「一项功能」，只是实现方式为**宿主渲染**：**插件产数据、宿主画界面**（AGENTS #12）。它也是第三方/未签名插件唯一可用的功能形态。

清单形态：`ui.package="."`、`ui.components=[]`、`ui.entry=""`，`contributes.pages` **恰好一个**；页面数据写进 `ui.manifest.json`（或它引用的数据文件），因此仍有 `ui.digest`，仍走同一签名/安装管线。

```jsonc
{
  "slug": "files",
  "title": "文件",
  "icon": "folder",
  "sections": [
    { "title": "工作区", "items": [
      { "type": "text", "label": "根目录", "value": "/workspace" },
      { "type": "list", "action": "files.list", "params": { "path": "/workspace" },
        "item": { "title": "{{ name }}", "subtitle": "{{ path }}", "action": "files.open" } },
      { "type": "actions", "actions": ["files.refresh"] }
    ]}
  ]
}
```

| 规则 | 说明 |
|---|---|
| 只允许 `text/list/table/form/actions/empty` | 标准组件覆盖 90% 场景；新类型要设计评审 |
| 数据只来自声明过的 `actions` | 每个 action 有 input schema，Go 执行，cap 检查，结果可缓存；只允许本插件声明且 `ui:true` 的动作，或 preset 明确共享的宿主动作 |
| 模板只允许 `{{ field }}` 单层字段 | 不执行表达式、不访问原型；缺失字段显示空 |
| 布局由宿主决定 | 插件只给结构，不能指定像素/颜色/字体 |
| 不可用 JS | 未签名插件也能安全提供页面；文案字段支持 i18n key，由宿主按当前 locale 解析 |
| 错误可降级 | action 失败显示该项错误，不影响其他 section |
| 一个包一个 page | 声明式包也只贡献一个顶层 page；更多页面拆成更多功能包 |

---

## 11. host/web 适配器

### 11.1 路由

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | `index.html`（no-store；每请求 CSP nonce；加载占位；内联 import map 带 nonce） |
| GET | `/app/*` | 运行时/标准组件/Shell 资产；immutable |
| GET | `/plugins/*` | 已装插件资产；只服务 root 之下；immutable；摘要路径 |
| GET | `/api/ui/bootstrap` | UI Plan + CSRF（no-store） |
| POST | `/api/ui/action` | 动作调用；CSRF；caps；审计 |
| POST | `/api/ui/call` | 平台 UI 调用（notify/dialog/...） |
| POST | `/api/ui/response` | 宿主请求的应答 |
| GET | `/api/ui/events` | SSE |
| GET | `/api/ui/health` | 健康（插件状态、版本、摘要） |
| GET | `*`（Accept: text/html） | SPA 深链回退；`/api`、`/app`、`/plugins` 除外 |

SSE 由 `fetch` + `ReadableStream` 解析（可带 CSRF/Authorization 头），不用 `EventSource`；重连与 `Last-Event-ID` 由 Transport 维护。这样浏览器与 WebView2 Bind / Electron IPC 可以共用同一套订阅语义。

### 11.2 bootstrap

```jsonc
{
  "schema": 1,
  "ui": {
    "apiVersion": 1,
    "basePath": "/",
    "locale": "zh-CN",
    "theme": "ada-light",
    "runtime": "/app/ui.<sha256>.js",
    "csrf": "…"
  },
  "preset": "designer",
  "plugins": [
    {
      "id": "hello-panel", "version": "0.1.0", "trust": "official",
      "activationToken": "…", // 绑定 id + 组件摘要 + nonce；仅归因/防误用
      "entry": "/plugins/hello-panel@0.1.0/ui/entry.module.js", // 可为 null
      "manifest": "/plugins/hello-panel@0.1.0/ui/ui.manifest.json",
      "components": [
        {
          "name": "Hello",
          "module": "/plugins/hello-panel@0.1.0/ui/Hello.module.js",
          "plan": "/plugins/hello-panel@0.1.0/ui/Hello.plan.json",
          "css": "/plugins/hello-panel@0.1.0/ui/Hello.css"
        }
      ],
      "caps": ["ui"]
    }
  ],
  "contributions": { "pages": [], "slots": [], "commands": [], "actions": [] },
  "lock": { "digest": "sha256:…" }
}
```

- 运行时先拉 `ui.manifest.json`，按文件表校验每个模块/Plan/CSS 的 sha256，再 `import`；`lock.digest` 覆盖整个 UI Plan 快照，用于 SSE 增量对齐。
- `bootstrap` 由 Go 从已装插件汇总，**不在浏览器里扫目录**；顺序在这里已拓扑排序。
- 不含 secret；不含插件源码；只含已校验产物的 URL 与摘要。
- 认证：session cookie（HttpOnly/Secure/SameSite=Lax；写请求必须带 CSRF header）+ dev token；desktop 走进程内绑定，不走 HTTP。Lax 保证外部深链进来仍带会话，写操作另有 CSRF。

### 11.3 桌面（WebView 壳，不做 native UI）

**产品前提**：桌面 = 一个 WebView 窗口加载同一份 SPA；不使用 mygo native 控件，也不把页面渲染成原生控件。现有 `host/desktop` 的 Renderer/View 骨架（native 视图映射）不用于本设计；桌面适配器改成 **WebView Transport**。

| 路线 | 壳 | Node | 优点 | 代价 |
|---|---|---|---|---|
| **A. mygo WebView2**（首选） | Go + mygo WebView2 窗口 | 无 | 零 Node；Go 直接 Bind/事件；单二进制 | 依赖 WebView2 运行时；跨平台矩阵待验证 |
| B. Electron | Electron main + renderer | 有（仅壳） | 跨平台、调试成熟 | 把 Node 带进桌面宿主；Go 内核需 sidecar/IPC；破坏「绝对零 Node」 |

规则：

- 两条路线共用 `Transport`（`call/subscribe/respond`）：A 用 mygo Bind + 类型化事件；B 用 preload/contextBridge + IPC。renderer 永远 `nodeIntegration=false`、`contextIsolation=true`、`sandbox=true`。
- A 的 Go 内核与 WebView2 同进程；B 的 Go 内核作为本地 sidecar（localhost + 随机 token），renderer 不直接碰 Node API。
- 页面、插槽、组件与浏览器**同一份产物**；`contributes.pages` 不再映射成 mygo native 控件。菜单/托盘/文件对话框等系统能力由壳提供，走 Transport `call`，与 UI 插件同一套 gate。
- 「零 Node」的强口径只覆盖构建、插件编译/执行与 Web 宿主；**Electron 若选，必须明确接受「Node 只在桌面壳」的例外**。要求绝对零 Node 时只能选 A。
- ADR 0001 的「不引入 mygo 原生窗口」仍成立；但 mygo WebView2 仍是新依赖，U3 前需复议/依赖 ADR。桌面不作为 U0–U2 验收项。

### 11.4 日志与审计

- 动作调用、拒绝、确认对话框往返、插件装配/停用/更新、摘要不符 → 统一 audit JSONL（复用 `policy/audit-log`）。确认对话框是 UX，审计不把它当审批结论。
- 前端错误上报带 `traceID`、插件 id、组件 id、build 号；不含用户数据正文。
- 指标：boot 时长、激活时长、渲染时长、内存、失败率。

---

## 12. 开发工具链（无 Node）

| 命令 | 作用 |
|---|---|
| `ada ui new <name>` | 生成单组件功能包（`plugin.json` + 一个 `.aui`）；`--package` 生成多组件包（`ui/` 目录） |
| `ada ui dev` | 监听 `.aui`/清单 → Go 编译 → 内存供给 → SSE 热重载（整页刷新为 U1，组件级 HMR 为 U2） |
| `ada ui build` | 编译到 `dist/ui` + `ui.manifest.json`，供安装/签名 |
| `ada ui lint` | 结构/表达式/样式/a11y/令牌/安全规则 |
| `ada ui test` | Go golden + 运行时一致性（op trace，§14） |
| `ada ui inspect` | 打印 UI Plan、组件依赖、激活状态；浏览器 Inspector 是同一数据的可视化 |
| `ada web` | 长驻 Web 宿主：bootstrap/资产/动作/SSE/内容寻址资产库；与只读 `ada serve` 分开（U2） |

- 新增 `ada web` / `ada ui` 子命令的**参数解析与 help 放 `cmd/ada`**（≤ 200 行），业务在 `host/web`/`ui/`；`host/cli` 不增长，避免 ADR 0002 的 1500 行例外再破。
- 没有 `npm run`、没有 `vite.config`、没有 `node_modules`；`package.json` 只有 U3 依赖声明时才出现，且只被 Go 读取。
- **TypeScript 只做类型擦除，不做类型检查**（无 Node 不引入 tsc）；类型错误由编辑器与评审承担，`ada ui lint` 只查语法、规则与产物。这是无 Node 的明确代价，不假装有 `tsc`。
- `ada ui dev` 的 `--allow-local-ui` 只用于 loopback 开发（§9.3 的开发例外），不进入 `ada serve` 与生产安装流程。
- 格式化：`<script>` 用 esbuild 的 pretty 输出作为规范形；模板/CSS 由 `ada ui fmt`（U2）统一。
- 编辑器：提供 `plugin.json` JSON Schema 与 `.aui` 语法说明；不维护 LSP（U3 再议）。

---

## 13. 性能与体积预算

| 指标 | 预算 | 测法 |
|---|---|---|
| 运行时内核（gzip） | ≤ 35 KiB | esbuild minify + gzip |
| 标准组件（gzip） | ≤ 60 KiB | 同上 |
| Shell（gzip） | ≤ 20 KiB | 同上 |
| 首屏（本地，p95） | ≤ 150 ms | 浏览器冒烟记录；U3 用 chromedp 复测，不作为 U0–U2 CI 门禁 |
| 插件激活 | ≤ 10 ms | apply + 注册，不含网络；Go 计时 |
| 路由切换 | ≤ 20 ms | 已加载组件、无数据请求；Go/OpAdapter 计时 |
| 远程更新切换 | ≤ 50 ms | staging + swap，不含下载；Go 计时 |
| 编译 1 组件 | ≤ 50 ms | Go benchmark |
| 编译 50 组件 | ≤ 1 s | Go benchmark，并行安全（每组件独立） |
| 100 次激活/停用 | 无持续增长 | 注册表/监听器/样式计数（CI）+ JS heap 快照（U3 浏览器） |

浏览器侧的首屏 p95 与内存不是「无 Node」下可自动化的门禁：U0–U2 只记录冒烟数据，U3 用 `chromedp`（新依赖 ADR）或人工复测。不拿不可测的数字当验收。

优化顺序：Plan 体积（去空白/短键）→ 模块数量（按组件拆分，按需加载）→ 绑定粒度（只建动态槽）→ 调度（批量 flush）→ 缓存（摘要 URL）。

---

## 14. 测试矩阵

| 层 | 方法 | 门禁 |
|---|---|---|
| 编译 golden | 每个语法点/错误一个 fixture；输入 `.aui` → 期望 Plan/CSS/诊断 | 字节级 diff 失败即失败 |
| 表达式 | Go 只测 parser/AST/诊断 + JS harness 用同一 fixture 测求值；运算符/路径/调用/错误/步数上限 | 覆盖 ≥ 90% 分支，无第二求值器 |
| 样式 | 作用域改写、非法选择器、`@import`、`@font-face`、外部 `url()`；`@media` 内改写 | 每条规则一个反例 |
| 运行时一致性 | 渲染核心只生成 §6.3 的 Op 词表；测试用 OpAdapter 记录并断言操作与顺序；真实 DOM 由浏览器适配器执行，少量高风险用例浏览器冒烟 | Op 序列 golden + 状态断言 |
| 泄漏 | 100 次激活/停用；监听器/样式/路由/定时器计数 | 回落到 ±5% |
| 桥接拒绝矩阵 | 动作 × cap × trust × CSRF；未声明 action；超时；断连 | 全部拒绝且落审计 |
| 资产/深链 | 路径逃逸、符号链接、404 不返回 index、immutable 缓存 | httptest |
| 远程 | 签名错、摘要错、版本回滚、staging 失败、断网 | 旧版本仍在服务；断网时已加载页面继续 |
| 安全 | XSS 载荷、`javascript:` URL、原型污染键、CSP 违规 | 全部拒绝/转义 |
| a11y | 规则用例 + 路由焦点 | 0 error（warning 可配额） |
| 性能 | 上述预算的基准 | 回归 ±15% 报警 |

**如何在没有 Node 的情况下跑运行时测试**：

- 运行时核心只依赖 `DomAdapter` 接口；浏览器实现操作真 DOM，测试实现是 **OpAdapter**：把操作记成有序日志（约 100 行），断言结构/属性/文本/顺序，不依赖真实 DOM。
- JS 引擎不在 U0 假设「jsrt 现成可用」：`jsrt` 的 Host 目前没有 DOM/global 注入点，`Runtime` 也只吃 `compile.Artifact`。U0 先用 1 周 spike 验证「moejs 直接跑 runtime core + OpAdapter」是否可行；不可行则用 `chromedp` 跑同一套 JS（新依赖 ADR，仍不引 Node），或把运行时核心拆成不依赖 DOM 的纯函数在 Go 侧测。
- 表达式：Go 侧只测 parser/AST/诊断；求值用 JS harness 的同一份 fixture，避免「Go 求值器 + JS 求值器」双实现。
- 这是 U0 的第一道 gate；结论写入 [spikes.md](spikes.md)（新增 UI 运行时 harness 条目），U0 第 1 周必须给出「moejs 直跑 / chromedp / 纯 Go 拆分」三选一。没有定论不写 `ui/runtime` 的其它部分。

---

## 15. 目录结构与预算

```
ui/                          Go 编译器（基础设施）
  compile.go                 切块、模板、样式、产物
  expr/                      受限表达式（AST/求值语义）
  template/                  tokenizer + parser + 指令
  style/                     作用域改写与规则检查
  validate/                  a11y/安全/导入/令牌
  runtime/                   浏览器运行时（纯 ESM；Go 侧只做 embed 与测试 harness）
    assets.go                //go:embed *.js + components/*.js；Assets() 供 host/web 供给
    core.js                  作用域/服务/effect/调度
    dom.js                   DomAdapter（浏览器）
    plan.js                  Plan 解释器
    router.js
    i18n.js
    bridge.js
    components/              标准组件（按需拆分）
    op-adapter.js            测试用操作序列适配器（OpAdapter）
    harness_test.go          引擎/harness 一致性测试（仅测试编译，不进二进制）
  stdlib/                    默认标准组件清单/样式（与 components 分离）
host/web/                    宿主适配器（Go）
  server.go                  路由/静态/SSE/深链
  bootstrap.go               UI Plan 汇总
  action.go                  动作 RPC + caps
  assets.go                  内容寻址资产库与 GC
host/desktop/                WebView Transport（WebView2 Bind / Electron IPC），U3 条件项
plugins/ui/                  内置 UI 插件
  shell/  settings/  inspector/
presets/                     ui 段（shell/home/theme/locale）
```

预算：

| 范围 | 预算 | 超了 |
|---|---|---|
| `ui/`（Go） | ≤ 5000 行（compiler 1600 / expr 800 / template 900 / style 1200 / validate 500） | 拆包；仍超则 ADR;guard 新增规则 |
| `host/web` | ≤ 800 行（host 总预算 3600，见 ADR 0007） | 拆 `bootstrap`/`action`/`assets` 子文件 |
| 运行时内核 | ≤ 35 KiB gzip | 先砍非必需 API |
| 单个插件 | ≤ 800 行（Go）/ 1 个 `.aui`（UI） | 拆组件 |

---

## 16. 里程碑

**U0 的第 0 个提交（评审通过后，不写功能代码）**：先接受 ADR 0005/0006/0007/0008，并同步：AGENTS §2.2 基础设施加入 `ui/`、§4 #12 改为「默认声明式 + ADR 0006 受控例外」、§5.3 采用 3600/900/5000/35KiB、§5.4 加 `plugins/ui/`、Electron 的 Node 例外写进 §1/§10；主设计 §2.2/§14.3 改为 WebView 壳与受控组件；`install.md` 加 `kind=ui`/`ui.digest`/sigDigest；`guard.ps1` 加 `ui/` 行数、体积、`eval` 文本检查。没有这个提交，不写 `ui/` 或 `host/web` 的代码。

| 阶段 | 内容 | 验收（做到才算完） |
|---|---|---|
| **U0** 竖切（4 周） | 编译器 MVP（template/if/for/on/model/style）；运行时（挂载/更新/事件/条件/列表）；`host/web` 静态+深链；1 个 `.aui` 页面；OpAdapter + JS 引擎 spike；guard 加 `ui/` 规则 | 无 Node 机器上：`go test ./ui/... ./host/web/...` 全绿；浏览器打开 `/` 渲染计数器；每个指令/错误码至少一个 fixture；静态 grep 无 `eval`/`new Function` |
| **U1** 可用（4 周） | 路由/作用域/服务/inject/effect；Shell + 标准组件 12 个；i18n/主题；单组件/多组件功能包；声明式功能包；`ada ui dev/build/lint`；本地开发插件（`--allow-local-ui`） | 本地 3 个功能包（含 1 个多组件包）在不重启下出现；停用无残留；路由深链/后退正确；a11y 0 error；无远程安装、无跨插件动作 |
| **U2** 可信（4 周） | 远程包签名/trust/摘要；内容寻址资产库与长驻宿主；动作 RPC + 服务端属主表 + 审计；staging 更新；隔离 iframe 原型；Inspector | 签名/摘要/回滚/断网四类用例全绿；staging 失败旧版继续；第三方只能声明式或隔离；安装后 10 秒内导航出现 |
| **U3** 产品化（2 周） | 性能与体积达标；文档/示例/接入说明；桌面 WebView2/Electron 壳（仅 ADR 0001/0008 复议后）；npm 依赖内联（仅 U2 评估通过） | 预算达标；文档示例进 CI；未完成项显式标注，不塞进验收 |

每阶段都带「不许做」：

- U0：不许做路由、不许做远程安装、不许做跨插件动作、不许做 iframe。
- U1：不许做远程安装/签名、不许做第三方自定义 UI、不许做插件自绘全局 chrome。
- U2：不许把信任判定放进 JS、不许用 UI 确认框替代 Go 审批、不许绕过服务端动作属主表。
- U3：不许为性能把业务循环搬进运行时内核；桌面与 npm 没有新 ADR/评估就不做。

---

## 17. 风险与待决

| 风险 | 影响 | 对策 |
|---|---|---|
| Go 写 HTML/CSS 编译器工作量大 | 进度 | 受控语法（白名单）、不做通用 HTML；U0 只做最小子集；golden 先行 |
| Plan 解释器比编译 JS 慢 | 体验 | 只建动态槽；基准进 CI；必要时把热点槽降为专用闭包（仍不 eval） |
| 无 Node 生态，作者工具少 | 采用 | Go 内联 npm 依赖；提供最小 lint/fmt；示例仓库 |
| 同域信任被误解为沙箱 | 安全 | 安装确认明示「与宿主同权」；third-party 默认不进主 realm |
| CSS 隔离不彻底 | 视觉 | 每元素作用域属性 + 令牌 + 规则白名单；快照测试 |
| WebView2 / Electron 能力差异 | 桌面 | Transport 抽象；U3 前先做能力探测；history/import map 不满足时降级 hash；桌面受 ADR 0001/0008 约束 |
| host/web 预算与长驻宿主 | 交付 | ADR 0007 提高总预算并拆包；`ada web` 是长驻宿主，安装通知走事件/reload API，不 sleep 轮询 |
| 同域审批不可约束 | 安全 | 明确「审批不能靠同域 UI」；安全审批走 Go approval/宿主原生/独立通道；UI 确认只是 UX |
| 双运行时（Go 插件/UI 插件）概念混淆 | 维护 | 一个 manifest、一个 id；文档明确「后端 apply / 前端 apply」；Inspector 同屏展示 |

待决问题：

1. mygo WebView2 与 Electron 是否支持 import map、History API、SSE/流式 IPC；不支持时的最小降级集。
2. `ada web` 的长驻生命周期与安装通知方式（同进程事件、admin reload API、还是 `ada web` 子命令）。
3. npm 依赖内联放在 U2 评估、U3 落地，还是只支持相对导入。
4. 隔离 iframe 的产品价值是否足以抵消焦点/弹层/尺寸的复杂度。
5. CSS 是否强制「只用令牌」；强制会挡住品牌页面。
6. 路由守卫是否允许异步（当前设计：不允许）。
7. 组件级 HMR 的收益是否值得引入状态迁移复杂度。
8. 声明式功能包的字段类型是否足够（先 6 种，按真实插件扩展）。
9. 服务端动作属主表与 `ui.actions` 的存储/热更新方式（清单静态表还是运行期注册）。
10. 多组件功能包的边界：包内组件复用/状态共享是否需要显式 `ctx.store` 约定，还是允许自由 import。

---

## 附录 A：错误码

| 码 | 含义 |
|---|---|
| `EUISTRUCT` | `.aui` 结构错误（缺块/重复块/根不是元素） |
| `EUITEMPLATE` | 指令/属性/嵌套错误 |
| `EUIEXPR` | 表达式语法或非法调用 |
| `EUISTYLE` | 样式规则被拒 |
| `EUISECURITY` | 禁用标签/属性/代码形态 |
| `EUIIMPORT` | 导入不被允许 |
| `EUIA11Y` | 可访问性检查失败 |
| `EUIARTIFACT` | 产物摘要/结构/planVersion 错误 |
| `EUIAPIVERSION` | `ui.apiVersion` 与运行时不等 |
| `EUINTEGRITY` | 远程资产摘要/签名不符 |
| `EUIPLUGIN` | 插件激活/停用失败 |
| `EUISERVICE` | 必需服务缺失（保持 `pending`） |
| `EUIREACTIVE` | 响应式循环/求值深度超限 |
| `EUIRENDER` | 渲染期异常（进错误边界） |
| `EUIBRIDGE` | 桥接调用被拒/超时 |
| `EUIQUARANTINE` | 插件被隔离 |

## 附录 B：对 AGENTS 12 问的自检

| # | 问题 | 回答 |
|---|---|---|
| 1 | 属于哪一层？ | Go 编译/运行时 = L2 基础设施；host/web = 宿主适配器；Shell/页面 = UI 插件 |
| 2 | 删掉它另一个产品还能跑？ | 删掉任一 UI 插件，SPA 仍启动；删掉运行时，UI 整体不存在，但 Go 内核/CLI 不受影响 |
| 3 | 只做一件事？ | 编译、运行时、host/web、插件各自单一职责 |
| 4 | kernel 新增 agent 词汇？ | 无；`kernel/` 零改动 |
| 5 | 注册返回 Dispose 且在 effect？ | 前端 `ctx.effect` 同语义；后端沿用 kernel |
| 6 | 安全路径唯一？ | 动作门禁在 Go 的服务端属主表与 approval seam；caps 只是粗粒度授权；同域 UI 代码本身不是可沙箱对象（§9.2） |
| 7 | 事件被当门禁？ | 否；SSE 只通知，装配由签名/trust 决定；命名空间过滤只降噪 |
| 8 | 依赖用 inject？ | 是；缺失 → pending，不静默 nil |
| 9 | 产品差异来自 preset？ | 是；`ui.shell/home/theme/locale` + 插件列表 |
| 10 | 契约测试/golden？ | 编译 golden、运行时一致性、拒绝矩阵、摘要/签名 |
| 11 | 文档与示例同步？ | 本文件 + U0 起进 CI 的 fixture |
| 12 | 临时分支/TODO 后门？ | 无；待决问题显式列出，不用隐藏开关 |

**两处显式反转**：自定义 UI（ADR 0006）与 host 预算（ADR 0007）都不沿用旧口径；没有 ADR 编号就不算默认决定。

## 附录 C：与 v1 的 Page/Section/Item

v1 的 `Page/Section/Item` 契约在本设计中保留为**声明式页面**（§10）：插件给数据、宿主渲染。差异：

| v1 | 本设计 |
|---|---|
| 页面由 mygo 原生渲染 | 同一 schema 由 SPA 宿主渲染；桌面 WebView2/Electron 加载同一份 SPA，不再映射成原生控件 |
| 前端插槽挂组件 | 功能包的组件先编译为 Plan，再按插槽注册；包内组件私有 |
| 无远程 | 远程安装/验签/锁定，功能包整体同源供给 |
| 无 Node 编译 | Go 编译功能包（单组件/多组件），浏览器只执行 Plan |

自定义组件不是 v1 的回归，而是受控例外（[ADR 0006](adr/0006-custom-ui-components.md)）：仅 `verified` 及以上，且只替换**内容区**；导航、页头、空态、错误态、确认框由宿主或标准组件渲染，插件不能替换全局 chrome。页面默认仍应走声明式，保证「插件不画 UI」的主路线不被悄悄替换。

## 附录 D：落地要改的契约（清单）

### D.1 U0 前置（现有代码本来就缺，不是 UI 新增）

| 位置 | 缺口 | 处理 |
|---|---|---|
| `plugin.Manifest` | 没有 `APIVersion`/`Contributes`；`Kind` 只接受 go/ts；`DisallowUnknownFields` 会拒绝新清单 | 先补字段/类型与校验；旧宿主遇到新清单必须显式报版本错，不能静默 |
| `loader` | 未知 `kind` 直接失败 | 支持 `kind=ui`；无后端 `entry` 的插件允许加载 |
| `install` | `VerifyDir` 只 `switch ts/go`，`kind=ui` 不算 digest、不设 trust；签名只对 `ts` 生效 | 加 `kind=ui` 分支；`ui.digest` 进签名消息；远程包拒绝现场编译 |
| `contrib` | 目录/Registry/Page/Action 类型不存在 | 按主设计 §9.4 先落最小类型，再谈 UI 贡献 |
| `apiVersion` | 主设计 §9.2 有，代码未实现 | U0 补齐并写版本门禁 |
| `caps` | `ui` 无调用点；`Authorize` 不使用 target | 不假装 target 能拦动作；`ui` 的调用点 U2 才落 |
| **trust 单一来源** | install 得到 verified/official 后没有灌进 Broker；清单自述可冒充 | 建 `trust` 服务：install 索引/keyring → loader → bootstrap；UI 的 SFC 准入只读它。用例：复制目录并自述 `trust:builtin` 必须降为 `third-party` 且不能跑自定义组件 |
| 命名 | `ada:ui` 已被主设计 §11.6 用作插件面向的宿主能力模块 | SPA 运行时改名 `ada:surface`；`ada:ui` 留给插件能力 |

### D.2 UI 新增

| 位置 | 变更 | 类型 |
|---|---|---|
| `plugin.Manifest` | 新增 `UI *UISpec`；`Kind` 接受 `"ui"`；`contributes.pages` 增加 `component`/`order`；`contributes.actions` 增加 `ui` | 枚举 + 字段扩展 |
| `plugin.json` schema | 新增 `ui` 段：`apiVersion/package/entry/components/inject/provide/actions/digest`；一个插件最多一个包；`caps` 仍只用 `ui`；顶层 `inject` 只属于 Go 侧 | 兼容 |
| `idPattern` 等校验 | `ui.entry` 必须是插件目录内相对路径；组件名唯一；入口/组件路径存在 | 收紧 |
| `install` | 远程包校验预编译后端 Artifact（若有）与 `dist/ui`/`ui.digest`；本地目录可现场编译；`install.md` §2/§3 的签名公式改为 `manifestJSON + sigDigest`（全栈 = `sha256(backendDigest + ui.digest)`） | 新增步骤 + 契约更新 |
| `assemble`/preset | preset 增加 `ui` 段；UI 插件沿用 `ui.inject` 拓扑序 | 新增数据字段 |
| `host/web` | 新宿主适配器：bootstrap/静态/深链/动作/SSE/内容寻址资产库；复用 `host/server` 的路径安全规则 | 新增（ADR 0007） |
| `host/desktop` | 改为 WebView Transport（mygo WebView2 Bind 或 Electron preload/IPC），不做 native Renderer/View；受 ADR 0001/0008 约束 | 新增 |
| `plugins/seam/ui` | Go→UI 的 Request/Notify 契约；当前不存在，U2 才需要 | 新增 seam（未实现） |
| `observe`/audit | 增加 UI 事件与动作审计字段 | 新增 |
| `scripts/guard.ps1` | 新增 `ui/` 行数、运行时体积、`eval` 文本检查；host 总预算 3600 | 门禁 |
| 文档 | 主设计 §2.2/§3.1/§9.1/§9.2/§9.4/§13/§14、`caps.md`、AGENTS §5.3/§5.4、README | 同步更新 |

**不改**：`kernel/`、`jsrt/`、`compile/` 的既有接口、`expr/`（flow 专用）、现有 `caps` 语法。UI 编译器是新增的 L2 包，测试与预算独立。

## 附录 E：评审时最该被质疑的 6 个点

1. **Go 编译器是否值得**：如果不做 Go 编译，就必须 Node 构建或浏览器 `eval`；二者都违反硬约束。代价是模板语法受控、生态工具少。
2. **Plan 解释器性能**：用绑定级 effect 与基准兜底；若某类页面不达标，优化解释器或把该槽降为专用闭包，仍不生成/执行任意代码。
3. **同域信任**：设计没有假装 JS 沙箱；如果评审要求「第三方自定义 UI 也安全」，答案只能是 iframe 隔离，成本与体验代价要单列。
4. **`kind: "ui"` 是否必要**：也可以让所有 UI 插件都挂 `kind: "ts"` 的空后端；独立 kind 让安装与清单检查更明确，代价是枚举扩展。
5. **static 分发**：不是本轮目标；若将来要做，必须单独 spec（同源预编译、无 `blob:`、无运行期验签、无 actions），不能顺手加进 hosted 的实现。
6. **组件/包与插件清单两文件**：组件文件是 UI 实现粒度，功能包是签名/安装粒度；清单必须独立存在，才能在不上代码的情况下静态审计贡献。
