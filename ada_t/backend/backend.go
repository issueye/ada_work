// Package backend 定义渲染后端契约:widget 树进,原生呈现;事件回传。
//
// 渲染桥(rt)在任意 goroutine 上 Apply 最新树;后端自行决定呈现方式:
// mygo 后端把树重放为原生控件(GPU 绘制),headless 后端只留快照与操作日志。
package backend

import "github.com/issueye/ada_t/widget"

// WindowSpec 是启动时要创建的窗口描述(宽高为 DIP)。
type WindowSpec struct {
	ID     string
	Title  string
	Width  int
	Height int
	// BackgroundColor 是窗口底色(CSS 语法);空为系统默认。headless 忽略。
	BackgroundColor string
}

// DispatchFunc 是后端回传事件的入口:winID + handler id + 事件载荷。
type DispatchFunc func(winID, hid string, payload any)

// Backend 是渲染后端。实现必须线程安全(Apply 来自 rt 的 loop goroutine)。
type Backend interface {
	// Apply 提交窗口最新树;tree 为 nil 表示窗口已移除。
	Apply(winID string, tree *widget.Node)
	// Run 启动后端并阻塞(通常占主线程),直到 Stop;
	// windows 是启动窗口列表(已在 rt 侧挂载)。
	Run(windows []WindowSpec) error
	// Stop 请求退出 Run。可从任意 goroutine 调用。
	Stop() error
}

// Dispatcher 是能回传事件的后端可选实现的接口;
// App 在构造后注入 DispatchFunc。
type Dispatcher interface {
	SetDispatch(fn DispatchFunc)
}

// ToastSource 是能显示 toast 的后端可选接口;App 在构造后注入来源
// (rt 的 toast 队列:组件 ctx.toast 入队,后端重放帧里消费显示)。
type ToastSource interface {
	SetToastSource(fn func(winID string) []string)
}
