# tool-helper:工具发现与按名调用

> 目录:`plugins/tool/tool-helper`(Go 插件)。它不实现任何具体能力,只是把
> `tools` 注册表本身暴露成两个模型工具:`tool_find` 与 `tool_exec`。

## 1. 目的

工具变多以后,把全部工具塞进模型请求既贵又容易选错。`tool-helper` 提供两段式用法:

1. `tool_find` 按名字/摘要/标签过滤,先看有哪些工具、参数长什么样;
2. `tool_exec` 按名字调用其中任意一个,参数放在 `args` 对象里。

这不是第二条执行路径:两个工具都通过同一个 `tools.Registry`。目标工具的
**guard、审批、沙箱、超时、caps、事件**全部照常执行,`tool_exec` 只负责转发。

## 2. 文件

```text
plugins/tool/tool-helper/
  tool_helper.go        插件与两个工具的实现
  tool_helper_test.go   过滤/转发/错误/递归拒绝的单测
examples/tool-helper/
  toolhelper.preset.json  agent 栈 + tool-helper + text-helper
  replay.jsonl           find → exec(write_file) → 汇总
  workspace/a.txt        只读示例文件
```

Go 插件必须编译进宿主:注册在 `platform/host/builtins` 的
`builtins.Plugins()` 里;发布包的 `plugin.json` 由 `builtins-dump` 自动导出。

## 3. 工具

| 工具 | 参数 | 行为 |
|---|---|---|
| `tool_find` | `query`(可空)、`limit`、`verbose` | 列出可见工具;`query` 对名字/摘要/标签做不区分大小写的子串匹配;`verbose` 附带输入 schema |
| `tool_exec` | `name`(必填)、`args`(对象,默认 `{}`)、`timeoutMs` | 通过 `tools.Registry.Invoke` 调用目标工具,并把目标的结果/状态原样返回 |

`tool_find` 只列未被 `Restrict` 隐藏的工具;`tool_exec` 即使按名字命中被限制的工具,
`Registry.Invoke` 仍会返回 `denied`。

### 3.1 结果与状态

- `tool_exec` **透传**目标工具的结果状态(`ok`/`error`/`denied`/`timeout`),不把失败改写成成功;
- 目标工具的 `Content` 原样回到模型(没有内容时补一句「目标工具没有输出」);
- 目标工具自己的 `tool/result` 事件照常发出,这次嵌套调用在审计里可见。

## 4. 执行语义

1. **同一注册表**:`tool_exec` 调 `Registry.Invoke`,因此目标工具的 guard/pre-execute/
   execute/post-execute/超时策略都会再跑一遍;审批策略需要二次确认时会二次确认,不会绕过。
2. **不绕过 caps**:目标工具检查调用方能力时看到的是同一个会话与调用点。
3. **递归防护**:
   - `tool_exec` 永远不能调用 `tool_exec` 自身(默认 deny,即使配置漏写也强制加回);
   - 额外用私有 context key 记录嵌套深度,超过 `maxDepth`(默认 1)直接拒绝;
   - 间接递归(目标工具又调用 `tool_exec`)同样受深度限制。
4. **超时**:`timeoutMs` 作为本次嵌套调用的外层 `context` 截止时间;目标工具自己的
   `ToolDef.Timeout` 仍然生效,取更早的那个。
5. **JS 工具可被调用**:`tool_exec` 是 Go 工具,目标 JS 工具运行在自己的 worker 上;
   从 Go 调用不会触发同一 runtime 的嵌套死锁(与 JS 插件里再调自己不同)。

## 5. 配置

preset 里对 `tool-helper` 的 patch:

| 字段 | 默认 | 说明 |
|---|---|---|
| `deny` | `[]`(另有强制 `tool_exec`) | 禁止被 `tool_exec` 调用的工具名列表 |
| `maxDepth` | 1(1–5) | 嵌套深度上限;1 表示不允许再嵌套 `tool_exec` |
| `defaultLimit` | 20(1–100) | `tool_find` 的默认返回条数 |

`deny: []` 不可能放开对 `tool_exec` 自身的禁止——代码在读取配置后无条件加回。

## 6. 运行

```powershell
go run ./cmd/ada up    --preset examples/tool-helper/toolhelper.preset.json
go run ./cmd/ada agent --preset examples/tool-helper/toolhelper.preset.json -message "找到写文件的工具并调用"
```

示例回放:先 `tool_find` 查 `write` 相关工具,再 `tool_exec` 调用 `write_file` 写入
`out.txt`;真实端点把 `disable` 改掉并 patch `llm-openai` 即可。

## 7. 测试

```powershell
go test ./plugins/tool/tool-helper -v
go test ./platform/host/cli -run 'TestUpToolHelper|TestAgentToolHelper'
```

| 场景 | 用例 |
|---|---|
| 过滤/limit/verbose schema | `toolhelper.TestToolFind` |
| 转发、状态透传、未知工具、自身递归拒绝 | `toolhelper.TestToolExec` |
| 内置注册表存在 tool-helper | `builtins.TestBuiltinGoPlugins` |
| 装配(Go 插件 + 清单) | `cli.TestUpToolHelper` |
| 端到端:Go 工具调用 TS 工具并落盘 | `cli.TestAgentToolHelper` |

## 8. 已知边界

1. **不是隐藏能力**:`tool_exec` 只能调用注册表里已经存在的工具;它不会绕过
   `Restrict`、没有发现新能力。
2. **审批可能触发两次**:外层 `tool_exec` 与目标工具各自经过审批链;这是有意的
   fail-closed 行为,不做特殊豁免。
3. **结果体积**:目标结果原样返回,没有额外的截断;目标工具自己的截断/脱敏策略负责体积。
4. **无跨会话状态**:不缓存、不记忆本轮用过哪些工具;需要记忆时用 `memory-helper`。
5. **正则/模糊匹配**:`tool_find` 只做子串匹配,不做同义词或向量检索。
6. **发布默认开启**:`agent-release`/`workspace-release` 的 allowlist 包含 `tool-helper`;
   若要收紧,可在 preset 的 `disable` 里去掉它,或配置 `deny` 列表。
