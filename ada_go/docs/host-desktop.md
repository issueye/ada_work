# host/desktop(视图映射骨架)

> **状态说明(2026-11-20)**:产品前提已改为桌面只做 WebView 壳(mygo WebView2 或 Electron)，不做 native UI。本文件的 native Renderer/View 映射骨架**不再是桌面方向**;新的桌面契约见 [ui-framework.md](ui-framework.md) §11.3 与 [ADR 0008](adr/0008-desktop-webview-shell.md)。保留本文件只作为既有实现与历史背景。
>
> 配套 [plugin-kernel-design.md](plugin-kernel-design.md) §12(宿主)与 AGENTS §2.2(宿主适配器)。

## 1. 范围

```
host/desktop/   内核快照 → 视图模型(View)→ Renderer
```

本包**不引入任何窗口/UI 依赖**(mygo 等),只定义映射契约;真实窗口由宿主在 Renderer 里实现。

## 2. 契约

| 类型 | 语义 |
|---|---|
| `Renderer` | `Render(View) error`;桌面宿主实现 |
| `View` | 只读视图:FiberRow / ServiceRow / EffectRow / Events |
| `New(app, renderer)` | app 与 renderer 都不能为 nil |
| `Refresh()` | `app.Inspect()` → View → Render;返回本轮视图 |
| `Subscribe(events)` | 在根上下文监听事件,事件到达时 Refresh;返回 Dispose 取消订阅 |
| `OpenPage(name)` | 发出 `page/open` 事件(供 `assemble.TriggerOnEvent` 延迟激活) |

## 3. 测试

- `desktop.TestRefresh`:挂载提供服务的插件后刷新,断言 fibers/services/effects 映射与渲染器调用次数(两次刷新);
- `desktop.TestSubscribe`:订阅 `fs/observe` → Emit 触发重绘 → Dispose 后不再触发;
- `desktop.TestOpenPage`:`page/open` 载荷与空页面名校验;
- `desktop.TestNewValidation`:nil app/renderer 明确报错。

## 4. 偏差与边界

1. **没有真实窗口**:没有 mygo/菜单/托盘/主线程;Renderer 只是接口。
2. **事件驱动刷新是可选订阅**:`Subscribe` 由宿主选择事件;重绘发生在事件派发回调里,渲染器必须快速返回。
3. **只读视图**:不能从 View 触发插件操作(命令/审批入口留空)。
4. **没有生命周期托管**:不负责 app 的启动/停止。
5. **快照是一次性的**:并发装配期间刷新可能看到中间状态(与 `Inspect` 语义一致)。

## 5. 下一步

1. mygo 窗口 Renderer + 事件订阅(节流重绘);
2. 命令面板/审批弹窗的映射(只做数据,不做业务);
3. 与 host/server 共享视图模型(若需要)。

## 6. 验证命令

```powershell
go test -race -count=1 ./host/desktop
go test -race -count=1 ./...
pwsh scripts/guard.ps1
```
