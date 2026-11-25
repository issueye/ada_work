# ADR 0011:Node 网络模块直连,不接入 net-guard

> 迁移说明(2026-11-25):执行层的 `compile`/`jsrt` 已抽到同级独立库 **ada_jsrt**;下文路径中的 `platform/jsrt` 现位于该库的 `jsrt` 包,决策与行为不变。

- 状态:已接受(产品决定优先于通用安全假设)
- 日期:2026-11-23
- 复议日期:2027-06-30
- 相关:[node-compat.md](../node-compat.md)、[ADR 0012](0012-remove-seam-net.md)(撤销本 ADR 的保留条款)、AGENTS §0 红线 3/6/§2.2 P7

## 1. 背景

`node:fs` 的接入方式是:只有当 fs/workspace 链上没有策略装饰器时才安装,
否则 `import "node:fs"` 直接失败。这样 `fs-sandbox`、`caps-gate`、
`workspace-guard` 不会被 Node 路径绕过(见 [node-compat.md](../node-compat.md) §3)。

按同一推理,`node:http`/`https`/`net`/`dns` 最初也被拒绝安装:
它们会绕开 `seam/net`、`net-guard` 的授权、私网拒绝、重定向逐跳校验与审计。

产品方给出的约束改变了前提:

> 只需要处理文件和写入的权限;网络不存在文件改动的问题,不做网络的限制。

也就是说,安全边界的目标是**本地文件与写入**,而不是「所有出网」。
把网络纳入同一道审批,会让脚本为一次只读的 HTTP GET 付出与写文件相同的代价,
而它并不改变任何本地状态。

## 2. 决策

在 **2027-06-30 前**,`node:http`、`node:https`、`node:dns`、`node:dns/promises`、
`node:net` 由桥接层直接调用 Go 标准库(`net/http`、`net`),**不经过** `seam/net`、
`net-guard` 与 `caps`。

保留的约束:

1. 只支持 `http`/`https`;`node:net` 只做 TCP(无 UDP、无原始套接字、无 `listen`);
2. 请求体/响应体上限 4 MiB,跨 JSON 边界一律 base64,遵守 AGENTS §5.6;
3. 所有调用都继承调用级 `ctx`:工具超时、取消与插件卸载会中断在途请求;
4. 仍是宿主注入(`__adaHost.httpDo` / `dnsLookup` / `net.*`),脚本没有直接 syscall;
5. `fs` 的边界完全不变:`node:fs` 继续受 fs/workspace 策略约束。

明确不做的:

- 不提供 `http.createServer`、`net.createServer`、`dns.setServers`;
- 不提供 `node:tls` 的原始握手;
- 不把 `seam/net` 的 `net-guard` 逻辑复制一份到桥接层——那会变成两套规则。

## 3. 替代方案

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 继续拒绝安装 | 只保留 `seam/net` | 与产品方明确要求冲突;脚本作者要为一次 GET 改写成服务调用 |
| B. 走 `seam/net` + `net-guard` | Node 模块内部转调 seam | `net-guard` 默认拒绝私网与未授权主机;本地测试服务、内网 API、`localhost` 调试全部失败,而它们与文件安全无关 |
| C. 复制一份策略到桥接层 | 在 `nodeHTTPDo` 里再做一次授权 | 两套规则必然漂移;真正的策略应留在 `net-guard` 一处 |
| D. 本 ADR | 直连标准库,只保留能力性限制 | 与「约束只针对文件与写入」一致;代价是 Node 网络路径不受 `net-guard` 审计 |

## 4. 影响面

> **更新(2026-11-23)**:本节「保留 `seam/net` 与 `net-guard`」的条款已由 [ADR 0012](0012-remove-seam-net.md) 撤销 —— 四个插件已一并删除。下面保留原始记录,便于追溯当时为什么先保留、后来为什么删。

- 新增代码:`platform/jsrt/nodenet.go`(参数解码与句柄)、`platform/bridge/node_net.go`
  (标准库调用)、`platform/jsrt/nodemods/src/{http,https,dns,dns_promises,net}.js`。
- ~~`seam/net` 与 `net-guard` 的行为不变;需要授权/审计的产品仍应让插件用 `seam/net`。~~
  → 已删除,见 ADR 0012。
- 审计:Node 网络请求**不会**出现在任何拒绝/放行记录里。这是本决策的
  已知代价,评审时必须承认,不能声称「全链路可审计」。
- 若某个产品需要收口,正确做法是新增一个 preset 开关(禁用 Node 网络模块),
  或按 ADR 0012 §5 重新建 seam 并让 Node 模块经过它;不要在桥接层加 `if`。

## 5. 回滚方式

- 删除 `platform/bridge/plugin.go` 里 `Host.HTTPDo`/`DNSLookup`/`NetDial` 的注入,
  以及在 `nodemods.go` 中移除 5 个模块名:链接期立即恢复为「模块不存在」,
  脚本会明确失败,不会静默降级。
- JS 源码与测试可以保留,作为将来 policy-aware 适配器的参考实现。

## 6. 复议条件

- 产品要求恢复「网络也受授权/审计」(此时按 ADR 0012 §5 重建 seam,并推翻本 ADR);
- 复议日期到达。
