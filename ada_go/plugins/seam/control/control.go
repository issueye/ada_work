// Package control 是五层架构中的控制层契约。
//
// 它只描述「对 agent 会话的控制操作」,不包含线协议、分帧、stdio 或传输细节。
// 为了让 Go 实现与 TS 插件实现共用同一份契约,控制层的 kernel 服务值是
// Invoker(JSON 方法调用),而不是只能由 Go 实现的强类型接口:
//
//   - Go 实现:实现 Invoker,直接提供 "control" 服务;
//   - TS 实现:插件 ctx.provide("control", { invoke(method, params) {...} }),
//     bridge 会把它包装成 Invoker 后再注册,插件系统本身不变。
//
// 协议适配层(control-rpc-v1 / control-acp)只依赖 Client,不关心底层是 Go 还是 TS。
package control

import (
	"context"
	"encoding/json"
	"time"

	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ServiceName 是控制层在 kernel 中注册的服务名。
const ServiceName = "control"

// 控制方法名。Go 与 TS 实现必须实现同一组方法。
const (
	MethodSessionCreate     = "session.create"
	MethodSessionLoad       = "session.load"
	MethodSessionList       = "session.list"
	MethodSessionPrompt     = "session.prompt"
	MethodSessionHistory    = "session.history"
	MethodSessionCancel     = "session.cancel"
	MethodSessionPermission = "session.permission"
	MethodSessionClose      = "session.close"
)

// EventUpdate 是控制层流式更新事件;TS 实现通过 ctx.emit 发送,
// Go 实现通过 kernel 事件发送,协议适配器订阅后映射成 ACP session/update。
const EventUpdate = "control/update"

// 控制层流式更新的 kind(Update.Kind);data 都是 {"text":...}。
const (
	// UpdateAssistantDelta 是 assistant 文本增量。
	UpdateAssistantDelta = "assistant/delta"
	// UpdateAssistantReasoning 是推理/思考增量,与正文分开,便于客户端折叠显示。
	UpdateAssistantReasoning = "assistant/reasoning"
)

// Invoker 是控制层的语言中立服务值。
//
// params/result 都是 JSON;禁止在这里出现具体协议的帧或 stdio 对象。
type Invoker interface {
	Invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)
}

// SessionInfo 是会话的只读元数据。
type SessionInfo struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// CreateRequest 创建一个新会话。
type CreateRequest struct {
	Title string         `json:"title,omitempty"`
	Cwd   string         `json:"cwd,omitempty"`
	Meta  map[string]any `json:"meta,omitempty"`
}

// PromptRequest 是一次用户输入。
type PromptRequest struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
}

// TurnResult 是一轮 prompt 的最终结果。流式片段通过 EventUpdate 发送。
type TurnResult struct {
	ID         string `json:"id,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Text       string `json:"text,omitempty"`
}

// Update 是控制层的流式更新;Kind 由控制层定义,ACP 适配器再映射成 session/update。
type Update struct {
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// 权限选项的语义类别;协议适配器据此判断「允许 / 拒绝」,不靠文案猜。
const (
	OptionAllowOnce    = "allow_once"
	OptionAllowAlways  = "allow_always"
	OptionRejectOnce   = "reject_once"
	OptionRejectAlways = "reject_always"
)

// PermissionOption 是权限请求中的一个选项。Kind 取上面的常量;
// 为空时适配器回退到 ID,仍无法判断时按拒绝处理(fail-closed)。
type PermissionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind,omitempty"`
}

// PermissionRequest 是 agent 向 client 发起的权限请求。
type PermissionRequest struct {
	SessionID  string `json:"sessionId"`
	Tool       string `json:"tool,omitempty"`
	ToolCallID string `json:"toolCallId,omitempty"`
	// Backend 指定审批人;空表示由 approval 取默认审批人。
	// ACP 场景下调用方应显式传 control-acp 的审批人名,避免与宿主审批人混淆。
	Backend string             `json:"backend,omitempty"`
	Title   string             `json:"title"`
	Detail  string             `json:"detail,omitempty"`
	Options []PermissionOption `json:"options,omitempty"`
}

