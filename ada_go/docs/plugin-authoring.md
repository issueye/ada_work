# 插件编写规范(给人与模型）

> 这篇是**新建插件的唯一口径**。模型侧同一份内容通过 `plugin_guide` 工具返回
> （`plugins/tool/plugin-helper`），文件在 `plugins/<id>/README.md` 里也会留一句摘要。
>
> 领域文档：[plugin-kernel-design.md](plugin-kernel-design.md)（内核与编排）、
> [seam-api.md](seam-api.md)（seam 契约）、[m3-assembly.md](m3-assembly.md)（preset / 清单）、
> [caps.md](caps.md)（能力与信任）。

## 1. 先回答「它该是什么」

按 AGENTS §3 决策树走，第一个「是」就是答案：

| 问题 | 产物 | 位置 |
|---|---|---|
| 只是装哪些插件、参数不同？ | preset / patch | `examples/*.preset.json` |
| 定义「agent 怎么跑一轮」？ | core 插件 | `plugins/core/<name>` |
| 定义某个宿主资源的接口？ | seam（只有类型/错误/事件名） | `plugins/seam/<name>` |
| 实现一个 seam？ | provider 插件 | `plugins/provider/<name>` |
| 约束一次调用、必须确定性阻断？ | policy 插件（装饰器 / guard） | `plugins/policy/<name>` |
| 只观察、改写、通知？ | 事件监听（写在某个插件里） | 同上 |
| 给模型用的工具？ | tool 插件 | `plugins/tool/<xx>-helper` |
| 装载/编译/执行/授权/日志？ | L2 基础设施（不是插件） | `platform/*` |
| 某个平台的窗口/协议/主线程？ | 宿主适配器 | `platform/host/*` |

一个插件只做一件事；混装 seam 与 policy 视为不合格。

## 2. 两种形态

| kind | 是什么 | 能否运行期新增 |
|---|---|---|
| `ts` | `plugin.ts` 源码，装配时由内置 esbuild 现场编译（无 Node） | **能**：写进工作区目录 + 改 preset + **重启机器人进程** |
| `go` | 编译进宿主的实现，经 `platform/host/builtins` 注册 | **不能**：必须改 Go 代码、注册 builtins、重新编译宿主 |

## 3. 目录与清单

```
plugins/<id>/
  plugin.json   清单（id 与目录同名）
  plugin.ts     入口（kind=ts）
  README.md     可选：给下一个维护者
```

```json
{
  "id": "my-plugin",
  "name": "my plugin",
  "version": "0.1.0",
  "kind": "ts",
  "entry": "plugin.ts",
  "inject": { "required": ["tools"], "optional": ["fs"] }
}
```

清单字段（`platform/plugin`）：

| 字段 | 说明 |
|---|---|
| `id` | kebab-case，与目录同名；重复身份会让装配失败 |
| `name` / `version` | 展示名；version 必须是 semver |
| `kind` | `ts` / `go` |
| `entry` / `artifact` | ts 必须有其一：源码入口或预编译产物 |
| `inject.required` / `optional` | 依赖的服务名。**required 缺失 → pending**，不是静默跳过 |
| `provide` | 插件对外提供的服务名（Go 插件由代码声明；TS 用 `ctx.provide`） |
| `caps` | 代码需要的调用点能力（`fs.read`/`fs.write`/`exec`…），是**声明**，不是授权 |
| `trust` | 由安装来源决定；磁盘上的 TS 插件一律 third-party |
| `config` | **不要写**：参数属于 preset 的 `patch` |

`plugins/<id>/plugin.json` 与 `builtins.Plugins()` 的一致性由
`platform/catalog` 的测试守着（Go 插件）；新 Go 插件必须放进 `plugins/` 并
`go run ./scripts/tools/builtins-dump -source plugins` 生成清单。

## 4. 代码形状（TS）

```ts
export function apply(ctx, config) {
  ctx.log("装配完成")
  ctx.tool({ name: "my_tool", summary: "一句话", input: { type: "object" } }, (args) => {
    return { status: "ok", content: "结果" }
  })
}
```

- ESM only；`apply` 必须可重入、无模块级状态；状态进 `ctx.store`。
- 允许 `async`：`apply`、工具处理器、事件监听器都可以 `await`（由执行层事件循环驱动，
  见 [event-loop.md](event-loop.md)）。**不要靠遗留定时器做后台工作**，周期性任务用 `sched`。
- ctx 能力：`log` / `tool` / `provide` / `emit` / `on` / `effect` / `store` / `inject`。
- 可用模块：`ada:fs`（read/write/edit/search/list）、`ada:shell`（run/exec）、
  `ada:secrets`（get）、`ada:plan`（get/set/update）、`ada:session`（current/append/history）、
  `ada:store`、`node:*`（宿主内置的 Node 兼容模块，见 [node-compat.md](node-compat.md)）。
- 工具 `input` 是 JSON Schema；`readOnly` 参与审批与并行判定；
  返回值可以是字符串，或 `{status, content, details}`。
- `ada:fs` / `ada:shell` 会经过策略装饰器链：沙箱、workspace、caps、审批仍然生效。

## 5. 接入并生效

1. `plugin_scaffold` 生成骨架（或手写清单 + 源码）；
2. 把插件目录写进 preset 的 `plugins`（相对路径按 preset 文件所在目录解析）；
   preset 用 `enable` 白名单时把 id 加进 `enable`；默认关闭时从 `disable` 里移除；
3. **重启机器人进程**——装配只发生在进程启动时，运行中的进程不会热加载新插件；
4. 验证：

```powershell
go run ./cmd/ada plan --preset <preset>      # 清单与顺序
go run ./cmd/ada check <插件目录>             # 清单 + 编译 + 能力
go run ./cmd/ada inspect --preset <preset>   # fiber / 服务 / effect / 装饰器链
```

## 6. 测试与文档（缺一不算完成）

| 交付物 | 要求 |
|---|---|
| 契约测试 | seam 提供 `seamtest`；provider 全绿 |
| 插件测试 | 行为用例（含失败/拒绝路径）+ golden（事件序列、Plan/Report） |
| 文档 | `docs/<插件>.md`：范围、配置、事件、错误码、已知边界 |
| 清单 | `plugin.json` 由 builtins-dump 生成，一致性测试通过 |
| 预算 | 单插件 ≤ 800 行（例外需 ADR），见 AGENTS §5.3 |

## 7. 硬约束（评审直接拒）

- 插件之间不 `import` 彼此的包，协作走服务或事件；
- 强制逻辑不许走事件：拒绝只能是装饰器 / guard；
- secret 不进 config、日志、事件、工具结果，按名走 `ada:secrets`；
- 不在 kernel / host 加业务特判，产品差异写进 preset；
- 禁止 `eval`、npm 包、`node_modules`、`.node`、`require`；
- 注释与文案用中文，标识符用英文；注释写「为什么」，不写「做了什么」。

## 8. 常见错误

| 症状 | 原因 | 修法 |
|---|---|---|
| `EPRESET: enable 指向不存在的插件` | preset 引用了不在扫描目录里的 id | 把插件目录加进 `plugins`，或修正 id |
| 插件显示 pending | `inject.required` 的服务没有被启用 | 在 preset 里启用 provider，或改成 optional 并自己降级 |
| `kind=go 插件没有注册实现` | Go 插件没进 builtins | 注册进 `platform/host/builtins` 并重新编译 |
| 生成插件后对话里看不到 | 装配只在启动时发生 | 把插件接进 preset，重启机器人进程 |
| 写工作区被拒绝 | fs 策略/workspace 只读 | 让用户打开写权限，或改写到允许目录 |
