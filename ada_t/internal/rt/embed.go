package rt

import _ "embed"

// surfaceJS 是 ada:surface 运行时(响应式 + Plan 解释器 + 事件分发),
// 在 moejs 里加载;源文件只服务于嵌入与 review,不参与 Go 编译。
//
//go:embed surface.js
var surfaceJS string
