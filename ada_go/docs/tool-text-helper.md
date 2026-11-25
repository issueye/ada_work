# 插件:text-helper(TS + 注入 ada:fs)

> 仓库里**唯一**的文件工具实现,位置 `plugins/tool/text-helper`。
> 原 `plugins/tool/fs` 已并入本插件并删除:同一能力只有一份定义。
> 工具名沿用 `read_file` / `write_file` / `edit_file` / `search_files` / `list_dir`,
> 所有 preset 与 replay 无需改写。

## 1. 定位

| 层 | 内容 |
|---|---|
| 插件 | `plugins/tool/text-helper`(kind=ts,`inject: tools/fs`) |
| 暴露的工具 | `read_file` / `write_file` / `edit_file` / `search_files` / `list_dir` |
| 底层 | 注入模块 `ada:fs` → `seam/fs` → `fs-local` + 策略链 |

插件只做参数校验、路径归一化与结果格式化;路径夹紧、只读/沙箱、审批与审计
全部由 fs 实现与策略负责。

历史上这两个名字并存:`tool/fs` 是产品工具,`text-helper` 是 M4.5 的脚本插件示例。
两者底层完全相同、工具一一对应,只是包装层略有差别,因此合并为一份
(保留 `text-helper` 这个 id,沿用 `tool/fs` 的工具名与加固做法)。

## 2. 文件

```
plugins/tool/text-helper/
  plugin.json     id=text-helper,kind=ts,inject: tools/fs
  plugin.ts       TS 实现
examples/text-ts/
  text.preset.json  agent 栈 + text-helper
  replay.jsonl      write → edit+search 回放
  workspace/        示例工作区
```

## 3. 工具

| 工具 | 行为 | 加固点 |
|---|---|---|
| `read_file` | 读取文本 | 只读;`path` 只做词法归一,越界由策略判定 |
| `write_file` | 原子写入(自动建目录) | 回执带 UTF-8 字节数 |
| `edit_file` | 精确替换(read-before-edit) | `old` 必填校验 |
| `search_files` | 搜索文本,返回 `path:line: text` | `limit` 缺省 200、上限 1000 |
| `list_dir` | 列目录(可递归) | `limit` 缺省 200、上限 1000 |

五个工具都声明 `timeoutMs: 30000`,错误统一包成中文上下文
(`读取 /workspace/x 失败: …`),不把底层错误原样丢给模型。

## 4. 注入模块

`ada:fs`:

```js
export function read(path) { return host.fs.read(path) }
export function write(path, content, createDirs) { return host.fs.write(path, content, createDirs) }
export function edit(path, old, newText, all) { return host.fs.edit(path, old, newText, all) }
export function search(query, path, limit) { return JSON.parse(host.fs.search(query, path || "/workspace", limit || 0)) }
export function list(path, recursive, limit) { return JSON.parse(host.fs.list(path || "/workspace", recursive, limit || 0)) }
```

对应 `jsrt.Host.FSRead/FSWrite/FSEdit/FSSearch/FSList`;搜索/列目录返回 JSON 数组。

## 5. 运行

```powershell
go run ./cmd/ada agent --preset examples/text-ts/text.preset.json -message "写文件、替换并搜索"
go run ./cmd/ada agent --preset examples/agent/agent.preset.json -message "读取 a.txt 并总结"
```

真实端点:切到 `llm-openai` 并设置 `ADA_LLM_API_KEY`;已手工验证 write→edit 成功。

## 6. 测试

| 场景 | 用例 |
|---|---|
| TS 文件工具全链路 | `platform/bridge.TestJSToolText`(读/写/编辑/搜索/列目录) |
| 脚本装配 | `platform/host/cli.TestUpTextScriptPlugin` |
| 脚本写/编辑 | `platform/host/cli.TestAgentTextScriptPlugin` |
| agent 回放读文件 | `platform/host/cli.TestAgentReplay` |
| fs-local Search | Go provider 测试覆盖(真实文件搜索) |

## 7. 已知边界

1. **搜索是字面量匹配、大小写敏感**,不是正则;只搜文本文件,单文件 ≤1 MiB。
2. **编辑是 read-before-edit**:并发改同一文件会冲突(`ECONFLICT`)。
3. **脚本路径只做词法前缀**:真实路径/越界由 fs-local 与策略负责。
4. **搜索/列目录返回 JSON**,脚本再格式化;`limit` 已夹到 200/1000,不会把整棵树拉进上下文。
5. **无 SDK 类型**:`ctx.tool`/`ada:fs` 类型仍由插件自己声明;后续接 `@ada/sdk` 时再替换。
6. **生产需挂 fs-sandbox/caps**:示例只挂 fs-local。
