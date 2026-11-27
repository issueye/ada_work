# 插件最小闭环(ada_t 作为宿主侧运行时)

> 目标:在**同一个进程**里,让第三方 `.aui` 组件包安全地嵌进宿主界面(如 ada_chat),
> 而宿主仍然掌控布局、尺寸、主题、服务能力与事件边界。

## 0. 边界先说清楚

| 维度 | 本层提供 | 本层**不**提供 |
|---|---|---|
| 代码执行 | 编译期受限表达式、`DisableDynamicCode`(无 `eval`/`Function`)、标签/属性白名单 | 进程级沙箱、系统调用拦截 |
| 能力 | `plugin.json` 的 capabilities 白名单,`ctx.api.call` 默认拒绝 | 运行时权限弹窗/授权 UI |
| 文件/网络 | 插件 JS 没有任何文件/网络 API,只能经宿主暴露的服务 | 插件自带的 Go 代码(插件只是编译好的组件包) |
| 隔离 | 模块名按插件前缀命名、组件名全局唯一、事件名加 `plugin.<id>.` 前缀 | 地址空间/内存隔离 |
| 信任 | sha256 摘要(内部)、ed25519 签名(第三方)、宿主自定义信任策略 | 证书链/商店分发 |

一句话:**编译期安全 + 能力白名单 + 命名空间,不等于运行时权限隔离**。
插件与宿主同进程同 loop,宿主暴露的每个服务都是插件的潜在攻击面,服务实现必须自己校验输入。

## 1. 插件目录结构

```
examples/plugins/hello/
  plugin.json          # 清单(必需)
  HelloCard.aui        # 组件(entry 与 slot 组件)
  HelloSide.aui
  util.ts              # 可选的相对导入模块
```

`plugin.json`(最小):

```json
{
  "id": "hello",
  "name": "Hello 插件",
  "version": "0.1.0",
  "entry": "HelloCard",
  "capabilities": ["host.greet"],
  "slots": [
    { "slot": "sidebar.item", "component": "HelloSide", "order": 10 },
    { "slot": "composer.action", "component": "HelloCard", "order": 10 }
  ]
}
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | ✅ | 字母数字与 `_.-`,≤64;运行时用它做模块前缀与事件前缀 |
| `version` | ✅ | 宿主展示/升级判断用(本层不做版本约束求解) |
| `entry` | ✅ | 入口组件名(编译时的 `-entry`) |
| `name` / `description` | 否 | 展示用 |
| `deps` | 否 | 依赖的其它插件 id(目前只记录,不自动解析加载顺序) |
| `capabilities` | 否 | 允许调用的宿主服务方法 `service.method`;支持 `*`、`host.*`、`host.*.append` |
| `slots` | 否 | 槽位贡献:只声明 `slot` + `component`(+ 建议尺寸/order) |
| `sha256` | 否 | 编译产物摘要;存在即校验,`PluginTrust{RequireDigest:true}` 时强制 |
| `signature` | 否 | 对 `sha256` 字符串的 ed25519 签名(base64);宿主配置公钥时强制 |
| `publisher` | 否 | 发布者标识(展示/审计) |

清单解析是**严格**的:`DisallowUnknownFields`,拼错字段直接报错。

## 2. 宿主 API

```go
hostPkg, _ := adat.CompileDir("examples/plugin-host/host", "")

b := embed.New(embed.Options{Theme: tokens})
app, _ := adat.NewApp(hostPkg, adat.AppOptions{
    Backend:  b,
    SlotMode: true,          // LoadPlugin 时自动挂载槽位贡献
    Theme:    tokens,        // 插件 ctx.theme() 可读
    PluginTrust: adat.PluginTrust{RequireDigest: true},
})

// 1) 注册插槽(插件只能投放到已注册的槽位)
app.RegisterSlot(plugin.SlotSidebarItem)
app.RegisterSlot(plugin.SlotComposerAction)

// 2) 暴露宿主服务(插件需在 capabilities 里声明 host.<method>)
app.ExposeHost("host", "bots.list", func() []Bot { ... })
app.ExposeHost("host", "chat.append", func(text string) error { ... })

// 3) 编译 + 装载
pkg, man, _ := adat.CompilePlugin("examples/plugins/hello")
info, err := app.LoadPlugin(pkg, man)     // 或 app.LoadPluginDir(dir)

// 4) 宿主画槽位(在自己的 ui.Context 里)
for _, v := range app.SlotViews(plugin.SlotSidebarItem) { b.Draw(c, v.ID) }

// 5) 宿主 → 插件事件
app.EmitTo("hello", "message", map[string]any{"text": "hi"})

// 6) 插件 → 宿主事件(回调在独立 goroutine)
app.OnPluginEvent(func(pluginID, event string, payload any) { ... })

