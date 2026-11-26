// Package agent 是 agent-loop 对控制层的稳定契约。
//
// 它只暴露「创建会话 + 跑一轮」两个动作,避免控制层直接依赖 core/agent-loop
// 的具体类型,也让 Go 与 TS 的控制实现能围绕同一个接口测试。
package agent

import (
	"context"

	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// EventDelta 是模型增量观察事件(emit,不是门禁)。
//
// agent-loop 在每次 stream 回调里发出,带 session/turn/step 归属,
// 控制层据此把 token 级增量投影成 control/update;它不是模型可见状态,
// 也不会写进 session。
const EventDelta = "agent/delta"

// DeltaEvent 是 EventDelta 的载荷。
type DeltaEvent struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId,omitempty"`
	StepID    string `json:"stepId,omitempty"`
	Kind      string `json:"kind"`           // text / reasoning / tool_args
	Text      string `json:"text,omitempty"` // 只有 text 类增量带内容
}

// Result 是一轮执行的最终结果;流式片段由控制层事件负责。
type Result struct {
	TurnID    string `json:"turnId,omitempty"`
	Text      string `json:"text,omitempty"`
	Steps     int    `json:"steps,omitempty"`
	ToolCalls int    `json:"toolCalls,omitempty"`
}

// Runner 是 agent 执行能力;由 core/agent-loop 提供。
type Runner interface {
	Open(ctx context.Context, meta session.Meta) (session.Session, error)
	Run(ctx context.Context, sess session.Session, userText string) (Result, error)
}
