# ADR 0005:把 UI 编译器与浏览器运行时放进 L2 基础设施

- 状态:提案(待评审;通过后同步 AGENTS §2.2/§5.3/§5.4)
- 日期:2026-11-20
- 复议日期:2027-06-30
- 相关:[ui-framework.md](../ui-framework.md)、[plugin-kernel-design.md](../plugin-kernel-design.md) §2/§3、AGENTS §2.2/§5.3/§5.4/§12

## 1. 背景

需求是「无 Node 环境、SPA、插件化、可远程加载、组件/组件包实现一项功能」。现有仓库的全部编译/执行机制都在 L2(esbuild 的 `compile`、moejs 的 `jsrt`、`plugin`、`caps`)，前端没有对应物。若把 UI 编译器/运行时放进插件或宿主，会出现三个问题:

1. **鸡生蛋**:UI 插件要先被编译才能装载，而编译能力本身若是插件，就要先激活它;激活又依赖 UI 运行时。装配顺序无法自洽。
2. **无法静态审计**:清单里的页面/插槽/能力要在不执行代码时列出;编译若在运行时插件里做，安装期就无法验签产物。
3. **宿主耦合**:把运行时塞进 `host/web` 会让 SPA 只能跑在 web 宿主，mygo/desktop 复用同一套组件时要么复制、要么反接;`host` 的 800 行预算也不够。

同时，UI 运行时只认组件、信号、路由、作用域，不认识 agent/产品;它与 `assemble`/`jsrt` 同属「装载/编译/执行机制」，没有领域语义。

## 2. 决策

1. `ui/`(Go 编译器 + 产物校验 + 表达式/模板/样式)归 **L2 基础设施**，不是插件。
2. `ui/runtime/`(纯 ESM 浏览器运行时 + Go embed 资产)归 **L2 基础设施**，由 `host/web` 与 `host/desktop` 共同消费，不绑定某个宿主。
3. `host/web` 是 **宿主适配器**:只做 HTTP/SSE/深链/动作桥/同源资产，不做编译、不做 UI 语义。
4. `plugins/ui/*`(Shell、设置、检查器、页面)是 **UI 插件**，走现有清单/安装/信任/装配管线。
5. 主线不新增第三方依赖:Go 侧复用 esbuild Go API 与标准库;浏览器侧零依赖;测试用 OpAdapter + JS 引擎(U0 先 spike,首选 moejs 直跑;不可行则 chromedp 仅作备选且另立依赖 ADR,仍不引 Node)。
6. 预算:`ui/` ≤ 5000 行(拆分见设计文档 §15);`host/web` ≤ 800 行(host 总预算例外见 ADR 0007);运行时 ≤ 35 KiB gzip;超预算按 AGENTS §12 另立 ADR。
7. 评审通过后:AGENTS §2.2 基础设施内容加入 `ui/`;§5.3 增加 `ui/` 预算;§5.4 增加 `plugins/ui/` 目录;`docs/ui-framework.md` 从提案转为契约。

## 3. 替代方案(至少两个)

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. 运行时作为 `host/web` 资产 | 把 JS 运行时与编译器都塞进 host/web | SPA 与 web 宿主焊死;desktop 无法复用;host 预算(每个适配器 ≤800 行)必然突破;编译发生在宿主层，违反「host 只做映射」 |
| B. 编译器做成 UI 插件 | 先装一个 `ui-compiler` 插件，再编译其他 UI 插件 | 鸡生蛋:编译插件本身要运行时;安装期无法验签;静态审计失效 |
| C. Node/Vite + 运行时编译 | 引入 Node 构建链，浏览器 `new Function` 编译模板 | 直接违反「无 Node」与 AGENTS §5.6(禁止 `eval`);CSP 需 `unsafe-eval`;远程插件无法在验签后执行 |
| D. 只做宿主渲染的声明式页面 | 不支持 `.aui`，全部由宿主画 | 满足不了「组件/组件包实现一项功能」;复杂页面无法表达;插件作者被锁死 |

## 4. 影响面

- 分层:新增 L2 包 `ui/`;`host/web` 新适配器;`plugins/ui/` 新目录约定;`kernel/`、`jsrt/`、`compile/`、`expr/` 零改动。
- 契约:清单新增 `kind=ui` 与 `ui` 段;`contributes.pages` 增加 `component`/`order`;`contributes.actions` 增加 `ui`;`caps` 语法不变(`ui` 已是合法能力)。
- 安全:编译不在浏览器发生;信任边界仍在 Go 的 caps/signature;同域运行 = 完全可信，未签名第三方只能声明式或隔离(设计文档 §9)。
- 测试:新增编译 golden、运行时一致性(OpAdapter + 引擎 spike)、桥接拒绝矩阵、资产/深链/签名用例;不新增 Node 工具链。
- 里程碑:新增 U0–U3 子轨道(设计文档 §16)，不改 M5 验收。

## 5. 回滚方式

删除 `ui/`、`host/web/`、`plugins/ui/` 与相关清单字段;SPA 不发布;Go 内核、CLI、既有插件与测试不受影响。清单新增字段为可选，旧插件无需迁移。

## 6. 复议条件

- U0 spike 证明:Go 编译 Plan 可 golden;OpAdapter + 引擎能跑运行时;浏览器无 `eval` 可完成渲染;WebView2/Electron 支持 import map/History/IPC(或明确降级集)。
- 若 U0 不成立:回滚到方案 A(host 资产)或只做声明式页面，并关闭本 ADR。
- 若 `ui/` 行数或运行时体积持续超预算:拆包或另立 ADR，不静默超支。
