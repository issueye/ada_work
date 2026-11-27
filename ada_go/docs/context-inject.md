# policy/context-inject(工作区上下文注入）

> 把工作区里的说明文件拼成一段「工作区上下文」,在每次模型调用前追加到 system prompt。
> 契约面：[seam-api.md](seam-api.md)（`llm/request` waterfall）；同类策略参考
> [seam-fs.md](seam-fs.md)（装饰器链）与 [plugin-authoring.md](plugin-authoring.md)（插件规范）。

## 1. 范围

```
plugins/policy/context-inject/   监听 llm/request,把文件内容并入第一条 system 消息
```

它解决的是「模型不知道项目约定」：AGENTS.md / README.md 里的规则、目录说明、
命令约定，不必每次让用户粘贴。**不是**记忆、不是检索、不是门禁。

## 2. 配置

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `title` | string | `工作区上下文` | 注入段的标题(`## <title>`) |
| `files` | string[] | — | 工作区相对路径;`/workspace` 前缀可省,也接受绝对 fs 路径 |
| `text` | string | — | 直接注入的静态文本,排在文件内容之前 |
| `maxBytes` | int | 32768 | 整段注入的上限(字节);小于 256 时按 256 处理 |

```json
{ "context-inject": { "title": "项目约定", "files": ["AGENTS.md", "docs/architecture.md"], "maxBytes": 16384 } }
```

## 3. 行为

- 监听 `llm/request` waterfall(`Priority = 200`),把内容**追加**到第一条 `system`
  消息的文本块;没有 system 消息时前置一条。
- 文件通过 `fs` seam 的**装饰器链**读取：沙箱、workspace、caps、审批照常生效;
  读不到（不存在 / 被策略拒绝）就**跳过**,不往对话里塞错误,也不让整轮失败。
- 注入内容是**派生数据**：不进 session、不进模型可见事实;文件变化下一轮就生效。
- 超过 `maxBytes` 时按字节截断并追加「（上下文已截断）」。

## 4. 不变量

| 不变量 | 理由 |
|---|---|
| 只改写请求,不阻断 | 门禁必须走装饰器/guard（AGENTS P7）;注入失败不应让对话失败 |
| 只并进第一条 system | 多家 OpenAI 兼容端点对多条 system 的拼接方式不一致 |
| 不读 secret | 注入的是项目说明,不是凭证 |
| 有上限 | prompt 预算有限,不能因为一个大文件把整轮挤爆 |

## 5. 已知边界

1. 只读文件,不递归扫目录（要目录摘要请用 `workspace-helper` / `text-helper`）;
2. 不按 token 精确计量,`maxBytes` 是字节近似;
3. 每轮调用都重新读文件（保持最新,但大文件有 IO 成本）;
4. 依赖 `fs` 服务；没有 fs 时只能注入 `text`。

## 6. 验证

```powershell
go test ./plugins/policy/context-inject
go run ./cmd/ada plan --preset examples/agent/agent.preset.json   # 看 context-inject 在顺序里
go run ./cmd/ada inspect --preset examples/agent/agent.preset.json --chain   # 装饰器链
```