// 7) 卸载:视图回收、组件注销、实例/订阅清理
app.UnloadPlugin("hello")
```

公开类型:`PluginInfo`、`SlotView`、`PluginStats`、`PluginTrust`。
`CompileDir` / `CompilePlugin` / `PluginDigest` 让宿主**不必** import `internal/*`。

### 状态与生命周期

```go
go func() { _ = app.Run() }()   // embed.Run 阻塞到 App.Stop;宿主主线程留给 mygo 窗口
...
_ = app.Stop()                  // 关窗时停止 JS 世界
```

`SlotMode` 与 `backend/embed`:

- `embed.Backend` 不创建原生窗口,只维护「视图 id → widget 树」;
- 宿主在**自己的** `ui.View` 里调 `b.Draw(c, viewID)`;
- 输入事件在 `Draw` 时检测,经 `Dispatch → rt.Fire` 回 JS;
- `App.Apply` 的树来自 rt 的渲染 pass,宿主主线程读树有锁保护;
- 插件 `ctx.toast` 入队的提示由宿主经 `b.DrainToasts(viewID)` 取出并自行展示
  (embed 不会自动画,避免替宿主决定提示样式)。

## 3. 槽位(slot)

插件**只声明**投放到哪个槽位,位置/尺寸/主题/顺序由宿主决定。

标准槽位常量(`plugin` 包,宿主可注册也可自定义):

| 常量 | 值 | 典型位置 |
|---|---|---|
| `SlotSidebarItem` | `sidebar.item` | 侧栏列表条目 |
| `SlotChatHeader` | `chat.header_action` | 会话头部动作按钮 |
| `SlotComposerAction` | `composer.action` | 输入框旁动作 |
| `SlotMessageRenderer` | `message.renderer` | 自定义消息渲染 |
| `SlotSettingsTab` | `settings.tab` | 设置页签 |
| `SlotCommand` | `command` | 命令面板条目 |

`SlotViews(slot)` 返回该槽位的视图(按 `slot`、`order`、`id` 排序),宿主逐条 `Draw`。
贡献里的 `width`/`height` 只是**建议**,宿主可忽略。

## 4. 能力白名单(默认拒绝)

- 插件组件里的 `ctx.api.call(service, method, ...)` 先查 `plugin.json` 的 `capabilities`;
- 命中规则:精确 `host.bots.list`,或通配 `host.*` / `host.*.append` / `*`;
- 未命中直接抛异常(JS 侧 `try/catch`;事件处理器里的越权会进运行时日志):

  ```
  surface: 插件 hello 无能力调用 host.secret(需在 plugin.json 的 capabilities 声明)
  ```

- `app.HostCapabilities()` 返回已暴露的 `service.method`,宿主可据此生成插件文档或校验清单。

宿主服务在 **JS loop** 上同步执行:只适合内存级快速操作;慢任务在服务内部立即返回,
再用 `app.EmitTo` 把结果推回插件。

## 5. 事件命名空间

| 方向 | 写法 | 实际事件名 |
|---|---|---|
| 宿主 → 插件 | `app.EmitTo("hello", "ping", p)` ↔ 插件 `ctx.on('ping', fn)` | `plugin.hello.ping` |
| 插件 → 宿主 | 插件 `ctx.emitHost('command', p)` ↔ 宿主 `OnPluginEvent` | `plugin.hello.command` |

- 插件里的 `ctx.on` **自动**只订阅自己命名空间的事件,宿主 `app.Emit("ping", ...)` 不会串到插件;
- 插件之间默认不能通信(各自前缀不同);需要协作时由宿主做中转;
- `ctx.emit`(父组件)→ 用于插件内部组件树,不出插件。

## 6. 主题

宿主通过 `SetTheme(tokens)` 下发设计令牌,插件用 `ctx.theme()` 读取:

```js
const theme = ctx.theme()      // { bg:'#10151d', panel:'#1d2430', fg:'#e8eef7', ... }
ctx.theme('fg')                // 单个令牌;缺失返回 ''
return { theme, state }        // 模板里 a-bind:color="theme.fg" / a-bind:background="theme.panel"
```

```aui
<Column a-bind:background="theme.panel" radius="6">
  <Text a-bind:color="theme.fg">…</Text>
</Column>
```

ada_chat 可把 lobe-mygo 的 token 直接映射成扁平表(`bg/panel/fg/muted/accent/radius`),
插件就不需要自带一套配色。scoped `<style>` 仍然可用(令牌按组件隔离)。

ada_t 自带该映射:`ada_t/lobe`.`Theme.FlatTokens(dark)` 产出
`dark/bg/panel/elevated/fg/muted/tertiary/accent/accentHover/border/radius`,
`ApplyFlat(app, dark)` 下发到 `adat.App.SetTheme`;原生渲染同源应用 lobe-mygo 主题。

## 7. 信任与安装

### 内部插件(随宿主发布)

```sh
adat plugin build examples/plugins/hello -write   # 编译 + 把 sha256 写回 plugin.json
adat plugin show  examples/plugins/hello          # 查看清单
```

宿主侧:

```go
adat.AppOptions{PluginTrust: adat.PluginTrust{RequireDigest: true}}
```

摘要不符/缺失会在 `LoadPlugin` 阶段被拒绝(不进入 JS 世界)。

### 第三方插件(签名)

```go
// 发布方:对 "sha256:…" 摘要字节签名
_ = plugin.Sign(man, privKey)

// 宿主:配置发布者公钥(可多把轮换)
adat.AppOptions{PluginTrust: adat.PluginTrust{
    RequireDigest: true,
    PublicKeys:    []ed25519.PublicKey{pub1, pub2},
}}
```

`VerifySignature` 用 `crypto/ed25519` 验证 `manifest.SHA256` 字符串本身;
签名不覆盖清单其它字段,所以**能力白名单也随摘要一起受保护**——改清单必须重算摘要并重新签名。

### 默认拒绝清单

- 插件没有文件/网络 API(JS 侧不存在 `require('fs')`/`fetch`);
- 未声明的宿主服务一律拒绝;
- 未注册的槽位拒绝装载;
- 组件名与已装载组件冲突拒绝装载。

## 8. 嵌入 ada_chat(宿主侧接入清单)

ada_chat 是独立的 Go module,接入步骤:

1. **统一 mygo 版本**:ada_t 目前编译于 `mygo v0.10.0`;ada_chat 用的是更早的
   `v0.8.x`。先把 ada_chat 升到同一版本(否则 `go mod` 只能选一个版本,`ui` API 可能不兼容)。
2. `go.mod` 加 `require github.com/issueye/ada_t <version>`(或 `replace` 到本地目录)。
3. 在 ada_chat 的 `internal/app` 里建 `*adat.App`:
   - `backend/embed` + `SlotMode: true`;
   - 注册槽位:`sidebar.item`、`chat.header_action`、`composer.action`、`settings.tab`、`command`;
   - 暴露服务(命名空间化,而不是共享一个 `Expose`):
     `host.bots.list` / `host.bots.get`、`host.sessions.list`、
     `host.messages.history` / `host.chat.append`、`host.tools.list` / `host.tools.run`、`host.settings.get`。
4. 把 ada_chat 现有 `ui.Context` 的绘制点改成:
   原生控件(侧栏/输入框/标题)照旧,插件贡献的位置调 `embed.Draw(c, view.ID)`;
   侧栏列表项、输入框旁动作、设置页签都可以这样插入。
5. 主题:把 lobe-mygo 的 token 映射成扁平表,`adat.NewApp(..., Theme: tokens)`;
   或直接用 `ada_t/lobe`.`Default().ApplyFlat(app, dark)` / `FlatTokens(dark)`。
6. 生命周期:ada_chat 关闭时 `app.Stop()`;插件变化时 `UnloadPlugin` + `LoadPluginDir` 重装
   (插件不参与主包 HMR)。

过渡方案(如果暂不想改主窗口):先用 ada_chat 已有的子窗口/浮动面板承载插件窗口
(`mygoui.New()` + `App.Window`),槽位能力先不接;等主窗口改造完再切 `embed`。

## 9. 已知限制(本层承诺之外)

- **组件名全局唯一**:插件组件不能与宿主或其它插件重名(模板按标签名查注册表);
  需要命名冲突时,用 `<meta name="HelloCard">` 手工前缀。
- **插件不能 Expose 自己的服务**:插件 → 宿主只走 `ctx.emitHost` 或宿主暴露的服务;
  插件之间无直接调用。
- **`deps` 不自动解析**:宿主决定装载顺序;循环依赖由宿主负责。
- **插件 HMR 不支持**:改插件后 `UnloadPlugin` + `LoadPluginDir`(主包 HMR 不受影响)。
- **无运行时权限隔离**:见 §0;宿主暴露的服务必须自己做输入校验与限流。
- **`LoadPlugin` 失败回滚是尽力而为**:组件注册表会清理,但 JS 侧已创建的实例由
  `unregisterComponents` 兜底。

## 10. 验收(仓库内测试)

| 行为 | 测试 |
|---|---|
| 清单解析/校验/通配/签名 | `plugin/plugin_test.go` |
| 装载、首帧、能力允许/拒绝、事件前缀、模块前缀隔离、卸载、重名拒绝 | `internal/rt/plugin_test.go` |
| 公共 API + embed 绘制 + 点击回传 + 主题 + 槽位注册 + 信任策略 | `plugin_host_test.go` |
| 示例插件不漂移 | `TestExamplePluginCompiles` |