// PermissionDecision 是 client 对权限请求的回答。
type PermissionDecision struct {
	OptionID  string `json:"optionId,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// Client 是控制层的类型化门面;协议适配器与 core 插件都通过它调用控制层。
type Client struct {
	inv Invoker
}

// New 包装一个 Invoker;inv 为 nil 时调用返回 EUNIMPLEMENTED。
func New(inv Invoker) *Client { return &Client{inv: inv} }

// Invoker 返回底层接口(诊断用)。
func (c *Client) Invoker() Invoker {
	if c == nil {
		return nil
	}
	return c.inv
}

func (c *Client) invoke(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c == nil || c.inv == nil {
		return nil, Unimplemented("没有安装控制层实现")
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, Invalid("控制层参数编码失败: " + err.Error())
	}
	return c.inv.Invoke(ctx, method, b)
}

func (c *Client) Create(ctx context.Context, req CreateRequest) (SessionInfo, error) {
	out, err := c.invoke(ctx, MethodSessionCreate, req)
	if err != nil {
		return SessionInfo{}, err
	}
	var info SessionInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return SessionInfo{}, Invalid("session.create 结果非法: " + err.Error())
	}
	return info, nil
}

// Load 按 id 恢复会话。cwd 可选:ACP session/load 带 cwd 时传入,
// 由控制层重新解析工作区;不传则保持会话已有的 meta。
func (c *Client) Load(ctx context.Context, id string, cwd ...string) (SessionInfo, error) {
	params := map[string]string{"sessionId": id}
	if len(cwd) > 0 && cwd[0] != "" {
		params["cwd"] = cwd[0]
	}
	out, err := c.invoke(ctx, MethodSessionLoad, params)
	if err != nil {
		return SessionInfo{}, err
	}
	var info SessionInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return SessionInfo{}, Invalid("session.load 结果非法: " + err.Error())
	}
	return info, nil
}

func (c *Client) List(ctx context.Context) ([]SessionInfo, error) {
	out, err := c.invoke(ctx, MethodSessionList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var list []SessionInfo
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, Invalid("session.list 结果非法: " + err.Error())
	}
	return list, nil
}

func (c *Client) Prompt(ctx context.Context, req PromptRequest) (TurnResult, error) {
	out, err := c.invoke(ctx, MethodSessionPrompt, req)
	if err != nil {
		return TurnResult{}, err
	}
	var result TurnResult
	if err := json.Unmarshal(out, &result); err != nil {
		return TurnResult{}, Invalid("session.prompt 结果非法: " + err.Error())
	}
	return result, nil
}

// History 返回会话历史更新(按 seq 升序),供 ACP session/load 等场景回放。
//
// 返回的是控制层更新而不是具体协议消息;适配器负责再映射。
func (c *Client) History(ctx context.Context, sessionID string) ([]Update, error) {
	out, err := c.invoke(ctx, MethodSessionHistory, map[string]string{"sessionId": sessionID})
	if err != nil {
		return nil, err
	}
	var list []Update
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, Invalid("session.history 结果非法: " + err.Error())
	}
	return list, nil
}

func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	_, err := c.invoke(ctx, MethodSessionCancel, map[string]string{"sessionId": sessionID})
	return err
}

func (c *Client) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionDecision, error) {
	out, err := c.invoke(ctx, MethodSessionPermission, req)
	if err != nil {
		return PermissionDecision{}, err
	}
	var decision PermissionDecision
	if err := json.Unmarshal(out, &decision); err != nil {
		return PermissionDecision{}, Invalid("session.permission 结果非法: " + err.Error())
	}
	return decision, nil
}

// Close 关闭一个会话;会话 id 必填,关闭的是会话而不是控制层本身。
func (c *Client) Close(ctx context.Context, sessionID string) error {
	_, err := c.invoke(ctx, MethodSessionClose, map[string]string{"sessionId": sessionID})
	return err
}

// 错误构造:与其它 seam 一致,带码、可跨边界。
func Invalid(msg string) error {
	return seamerr.New(seamerr.ErrInvalid, "control", msg, false)
}

func Denied(msg string) error {
	return seamerr.New(seamerr.ErrDenied, "control", msg, false)
}

func Unavailable(msg string) error {
	return seamerr.New(seamerr.ErrUnavailable, "control", msg, true)
}

func Unimplemented(msg string) error {
	return seamerr.Unimplemented("control", msg)
}
