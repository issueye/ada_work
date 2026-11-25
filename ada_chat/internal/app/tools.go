package app

import "time"

// 工具调用是助手在回答过程中调用的本地能力。卡片长什么样属于界面，这里只放状态和数据。

type ToolState int

const (
	ToolRunning ToolState = iota
	ToolDone
	ToolFailed
)

// ToolCall 是一次调用：参数、结果、状态和耗时。ID 是模型给的 tool_call id，
// 同一轮里唯一；Open 是它在对话里是否展开。
type ToolCall struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Args   string        `json:"args,omitempty"`
	Result string        `json:"result,omitempty"`
	State  ToolState     `json:"state"`
	Took   time.Duration `json:"took,omitempty"`
	Open   bool          `json:"open,omitempty"`
}

// ToolEvent 是机器人进程发来的一个工具状态；Status 取 running/ok/denied/error/timeout。
type ToolEvent struct {
	ID       string
	Name     string
	Args     string
	Status   string
	Text     string
	Duration time.Duration
}

// Approval 是一次升级审批请求：工具或沙箱要越权时，runtime 经 RPC 让宿主决定；
// 宿主超时未答复时 runtime 按拒绝处理（fail-closed）。
type Approval struct {
	BotID   int
	Turn    int
	ID      string
	Source  string
	Op      string
	Target  string
	Reason  string
	Timeout time.Duration
}

// PartKind 是一轮回答里的内容块类型。
type PartKind int

const (
	PartText PartKind = iota
	PartReasoning
	PartTool
)

// Part 记录一轮回答里思考、文本与工具调用的先后顺序：模型可能先思考再调用工具，
// 也可能在工具结果之后继续写。Text 在 PartText / PartReasoning 上有值，
// ToolID 只在 PartTool 上有值；Open 只用于思考块的展开状态。
type Part struct {
	Kind   PartKind `json:"kind"`
	Text   string   `json:"text,omitempty"`
	ToolID string   `json:"toolId,omitempty"`
	Open   bool     `json:"open,omitempty"`
}
