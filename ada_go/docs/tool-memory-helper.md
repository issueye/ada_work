# memory-helper:长期记忆系统(TypeScript 主实现)

> 目录:`plugins/tool/memory-helper`。模型面向的工具插件,不是 seam/provider:
> 记忆的读写经 `ada:fs` 走文件安全链,不新开第二条磁盘路径。

## 1. 目的与分层

`memory-helper` 让 agent 跨会话保留**显式写入的长期事实/偏好**,并提供检索、修正、
删除与统计。设计上把「业务逻辑」和「底层能力」分开:

| 层 | 位置 | 内容 |
|---|---|---|
| 主体(TS) | `plugins/tool/memory-helper/plugin.ts` | 记录模型、中文分词、BM25 排序、去重、淘汰、渲染、工具注册 |
| 文件能力 | `platform/bridge` 注入的 `ada:fs` | 读/写记忆文件,走 `seam/fs` + `fs-local` + 策略链 |
| 补充内置库 | `ada_jsrt/jsrt/nodemods/src/crypto.js` | `node:crypto` 的 `createHash("sha256")` / `randomBytes` / `randomUUID` |
| 引擎内置 | moejs `Intl.Segmenter` | 切分中英文词边界;引擎没有时脚本自动退化为逐字符扫描 |

为什么把哈希/随机数补进执行层而不是在脚本里手写:`node:crypto` 在
[node-compat.md](node-compat.md) §5.1 里原本属于「未实现」;记忆的稳定 id 与内容指纹
需要跨进程唯一、不可预测的随机源,以及真正的 SHA-256。宿主用 Go 的 `crypto/sha256`
与 `crypto/rand` 提供,跨边界只传 hex 字符串,不把二进制对象塞进 JSON。

## 2. 文件

```text
plugins/tool/memory-helper/
  plugin.json   kind=ts,inject: tools/fs
  plugin.ts     主实现
examples/memory-ts/
  memory.preset.json    agent 栈 + memory-helper + 回放
  replay.jsonl          write → search → 汇总
  workspace/            示例工作区
```

## 3. 工具

| 工具 | 参数 | 行为 |
|---|---|---|
| `memory_write` | `text`(必填)、`tags`、`scope`、`importance`、`pinned` | 新增;精确或近似重复会合并到已有记录 |
| `memory_search` | `query`(必填)、`scope`、`tags`、`limit` | 关键词检索,返回短 id + 相关度 + 摘要 |
| `memory_read` | `id`(必填,支持唯一前缀) | 读取完整正文与元数据 |
| `memory_list` | `scope`、`tags`、`limit`、`offset` | 按重要度/更新时间倒序分页 |
| `memory_update` | `id`(必填)+ 任一可改字段 | 修正正文/标签/作用域/重要度/`pinned` |
| `memory_forget` | `id` | 物理删除一条记忆 |
| `memory_stats` | — | 总数、作用域分布、常见标签、词条规模 |

只读工具(`search/read/list/stats`)声明 `readOnly: true`;真正的权限仍由
`tools` 策略(审批/超时/caps)与 fs 策略决定。

## 4. 记录与存储

记录:

```json
{
  "id": "0ed2968f-...",
  "text": "用户偏好中文回答，代码注释也使用中文。",
  "tags": ["preference", "language"],
  "scope": "project",
  "importance": 4,
  "createdAt": "2026-11-23T...",
  "updatedAt": "2026-11-23T...",
  "fingerprint": "sha256(规范化正文)",
  "pinned": false
}
```

文件是单文档 JSON(默认 `/workspace/.ada/memory.json`,即工作区下 `.ada/memory.json`):

```json
{"version":1,"updatedAt":"...","records":[...]}
```

- 写入经 `ada:fs.write`(临时文件 + rename),不会留下半文件;
- 文件不是合法 JSON 时**拒绝覆盖**并报错,避免把损坏当成空库重置;
- 单条记录读坏只跳过该条,不影响整库;
- 读取时按 `maxRecords` 与 3 MiB 文件上限淘汰「重要度最低、最久未更新」的记录。

