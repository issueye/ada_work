package controlacp

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

// ApproverName 是 ACP client 在 approval 注册表里的名字。
//
// 调用方用 control.PermissionRequest.Backend 指定它,避免与宿主审批人(CLI 弹窗等)
// 按默认排序误选。
const ApproverName = "acp-client"

// defaultOptions 是未显式给选项时展示给 ACP client 的选项。
var defaultOptions = []control.PermissionOption{
	{ID: control.OptionAllowOnce, Label: "允许一次", Kind: control.OptionAllowOnce},
	{ID: control.OptionRejectOnce, Label: "拒绝一次", Kind: control.OptionRejectOnce},
}

// DefaultPermissionOptions 返回默认选项的副本,调用方可安全修改。
func DefaultPermissionOptions() []control.PermissionOption {
	out := make([]control.PermissionOption, len(defaultOptions))
	copy(out, defaultOptions)
	return out
}

type acpOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func acpOptions(opts []control.PermissionOption) []acpOption {
	out := make([]acpOption, 0, len(opts))
	for _, o := range opts {
		kind := o.Kind
		if kind == "" {
			kind = o.ID
		}
		name := o.Label
		if name == "" {
			name = o.ID
		}
		out = append(out, acpOption{OptionID: o.ID, Name: name, Kind: kind})
	}
	return out
}

type permissionResult struct {
	Outcome struct {
		Outcome  string `json:"outcome"`
		OptionID string `json:"optionId"`
	} `json:"outcome"`
}

// RequestPermission 以 ACP 的 session/request_permission 向 client 发起请求。
//
// 这是服务端到客户端的反向调用:适配器等待 client 选择选项;
// 连接关闭、client 取消或调用方超时都返回错误,由 approval 侧按拒绝处理。
func (e *Endpoint) RequestPermission(ctx context.Context, req control.PermissionRequest) (control.PermissionDecision, error) {
	if e == nil || e.conn == nil {
		return control.PermissionDecision{}, control.Unavailable("ACP 连接不可用")
	}
	if req.SessionID == "" {
		return control.PermissionDecision{}, control.Invalid("ACP 权限请求需要 sessionId")
	}
	options := req.Options
	if len(options) == 0 {
		options = DefaultPermissionOptions()
	}
	toolCallID := req.ToolCallID
	if toolCallID == "" {
		toolCallID = req.Tool
	}
	if toolCallID == "" {
		toolCallID = "tool"
	}
	title := req.Title
	if title == "" {
		title = req.Tool
	}
	if title == "" {
		title = "需要授权"
	}
	params := map[string]any{
		"sessionId": req.SessionID,
		"toolCall":  map[string]any{"toolCallId": toolCallID, "title": title},
		"options":   acpOptions(options),
	}
	var res permissionResult
	if err := e.conn.Call(ctx, "session/request_permission", params, &res); err != nil {
		return control.PermissionDecision{}, err
	}
	switch res.Outcome.Outcome {
	case "selected":
		if res.Outcome.OptionID == "" {
			// 没有选项的 selected 不可判定,按 fail-closed 处理成取消。
			return control.PermissionDecision{Cancelled: true}, nil
		}
		return control.PermissionDecision{OptionID: res.Outcome.OptionID}, nil
	case "cancelled":
		return control.PermissionDecision{Cancelled: true}, nil
	default:
		return control.PermissionDecision{}, control.Invalid("ACP 权限响应非法: " + res.Outcome.Outcome)
	}
}

// Approver 返回把审批请求转成 ACP 权限请求的 approval.Approver。
func (e *Endpoint) Approver() approval.Approver {
	return &approver{ep: e}
}

// BindApproval 把 ACP client 注册为 approval 审批人。
//
// 由宿主装配层决定是否调用:ACP 会话应当把权限交给编辑器,
// 而不是宿主内置的审批人。返回的 Dispose 用于卸载。
func (e *Endpoint) BindApproval(ctx *kernel.Context) (kernel.Dispose, error) {
	reg, err := kernel.Service[*approval.Registry](ctx, "approval")
	if err != nil {
		return nil, err
	}
	return reg.Register(e.Approver())
}

type approver struct {
	ep *Endpoint
}

func (a *approver) Name() string { return ApproverName }

// Approve 实现 approval.Approver:把 approval.Request 映射成 ACP 权限请求。
//
// 只有 client 明确选择了 allow_* 选项才放行;取消、未知选项、连接错误一律拒绝。
func (a *approver) Approve(ctx context.Context, req approval.Request) (approval.Decision, error) {
	sessionID, _ := req.Details["sessionId"].(string)
	// 工具审批发生在 agent-loop 的会话 ctx 内;approval.Request.Details 不是必填,
	// 因此以 ctx 里的会话兜底,避免 fs/shell/gate 少传字段时 ACP 权限直接失败。
	if sessionID == "" {
		if sess, ok := session.FromContext(ctx); ok {
			sessionID = sess.ID()
		}
	}
	toolCallID, _ := req.Details["toolCallId"].(string)
	options := DefaultPermissionOptions()
	dec, err := a.ep.RequestPermission(ctx, control.PermissionRequest{
		SessionID:  sessionID,
		Tool:       req.Op,
		ToolCallID: toolCallID,
		Title:      req.Target,
		Detail:     req.Reason,
		Options:    options,
	})
	if err != nil {
		return approval.Decision{Allow: false, Reason: err.Error(), Approver: ApproverName}, err
	}
	if dec.Cancelled {
		return approval.Decision{Allow: false, Reason: "客户端取消了权限请求", Approver: ApproverName}, nil
	}
	for _, o := range options {
		if o.ID == dec.OptionID {
			return approval.Decision{Allow: isAllow(o), Reason: o.ID, Approver: ApproverName}, nil
		}
	}
	return approval.Decision{Allow: false, Reason: "未知权限选项: " + dec.OptionID, Approver: ApproverName}, nil
}

// isAllow 只认 allow_once / allow_always;ID 与 Kind 任一命中即可。
func isAllow(o control.PermissionOption) bool {
	switch o.Kind {
	case control.OptionAllowOnce, control.OptionAllowAlways:
		return true
	}
	switch o.ID {
	case control.OptionAllowOnce, control.OptionAllowAlways:
		return true
	}
	return false
}

// 编译期确认 approver 满足 seam 契约。
var _ approval.Approver = (*approver)(nil)
