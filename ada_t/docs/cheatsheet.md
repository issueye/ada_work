# Ada 组件速查卡

> 面向 `.aui` 组件作者的完整参考。架构与设计决策见 [README.md](../README.md)。

## 文件结构

```aui
<meta>    { "name": "组件名" }                     可选(缺省用文件名)
<template>  模板(单根元素)                        必需
<script lang="ts">  default export { setup }      可选
<style scoped>  .class { 属性: 值 }               可选
<i18n>    { "zh-CN": { key: 文案 } }               可选
```

一个目录 = 一个组件包;多组件时入口用 `-entry` 指定或 Options.Entry。
组件名不能与内置控件重名(重名按内置控件处理)。

## 模板指令

| 写法 | 含义 |
|---|---|
| `{{ expr }}` | 插值(转义;表达式见下) |
| `a-if="cond"` / `a-else-if` / `a-else` | 条件链(必须相邻兄弟) |
| `a-for="item, i in list"` + `a-key="item.id"` | 列表(a-key 建议提供;当前只用于缺失告警,实例按渲染路径复用) |
| `a-on:click="handler"` | 事件:裸名收 `$event` 为第一参 |
| `a-on:click="pick(item)"` | 带参调用 |
| `a-on:submit="send"` | TextInput 回车(仅 TextInput) |
| `a-model="state.x"` | 双向绑定(见各控件) |
| `a-bind:gap="state.gap"` | 动态属性 |
| `a-class:active="state.on"` | 动态 class(配样式令牌) |
| `a-prop:label="state.label"` | 子组件 props |

约定:`a-key` 只能与 `a-for` 一起使用;组件标签只接受静态 prop 与 `a-prop:`/`a-on:`
(组件上的 `a-html`/`a-ref`/`on*` 会被拒绝);动态 class 只作用于原生控件。

静态文本与静态属性支持 HTML 实体:`&amp;` `&lt;` `&gt;` `&quot;` `&apos;` `&nbsp;`,
以及数字实体 `&#65;` / `&#x1F600;`;表达式与指令值内不解码。

## 表达式

字面量 · 路径/下标 · `!`/`-` · `+ - * / %` · `==`(严格)· `< > <= >=` · `&&` `||` `??` · 三元 · 数组 · 括号 · 函数调用(作用域内,含零参 `now()`)· `$t(key, ...)`。
ref/computed 在表达式根部自动解包;赋值/`new`/`this`/链式调用禁止。
`a-for` 遍历数组得到元素;遍历对象得到**键名**(与 Vue 的取值语义不同)。
组件标签名 = 子组件编译后的名字(`<meta>` name 或文件名),不是 import 别名。

## setup 契约

```ts
import { reactive, ref, computed, watch, effect, onMount, onUnmount } from 'ada:surface'

export default {
  name: 'Counter',
  components: { Child },            // 子组件(模板中大写标签)
  setup(props, ctx) {
    const state = reactive({ n: 0 }) // 细粒度响应式
    const double = computed(() => state.n * 2)
    ctx.log(...)                     // 日志
    ctx.t('key', arg)                // i18n(模板里用 $t)
    ctx.api.call('svc', 'method', args)           // 调 Go 服务(同步)
    await ctx.api.callAsync('svc', 'method', args) // 异步(Promise)
    ctx.on('event', fn)              // 订阅 Go 广播(app.Emit)
    ctx.toast('已保存')              // 窗口底部提示
    ctx.emit('change', payload)      // 子 → 父
    ctx.onCleanup(fn)                // 卸载清理
    function inc() { state.n++ }
    return { state, double, inc }    // 模板可见的绑定
  },
}
```

## 控件速查

### 布局
| 控件 | 属性 | 说明 |
|---|---|---|
| `Column` / `Row` | fill / fill-x / fill-y / center / gap / gap-x / gap-y / padding / grow / align / justify / background / radius | 容器;仅绑定 `a-on:click` 时参与点击命中 |
| `Scroll` | grow(填满) | 垂直滚动容器 |
| `Spacer` / `Divider` | — | 占位/分隔 |
| `Split` | size(第一面板宽,a-model 双向)/ vertical | 可拖拽分栏;恰好两个子节点 |

### 文本
| 控件 | 属性 |
|---|---|
| `Text` | 内容(子文本)/ font-size / bold / color / align(行内对齐 start/center/end)/ padding / grow |

### 按钮
| 控件 | 事件 |
|---|---|
| `Button` / `PrimaryButton` | a-on:click;label = 子文本 |

### 输入(a-model 双向)
| 控件 | 值类型 | 额外属性 | 事件 |
|---|---|---|---|
| `TextInput` | 文本 | placeholder | input、submit(回车) |
| `NumberInput` | 数值 | min / max / step | input |
| `Slider` | 数值 | min / max | input(拖动) |
| `Stepper` | 数值 | min / max / step | input |
| `Switch` / `Checkbox` | 布尔 | label(Checkbox) | change |