## 5. 分词与排序

中文没有空格,脚本用两步近似:

1. `Intl.Segmenter("zh", {granularity:"word"})` 取词边界;连续汉字段再展开成
   **unigram(权重 0.45)+ bigram(权重 1.0)**;
2. ASCII/拉丁词按整词计;停用词(常见虚词)不建索引。

排序是 BM25 变体 + 若干确定性加成:

| 项 | 说明 |
|---|---|
| 词项 | `idf * tf 饱和 * 查询权重`,按当前候选集合动态算 `df/avgdl` |
| 精确短语 | 查询串整体出现在正文里时 +1.2 |
| 重要度 | `(importance - 3) * 0.12` |
| 新鲜度 | `0.3 / (1 + 天数/60)` |
| 作用域 | 指定 `scope` 过滤时,同作用域 +0.15 |

检索是**词典匹配,不是向量语义**:同义改写、跨语言改写不一定命中;要语义检索应接
embedding provider(见 §11)。

## 6. 去重、合并与淘汰

- **精确去重**:同一 `scope` 下 `sha256(规范化正文)` 相同即合并(标签取并集、重要度取最大、
  `updatedAt` 刷新;新正文长度超过旧正文 1.25 倍时替换正文)。
- **近似去重**:同一 `scope` 下词项 Jaccard ≥ `dedupeThreshold`(默认 0.85)也合并;
  设成 `1` 关闭近似去重,设成 `0` 完全关闭合并。
- **淘汰**:写入后记录数超过 `maxRecords`,或 JSON 超过 3 MiB 时,按
  「重要度低 → 更新早」逐个淘汰,并在工具结果里明确告诉模型淘汰了几条。
- **`pinned` 保护**:`pinned: true` 的记录不参与自动淘汰;全部记录都 pinned 且仍超限时,
  写入直接报错(不静默丢数据),需要先 `memory_update` 解除或 `memory_forget`。

## 7. 配置

preset 里对 `memory-helper` 的 patch 会作为 `apply(ctx, config)` 的第二个参数:

| 字段 | 默认 | 说明 |
|---|---|---|
| `path` | `/workspace/.ada/memory.json` | 记忆文件虚拟路径;相对路径挂到 `/workspace` |
| `maxRecords` | 2000(50–10000) | 记录上限 |
| `maxTextChars` | 4000(100–20000) | 单条正文上限 |
| `searchLimit` | 8(1–50) | `memory_search` 默认条数 |
| `listLimit` | 20(1–100) | `memory_list` 默认条数 |
| `defaultScope` | `project` | 未传 `scope` 时的作用域 |
| `dedupeThreshold` | 0.85(0–1) | 近似去重阈值 |
| `maxSummaryChars` | 240(60–1000) | 检索/列表里的正文摘要长度 |
| `autoRecall` | `false` | 每次模型请求前按最后一条用户消息自动召回 |
| `recallLimit` | 5(1–20) | 自动召回最多几条 |
| `recallMaxChars` | 1200(200–8000) | 注入的回忆摘要总字符上限 |
| `recallMinScore` | 0(≥0) | 自动召回的最低相关度 |

作用域固定为 `project` / `user` / `global`;工具参数里传未知值会被拒绝,不做静默归一。

### 7.1 自动召回(llm/request 中间件)

`autoRecall: true` 时插件在 `llm/request` 上注册一个 waterfall:

1. 先调 `next(req)` 让下游策略跑完;
2. 从请求的 `messages` 里取**最后一条 user 消息**作为查询(工具结果不算查询);
3. 用同一套 BM25 排序取 top `recallLimit` 条,受 `recallMinScore` 与 `recallMaxChars` 约束;
4. 在首条 system 提示之后插入一条 `system` 消息,正文以 `【长期记忆】` 开头;
5. 没有命中、查询为空、记忆文件读失败都**原样放行**——召回失败不能让模型请求失败。

