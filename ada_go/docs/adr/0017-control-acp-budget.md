# ADR 0017:control-acp 代码预算例外

- 状态:已接受(临时例外)
- 日期:2026-11-26
- 复议日期:2027-06-30
- 相关:[rpc-control-layers.md](../rpc-control-layers.md)、[seam-acp.md](../seam-acp.md)、AGENTS §5.3/§12、[ADR 0013](0013-tool-plugin-budget-exception.md)(同类先例);ADR 0009 已由 [ADR 0018](0018-remove-rpc-stack.md) 撤销

## 1. 背景

AGENTS §5.3 规定「单个插件 ≤ 800 行,单一职责」。`provider/control-acp` 实测 799 行生产代码(不含测试):

| 文件 | 行数 | 内容 |
|---|---|---|
| `control_acp.go` | 467 | 端点生命周期、session 方法、`session/update` 映射 |
| `permission.go` | 177 | `session/request_permission` ↔ `approval.Approver` |
| `acp_client.go` | 155 | `initialize` 能力记录 + `fs/*`、`terminal/*` 反向调用 |

它仍然只做一件事(实现 ACP 协议适配),但这件事实质包含三条生命周期不同的链路:

1. **会话生命周期**:`initialize` / `session/new|load|prompt|cancel`、stdio 连接与取消;
2. **更新映射**:assistant 文本/推理增量、工具调用、历史回放、控制面事件过滤;
3. **反向能力**:权限请求、客户端 fs/terminal 能力记录与 7 个调用。

压缩到 800 行以内只能靠删注释或把分支塞得更密,可审查性下降;而 ACP 的下一块能力
(MCP、按会话路由)必然继续增长。

## 2. 决策

在 **2027-06-30 前**,允许 `plugins/provider/control-acp` 的单插件生产代码上限为 **1000 行**,前提是:

1. 仍然一个插件、一个协议适配器,不混入 agent 循环、工具实现或传输层;
2. 新增功能先拆文件(权限、反向能力、映射各自独立),不得继续堆在单文件里;
3. 测试不计入该预算,但新增方法必须有契约/映射测试;
4. 超过 1000 行或出现第二个职责(例如把 MCP 客户端塞进同一插件)时,拆成
   `control-acp`(端点/会话)与 `acp-client`(反向能力)两个插件,而不是继续放宽。

## 3. 替代方案

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 拆成两个插件 | `control-acp` + `acp-client` | 反向能力脱离 ACP 连接没有独立产品价值;替换 ACP 实现时必须同时替换两个插件,装配与回滚更复杂 |
| B. 移入 `platform/`(L2) | 当作基础设施,不受插件预算约束 | ACP 是协议适配,不是装载/编译/执行机制;产品可以换成别的协议 |
| C. 压回 800 行 | 删注释、合并映射分支 | 降低可审查性,且下一次 ACP 功能立刻再次超限 |
| D. 本 ADR | 临时放宽到 1000 行,并约束拆分方向 | 保留一个可替换的 ACP 适配器;代价是一个插件的体量例外,有复议日期 |

## 4. 影响面

- 只影响 `plugins/provider/control-acp`;`seam/acp`(≤400)、`platform/host/acp` 仍按通用预算。
- `scripts/guard.ps1` 的插件预算表增加 `control-acp = 1000`;AGENTS §5.3 的例外列表同步。
- 外部契约(`control` 方法、ACP 方法)不因本 ADR 改变。

## 5. 回滚方式

- 复议时把反向能力拆成 `provider/acp-client`(复用 `seam/acp` 契约,由宿主持有连接),
  或把 `session/update` 映射移入独立插件;上限回到 800。
- 删除本 ADR 不改变已经落地的协议行为。

## 6. 复议条件

- `control-acp` 超过 1000 行,或出现第二个职责;
- 复议日期到达;
- ACP 协议新增 MCP 客户端等大块能力,需要评估是否拆包。
