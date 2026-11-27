---
name: ada-plugin-authoring
description: 在 ada 里新建插件（TS 运行期生成 / Go 需重编）；清单、ctx API、接入 preset 与验证的规范
---

# 新建 ada 插件

先读规范，再动手：调用 **`plugin_guide`** 工具拿完整口径（清单字段、ctx API、接入与验证）。
本技能只给最短路径与红线。

## 先判断形态

| 需求 | 形态 | 能否现在做 |
|---|---|---|
| 给模型一组新工具 | `kind=ts`，写到工作区 | 能；改 preset 后**重启机器人进程**生效 |
| 实现一个 seam / 改内核行为 | `kind=go`，注册 builtins | 不能运行期生成，要改源码并重新编译宿主 |

一个插件只做一件事：seam（契约）、provider（实现）、policy（门禁）、tool（工具）。

## 最短路径（TS）

1. `plugin_scaffold`：给 `id`（kebab-case，与目录同名）、`summary`，可选 `tool`；
   它写 `plugin.json` + `plugin.ts` + `README.md`，目录缺省 `plugins/<id>`。
2. 把目录写进 preset 的 `plugins`；preset 用 `enable` 时把 id 加进 `enable`。
3. **重启机器人进程**（装配只发生在启动时）。
4. `ada plan --preset <preset>` / `ada check <目录>` 验证。

## 清单要点

```json
{ "id": "my-plugin", "version": "0.1.0", "kind": "ts",
  "entry": "plugin.ts", "inject": { "required": ["tools"], "optional": ["fs"] } }
```

- `id` 与目录同名；`version` 必须 semver；
- `inject.required` 缺失 → 插件 pending（不是静默跳过）；optional 缺失照常装配；
- 参数不要写进清单的 `config`：属于 preset 的 `patch`。

## 代码要点

```ts
export function apply(ctx, config) {
  ctx.tool({ name: "my_tool", summary: "一句话", input: { type: "object" } }, (args) => {
    return { status: "ok", content: "结果" }
  })
}
```

- ESM only；apply 可重入、无模块级状态；状态进 `ctx.store`；
- ctx：`log` / `tool` / `provide` / `emit` / `on` / `effect` / `store`；
- 模块：`ada:fs`、`ada:shell`、`ada:secrets`、`ada:plan`、`ada:session`、`ada:store`、`node:*`；
- 读写会经过 fs/shell 的策略链（沙箱、workspace、caps、审批仍然生效）。

## 红线

- 插件之间不 import 彼此的包；协作走服务或事件；
- 拒绝只能走装饰器 / guard，不许用事件做门禁；
- secret 不进 config、日志、事件、工具结果；
- 禁止 eval、npm 包、node_modules、`.node`、`require`；
- 注释与文案用中文，标识符用英文。

## 常见失败

| 现象 | 原因 | 修法 |
|---|---|---|
| `enable 指向不存在的插件` | preset 引用了扫描目录外的 id | 把插件目录加进 `plugins` |
| 插件 pending | required 服务未启用 | 启用 provider 或改 optional |
| 生成后看不到 | 没有重启 / 没接进 preset | 改 preset，重启机器人进程 |