这条链路能成立,是因为执行层的 waterfall 现在会把 JS 返回的 JSON 还原成输入的具体类型
(`*llm.Request`),否则 `agent-loop` 的 `v.(*llm.Request)` 会失败、消息被丢弃
(见 [m2-execution.md](m2-execution.md) §3 的 waterfall 语义)。

自动召回只覆盖 LLM 请求;`prompts/assemble` 的静态 Section 仍不参与,所以系统提示里的
使用规则(`system-prompt.tools`)仍然保留,两者互补。

## 8. 运行

```powershell
go run ./cmd/ada up    --preset examples/memory-ts/memory.preset.json
go run ./cmd/ada agent --preset examples/memory-ts/memory.preset.json -message "记住我的偏好并回忆"
```

示例 preset 用 `llm-replay` 回放 write → search;真实端点把 `disable` 改成
`llm-replay`、patch `agent-loop.model` 与 `llm-openai` 即可。

## 9. 测试

```powershell
go test ./platform/bridge -run 'TestJSToolMemory|TestJSMemoryAutoRecall|TestNodeCrypto'
go test ./platform/host/cli -run 'TestUpMemoryScriptPlugin|TestAgentMemoryScriptPlugin'
```

| 场景 | 用例 |
|---|---|
| `node:crypto` 形状与 SHA-256/UUID | `platform/bridge.TestNodeCrypto` |
| 写入→合并→中文检索→读取→更新→列表→统计→删除 | `platform/bridge.TestJSToolMemory` |
| `pinned` 不被容量淘汰 + 全部 pinned 时明确报错 | `platform/bridge.TestJSToolMemoryPin` |
| `llm/request` 自动召回 + JS 返回值还原成 `*llm.Request` | `platform/bridge.TestJSMemoryAutoRecall` |
| JS waterfall 改写结构体指针输入 | `ada_jsrt/jsrt.TestWaterfallCoercesStructPointer` |
| 清单/编译/工具注册 | `platform/host/cli.TestUpMemoryScriptPlugin` |
| 真实 agent 回路 + 文件落盘 | `platform/host/cli.TestAgentMemoryScriptPlugin` |

## 10. 已知边界

1. **无跨进程锁**:同一运行时内工具调用串行,读改写是安全的;但两个进程同时写同一文件
   会丢更新。`ada:fs` 没有暴露锁文件原语,与 `task-helper` 的边界相同。
2. **检索是词典匹配**:没有向量/embedding,不支持同义改写;中文 bigram 对长句足够,
   对专业术语与跨语言表述不稳定。
3. **近似去重是启发式**:Jaccard 合并可能把「同主题但不同事实」合成一条;重要事实建议
   显式用不同措辞或不同 `scope`。
4. **物理删除**:`memory_forget` 不保留墓碑,删除后无法审计/恢复;需要审计时应由
   `audit-log` 记录工具调用本身。
5. **记忆不是会话事实**:文件在 `.ada/` 下,不写 session 事件;fork/resume/replay 不会
   自动同步记忆,多会话共享同一文件。
6. **自动召回只覆盖 llm/request**:`prompts/assemble` 的静态 Section 仍不参与;注入的是
   一条 system 消息,不改变首条系统提示。`autoRecall` 默认关,发布 preset 才打开。
7. **`node:crypto` 只收字符串**:跨边界不传 Buffer/TypedArray,`Hash.update` 收到非字符串
   会明确报 `TypeError`,不是静默按 UTF-8 近似。

## 11. 下一步

1. 让 `ada:fs` 暴露「带版本的读改写」或锁文件原语,把并发写做成事务;
2. 增加 embedding provider(可复用 `llm-openai` 的 `/embeddings`),检索改成
   词典 + 向量混合排序;
3. 让 `prompts/assemble` 也暴露 JS 可回填的语义(现在是静态 Section),减少一条 system 消息;
4. 记忆分片/命名空间(按项目、按用户)与过期策略进 preset 字段;
5. 召回命中后回写 `lastUsedAt`/`uses`,用于「常用优先」排序(需要可写的读路径)。