### 选择与集合
| 控件 | 值 | 额外属性 | 事件 |
|---|---|---|---|
| `Segmented` | 选中项文本 | options="小,中,大" | change |
| `Tabs` | 选中项文本(配 a-if 切面板) | options="页1,页2" | change |
| `Table` | 选中行下标 | a-bind:rows=行数组 / columns="标题…" / fields="字段…" / follow-end 贴底 | change({value}) |

### 反馈
| 控件/API | 说明 |
|---|---|
| `AlertDialog` | a-model 开关(双向)/ title / message / buttons="取消,确定" / a-on:action({index,label}) / a-on:dismiss |
| `Progress` | value(0..1)/ width |
| `ctx.toast(msg)` | 窗口底部提示(几秒消失) |

## 样式(scoped)

只允许 `.class { 属性: 值 }`;属性白名单:font-size / bold / color / background /
gap / padding / width / height / radius / grow。颜色 `#hex` 或有限命名色。
令牌按组件隔离,静态属性优先于 class 令牌。

## i18n

```aui
<i18n>{ "zh-CN": { "hello": "你好 {0}" } }</i18n>
```
模板 `{{ $t('hello', name) }}`;脚本 `ctx.t('hello', name)`。
回退:组件 → 应用级(AppI18n)→ key 本身(带告警)。占位 `{0}` `{1}`。

## Go 服务桥

```go
app.Expose("svc", "method", func(a string) (string, error) { ... })  // 注册
app.Emit("event", payload)                                           // 广播(任意 goroutine)
```
```js
ctx.api.call('svc', 'method', x)                  // 同步(阻塞渲染,只做快操作)
await ctx.api.callAsync('svc', 'method', x)       // 异步(独立 goroutine,需自行加锁)
ctx.on('event', fn)                               // 订阅广播
```
参数/返回值走 JSON;Go error → JS 异常/Promise 拒绝。慢任务必须 callAsync。

## 组件协作

```aui
<!-- 父 -->
<Child a-prop:items="state.list" a-on:pick="onPick" />
<!-- 子模板 -->
<Button a-on:click="pick(item)">…</Button>
<!-- 子脚本:ctx.emit('pick', { id: item.id }) -->
```
子实例按渲染路径复用;props 响应式更新;卸载自动清理订阅与清理函数。
`a-key` 不参与实例身份:重排/头部插入会让条目状态跟随下标。

## 常见模式

**页签**:`<Tabs a-model="state.tab" options="A,B" />` + `<Column a-if="state.tab === 'A'">…`

**列表选中**:`<Table a-model="state.sel" rows=… />` + `{{ rows[sel].name }}`

**确认后执行**:`askClear() { state.confirming = true }` → `AlertDialog a-on:action="onConfirm"`
→ `onConfirm(e) { if (e.index === 1) { … } }`

**异步加载**:`onMount(async () => { state.data = await ctx.api.callAsync(...) })`
(失败包 try/catch,降级显示——服务未就绪时不崩)

## 调试

```sh
adat lint <dir>                  # 编译诊断(EUISTRUCT/EUITEMPLATE/EUIEXPR/EUISTYLE…)
adat render <dir> -entry X       # headless 首帧树
adat inspect <dir> -entry X -live 2s   # headless 持续检查(stats + 树)
ADAT_INSPECT=1 go run <main>     # 运行期周期转储到终端
adat watch <dir> -entry X        # 热重载(模板变状态保留;脚本/依赖变重跑 setup)
```

常见错误:`EUISTRUCT` 结构 · `EUITEMPLATE` 指令/标签/属性 · `EUIEXPR` 表达式 ·
`EUISTYLE` 样式 · `EUISECURITY` on*/a-html · `EUIIMPORT` 裸包名导入 · `EUIARTIFACT` 产物摘要。

## 插件(宿主视角)

```sh
adat plugin show  <dir>            # 查看 plugin.json
adat plugin build <dir> -write     # 编译 + 把 sha256 写回清单
```

```json
// plugin.json
{ "id": "hello", "version": "0.1.0", "entry": "HelloCard",
  "capabilities": ["host.greet"],
  "slots": [{ "slot": "sidebar.item", "component": "HelloSide", "order": 10 }] }
```

```go
// 宿主
app.RegisterSlot(plugin.SlotSidebarItem)
app.ExposeHost("host", "greet", func() string { return "hi" })
info, _ := app.LoadPluginDir("path/to/plugin")
for _, v := range app.SlotViews(plugin.SlotSidebarItem) { b.Draw(c, v.ID) }
app.EmitTo("hello", "ping", nil)         // 宿主 → 插件
app.OnPluginEvent(func(id, ev string, p any) { … }) // 插件 → 宿主(ctx.emitHost)
app.UnloadPlugin("hello")
```

插件内:`ctx.theme()` / `ctx.theme('fg')`(宿主令牌)、`ctx.emitHost('cmd', p)`、
`ctx.pluginId`;`ctx.on` 只订阅 `plugin.<id>.*`;`ctx.api.call('host', 'x')` 需在
`capabilities` 声明 `host.x`,否则抛异常。完整说明见 [plugins.md](plugins.md)。
