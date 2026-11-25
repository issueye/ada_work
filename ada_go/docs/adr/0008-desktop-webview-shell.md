# ADR 0008:桌面只做 WebView 壳（mygo WebView2 / Electron），不做 native UI

- 状态:提案(待评审;通过后同步 AGENTS §10 与主设计 §14.3)
- 日期:2026-11-20
- 复议日期:2027-06-30
- 相关:[ui-framework.md](../ui-framework.md) §11.3、[host-desktop.md](../host-desktop.md)、[ADR 0001](0001-deferred-m5-scope.md)、AGENTS §10/§12

## 1. 背景

产品前提已改为:桌面只考虑 **mygo WebView2** 或 **Electron**，不考虑 mygo native 控件。现有 `host/desktop` 是 native Renderer/View 映射骨架(约 120 行)，与新前提不符;主设计 §14.3 的「mygo 把页面渲染成 native list/detail/form」也不再适用。

同时要面对两个事实:

1. **Electron 自带 Node**。它会把 Node 带进桌面宿主进程，与「无 Node」的强口径冲突;只能把 Node 限制在壳(main/preload)，renderer 不启用 Node。
2. **mygo WebView2** 仍是 mygo 依赖，且 WebView2 运行时是 Windows 侧能力;ADR 0001 的「不引入 mygo 原生窗口」不等于「可以无 ADR 引入 mygo WebView2」。

## 2. 决策

1. 桌面 = **一个 WebView 窗口加载同一份 SPA**;不做 native 控件映射，不做第二套页面 DSL。
2. 两条路线:
   - **A. mygo WebView2（首选）**:Go 进程 + WebView2 窗口;零 Node;Go 直接 Bind/typed event;单二进制。
   - **B. Electron（备选）**:Electron main + renderer;Node 只在壳;Go 内核作为本地 sidecar（localhost + 随机 token）;renderer `nodeIntegration=false`、`contextIsolation=true`、`sandbox=true`。
3. 两条路线共用 `Transport` 接口(`call/subscribe/respond`);浏览器用 HTTP/SSE，WebView2 用 Bind/事件，Electron 用 preload/contextBridge + IPC。运行时只认 Transport，不硬编码宿主。
4. 页面/插槽/组件与 Web 完全同一份产物;桌面不重新编译、不重新签名。
5. 桌面不作为 U0–U2 验收;U3 条件项，先做 1 周能力探测(History/import map/流式 IPC/WebView2 运行时)。
6. 「零 Node」口径:构建、插件编译/执行、Web 宿主零 Node;Electron 桌面壳是**显式例外**，选它就必须在安装/发布说明里写明「Node 只在壳」。要求绝对零 Node 时只能选 A。
7. ADR 0001 的 native 桌面延期不变;引入 mygo WebView2 仍需依赖 ADR/复议，不能借本 ADR 绕过。

## 3. 替代方案(至少两个)

| 方案 | 做法 | 否决理由 |
|---|---|---|
| A. mygo native UI | 继续用 native list/detail/form 渲染页面 | 与产品前提冲突;两套 UI 表达;复杂页面(文件树/画布)做不了 |
| B. 纯浏览器(不打包) | 桌面就是系统浏览器打开 `ada web` | 没有窗口/菜单/托盘/文件对话框;不是桌面产品 |
| C. Electron-only | Electron 里跑全部逻辑，不要 Go 内核 | 失去 Go 内核/插件/审批/审计;或把内核搬进 Node，违背现有架构 |
| D. mygo WebView2 + Electron 双壳 | 本 ADR | 满足产品前提;代价是双壳维护与 Node 例外，需条件项 |

## 4. 影响面

- `host/desktop`:从 native Renderer/View 改为 WebView Transport;不再做「页面 → native 控件」映射;预算另算。
- 打包:WebView2 需要运行时/安装器;Electron 需要 Node/Chromium 体积与签名;两者都要代码签名/自动更新策略。
- 安全:WebView2 的导航/新窗口/协议白名单;Electron 的 contextIsolation/sandbox/`will-navigate`/`setWindowOpenHandler`;renderer 不能拿 Node。
- 测试:桌面壳只做冒烟(启动、深链、IPC、CSP、更新);业务测试仍在 Web/Go。
- 文档:主设计 §14.3 的 native 规则要改为「WebView 壳」;host-desktop.md 需重写。
- 里程碑:U3 条件项;ADR 0001 复议时一起决定。

## 5. 回滚方式

删除桌面壳与 `host/desktop` 的 Transport 实现，只保留 Web 宿主;SPA/插件/Go 内核不受影响。Electron 的 Node 例外随壳一起删除。

## 6. 复议条件

- WebView2 在目标平台矩阵不可用/不可签名 → 转 Electron 或放弃桌面。
- Electron 的 Node 例外不被产品接受 → 只保留 A。
- 桌面壳开始承载业务/编排 → 立即回滚，业务回到插件。
