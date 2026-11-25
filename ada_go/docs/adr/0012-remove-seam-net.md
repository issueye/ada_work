# ADR 0012:移除 seam/net、net-http、net-guard、net-observe

- 状态:已接受(打破 seam 契约的删除;由本 ADR 追认)
- 日期:2026-11-23
- 复议日期:2027-06-30
- 相关:[ADR 0011](0011-node-network-direct.md)(本 ADR 撤销其 §4 的保留条款)、[node-compat.md](../node-compat.md)、AGENTS §0 红线 6、§12

## 1. 背景

ADR 0011 决定 Node 网络模块直连标准库,但**保留**了 `seam/net` 与 `net-guard`,并写明「需要授权/审计的产品仍应让插件用 seam/net」。在完成该改动后对全仓库做一次依赖审计,得到的事实是:

| 检查项 | 结果 |
|---|---|
| 声明 `inject: ["net"]` 的非 net 插件 | **0 个** |
| `plugins/tool/*` 里使用网络的工具 | **0 个**(只有 `tool/fs`、`tool/shell`、`tool/workspace`) |
| preset 里启用 net 栈的 | 只有 `examples/net`(纯清单展示,无行为) |
| 引用 `seam/net` 的非 net 代码 | `platform/host/cli` 的 `inspect --chain`、`policy/audit-log` 的事件订阅、`policy/securitymatrix` 的矩阵用例 |

也就是说:这四条能力**没有消费者**,而它承担的唯一职责(出站授权、私网拒绝、观察审计)**对真正在用的路径(JS 插件的 `node:http`)不生效**。这正是 AGENTS §4 第 11 条警告的形态——一个看起来存在、实际拦不住任何东西的边界,比没有边界更危险:它会让评审误以为「网络已受控」。

AGENTS §0 红线 6 要求「拒绝实现必须走 ADR」;删除同理。本 ADR 是这次删除的依据。

## 2. 决策

一次性删除以下内容:

| 类别 | 删除项 |
|---|---|
| 插件 | `plugins/seam/net`(145 行)、`plugins/provider/net-http`(184 行)、`plugins/policy/net-guard`(249 行)、`plugins/policy/net-observe`(113 行),含全部测试 |
| 示例 | `examples/net/`(preset + 3 个 manifest) |
| 文档 | `docs/seam-net.md` |
| caps | 能力命名空间 `net.fetch` / `net.fetch.<host>`(`platform/caps` 的语法校验) |
| 沙箱 | `seamsandbox.OpNetFetch` 及 `sandbox-none` 的两处拒绝分支 |
| 观测 | `audit-log` 的 `net/observe` 订阅;`ada inspect --chain` 的 `net` 链输出;`securitymatrix` 的 net 矩阵 |

不做的事:

- 不保留「以后可能用」的空 seam。需要时按 AGENTS §3 决策树重新建 seam + provider + policy,而不是留一个没有实现的接口。
- 不把授权逻辑搬到桥接层。ADR 0011 §3 已否决「复制一份策略」。
- 不改动 `node:http`/`https`/`dns`/`net` 的行为。

## 3. 替代方案

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 全部保留,只标注「可选」 | 文档写明 net-guard 非默认 | 零消费者的代码要一直维护、一直测试;而它对主路径无效,标注也无法消除「以为已受控」的误读 |
| B. 只删策略层,保留 `seam/net` + `net-http` | 去掉 `net-guard`/`net-observe`,留能力挂点 | `seam/net` 仍无消费者;留着的唯一作用是让将来的工具「看起来有门禁」,但门禁已被删除,反而更容易写出无策略的调用 |
| C. 本 ADR:全删 | 能力、策略、示例、文档一起删 | 仓库里网络只剩一条路径(Node 模块直连),没有第二种写法,也没有失效的门禁 |
| D. 反向:恢复 net-guard 并让 Node 模块转调 seam | 统一到受控路径 | 与 ADR 0011 的产品决定冲突(本地服务、内网 API、`localhost` 调试会全部失败),且要在同步运行时里补一层异步转接 |

## 4. 影响面

- **内置插件数**:Go 内置 51 → **47**(`platform/host/builtins`);TS 插件仍是 2 个,总数 49。
- **测试**:删除 `net-http`(8)、`net-guard`(14)、`net-observe`(4)、`TestNetMatrix`、`cli.TestUpNetStack`,以及 `caps`/`audit-log`/`sandbox-none` 里的 net 断言;新增 `TestNodeHTTPDNSNet`/`TestNodeHTTPError`/`TestNodeHTTPSInsecure` 已在 ADR 0011 落地。
- **破坏性**:`plugins/seam/net` 是公开 seam,删除后引用它的代码无法编译。仓库内已无引用;外部若引用,只能改用 `node:http`(JS)或标准库(Go)。
- **能力语义**:`net.fetch` 写入清单会被判为「未知命名空间」而校验失败,不会再出现「声明了但没人强制」的情况。
- **审计**:网络请求本就不在 `audit-log` 里(ADR 0011 已承认);删除 `net/observe` 后这一点在文档上不再有歧义。
- **门禁**:`scripts/guard.ps1` 与代码预算不受影响(`net-guard` 249 行未触及 800 行边界)。

## 5. 回滚方式

- `git revert` 本提交即可恢复四个插件、示例与文档;caps/sandbox 的删除项也在同一提交里,一并恢复。
- 若将来确有「按主机收口」的部署需求,不要恢复旧实现,应按 AGENTS §3 新建 `seam/net` + provider + policy,并**同时**让 Node 网络模块经过该 seam(即推翻 ADR 0011),否则仍会回到「两条路径、一条无门禁」的状态。

## 6. 复议条件

- 出现真实的消费者(例如 `tool/net-fetch`)且产品要求按主机授权;
- 产品要求「网络也进入审计」,即推翻 ADR 0011;
- 复议日期到达。
