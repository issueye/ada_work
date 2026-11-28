# tool-skill-helper.md — 技能工具（SKILL.md）

`plugins/tool/skill-helper` 是一个 **TS 工具插件**：把「技能」目录暴露成模型可调用的
`skill_list` / `skill_show` / `skill_scaffold`。技能是**数据**（Markdown + 附带文件），
不是可执行代码；它解决的问题是「让模型在需要时按需加载一份操作手册，而不是把所有流程塞进系统提示」。

> 与 `memory-helper` 的区别：记忆是**事实**（称呼、偏好、项目背景），技能是**步骤**
> （怎么做一类任务、有哪些坑）。记忆按会话召回，技能按名字按需读取。

## 目录约定

```text
<工作区>/skills/<id>/SKILL.md          # 默认扫描根 1
<工作区>/.ada/skills/<id>/SKILL.md     # 默认扫描根 2(隐藏目录,更贴近本机习惯)
<id>/references/*.md                   # 附带文件(按相对路径读取)
<id>/scripts/*                         # 附带文件(本插件只读,不执行)
```

`SKILL.md` 头部是简单 YAML 头（只解析 `key: value`，支持引号；不引入 YAML 库）：

```markdown
---
name: PDF 处理
description: 需要拆分/合并/抽取 PDF 时使用
---

# PDF 处理

1. 先看 references/checklist.md；
2. 用 scripts/pdf_split.py …
```

没有头部时整篇都是正文，技能名取目录名。

## 工具

| 工具 | 输入 | 行为 |
|---|---|---|
| `skill_list` | `query?` | 扫描各根的下一层目录，找 `SKILL.md`/`skill.md`，输出「名称 (id): 描述 [目录]」；`query` 匹配名称/描述/目录名；缺 `SKILL.md` 的目录列进「跳过」 |
| `skill_show` | `name`, `file?` | 按 id 或名称找到技能：默认读 `SKILL.md`，输出名称、描述、路径、附带文件列表与**去掉 frontmatter 的正文**；给 `file` 时只读该技能目录内的相对文件 |
| `skill_scaffold` | `name`, `description?`, `id?` | 在第一个扫描根下创建 `<id>/SKILL.md`（`id` 缺省由名称 slug 化）；已存在则拒绝，不覆盖 |

结果边界：正文超过 24 KiB 截断并标注原长度；递归列附带文件上限 60 条；扫描技能上限 200 个。

## 配置

```json
{
  "plugins": ["plugins", "../../plugins/tool/skill-helper"],
  "patch": {
    "skill-helper": {
      "roots": ["skills", ".ada/skills"],
      "autoInject": true,
      "maxIndexSkills": 50,
      "maxDescRune": 80,
      "maxIndexBytes": 4096
    }
  }
}
```

`roots` 是**虚拟路径**（相对路径自动挂到 `/workspace`），缺省为
`["/workspace/skills", "/workspace/.ada/skills"]`。

`autoInject`（默认 `false`）打开后，技能清单会**随每次模型请求自动注入**一条 system 消息；
其余三个键限制注入体积：最多列 `maxIndexSkills` 个技能、每条描述截到 `maxDescRune` 字符、
整段最多 `maxIndexBytes`（超出部分截断并提示用 `skill_list` 看全部）。

## 自动注入（autoInject）

为什么是「随请求」而不是「启动时缓存」:注入只改写 `llm/request`，不写进 session，
所以第二轮开始模型就看不到清单了；因此每轮都重新扫描并注入（与 `memory-helper` 的
`autoRecall` 同一语义）。扫描与 `skill_list` 共用一份实现，**没有启动期缓存**，
新建的技能下一轮请求就会出现。

注入形态:

```text
【可用技能】
可用技能(2 个;需要步骤细节时先 skill_show name=<id>):
- GitHub Trending 榜单报告 (github-trending-report): 需要抓取并按语言/周期整理 GitHub Trending 榜单……
- PDF 处理 (pdf-tools): 需要拆分/合并/抽取 PDF 时使用
```

边界:

- **没有技能时不注入**（避免每轮塞一条空消息）；扫描失败只记日志，不影响模型请求。
- 只列名称、id 与一行描述，不列目录、不贴正文：正文仍按需 `skill_show`，避免常驻 token。
- 请求里已经带 `【可用技能】` 标记时跳过，防止同一请求被重复派发时重复插入。
- 位置在首条 system 之后（与 `context-inject` / `memory-helper` 的注入消息并列）。
- 需要宿主提供 `ctx.waterfall`；没有时装配期直接报错，不静默降级。

## 安全边界

- 全部文件操作走 `ada:fs`：`fs-sandbox` / `workspace-guard` / 只读工作区照常生效；
  本插件不直接 `os.ReadFile`，也没有第二条读写路径。
- `skill_show` 的 `file` 只接受**技能目录内的相对路径**：拒绝 `..` 与绝对路径，
  因此 `file: ../../etc/passwd` 会被挡在插件参数层。
- 插件只读技能内容，**不执行** `scripts/` 里的任何东西；要执行交给 `shell-helper`（并受其策略约束）。
- `skill_scaffold` 会写文件，因此只读工作区下会失败——这是策略生效，不是插件缺陷。

## 示例

```json
{
  "name": "skill-helper",
  "plugins": ["../agent/plugins", "../../plugins/tool/skill-helper"],
  "disable": ["llm-openai", "session-jsonl"],
  "patch": { "agent-loop": { "model": "replay" } }
}
```

```text
ada plan --preset examples/skill/skill.preset.json
```

模型侧建议流程（发布 preset 的系统提示已包含）：

1. 不确定有没有对应技能时先 `skill_list`（可带 `query`）；
2. 命中后用 `skill_show` 读正文，需要细节再 `skill_show file=references/...`；
3. 用户要求沉淀新流程时用 `skill_scaffold`，再按需补附带文件（`write_file`）。

## 已知边界

- 头部只解析单行 `key: value`；多行 YAML（`|`/`>` 块）会按字面保留在描述里，不展开。
- 技能目录名与 frontmatter `name` 不一致时，两者都能用来查找；`id` 始终是目录名。
- 不做热更新缓存：每次调用都重新扫描，技能少时开销可忽略（上限 200 个目录）。
- 默认不注入；开了 `autoInject` 才把清单带进每轮请求。注入的是**索引**而不是正文——
  正文只有模型主动 `skill_show` 才会进入上下文。
- `autoInject` 每轮都会增加一条 system 消息（受 `maxIndex*` 限制）；技能很多时
  建议保留默认 50 条上限，让模型用 `skill_list` 查全量。
