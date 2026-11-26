// Package coreplan 是 `core/plan`:会话级计划。
//
// 计划不是内存状态:每次变更写成 `plugin:plan/updated` session 事件,因此
// fork/resume/replay 都能恢复;agent-loop 在下一轮请求里注入最新计划,
// control-acp 把它映射成 ACP 的 plan 更新。
package coreplan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	planseam "github.com/issueye/ada_go_v2/plugins/seam/plan"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// PluginName 是插件 id 与工具来源。
const PluginName = "plan"

// 工具名。
const (
	setTool    = "plan_set"
	updateTool = "plan_update"
	showTool   = "plan_show"
)

// Plugin 返回 plan 插件:inject tools,注册 plan_set / plan_update / plan_show。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		defs := []tools.ToolDef{setDef(), updateDef(), showDef()}
		var disposers []kernel.Dispose
		for _, def := range defs {
			d, rerr := reg.Register(def)
			if rerr != nil {
				for _, prior := range disposers {
					_ = prior(context.Background())
				}
				return nil, rerr
			}
			disposers = append(disposers, d)
		}
		return func(c context.Context) error {
			var first error
			for i := len(disposers) - 1; i >= 0; i-- {
				if err := disposers[i](c); err != nil && first == nil {
					first = err
				}
			}
			return first
		}, nil
	}).WithInject("tools")
}

func setDef() tools.ToolDef {
	return tools.ToolDef{
		Name: setTool, Summary: "创建或整体替换当前会话的计划",
		Source: PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"steps"},
			"properties": map[string]any{
				"title": map[string]any{"type": "string", "description": "计划标题(可选)"},
				"steps": map[string]any{
					"type": "array", "minItems": 1,
					"items": map[string]any{
						"type": "object", "required": []any{"content"},
						"properties": map[string]any{
							"content":  map[string]any{"type": "string", "description": "步骤内容"},
							"status":   map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
							"priority": map[string]any{"type": "string", "enum": []any{"high", "medium", "low"}},
						},
					},
				},
			},
		}),
		Handler: setHandler,
	}
}

func updateDef() tools.ToolDef {
	return tools.ToolDef{
		Name: updateTool, Summary: "更新计划中某一步的状态(1 开始的序号)",
		Source: PluginName,
		Input: schema.FromMap(map[string]any{
			"type": "object", "required": []any{"index", "status"},
			"properties": map[string]any{
				"index":  map[string]any{"type": "integer", "minimum": 1, "description": "步骤序号,从 1 开始"},
				"status": map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
			},
		}),
		Handler: updateHandler,
	}
}

func showDef() tools.ToolDef {
	return tools.ToolDef{
		Name: showTool, Summary: "查看当前会话的计划",
		ReadOnly: true, Source: PluginName,
		Input:   schema.FromMap(map[string]any{"type": "object"}),
		Handler: showHandler,
	}
}

func setHandler(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	sess, bad := sessionOf(ctx, call)
	if sess == nil {
		return bad, nil
	}
	var p planseam.Plan
	if err := json.Unmarshal(call.Args, &p); err != nil {
		return failResult("参数解析失败: " + err.Error()), nil
	}
	normalized, err := planseam.Normalize(p)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if err := appendPlan(sess, normalized, call); err != nil {
		return failResult("写入计划失败: " + err.Error()), nil
	}
	return okResult("已更新计划\n" + planseam.Text(normalized)), nil
}

func updateHandler(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	sess, bad := sessionOf(ctx, call)
	if sess == nil {
		return bad, nil
	}
	var args struct {
		Index  int    `json:"index"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return failResult("参数解析失败: " + err.Error()), nil
	}
	status := planseam.Status(args.Status)
	if !status.Valid() {
		return failResult("status 非法: " + args.Status), nil
	}
	p, ok := latest(sess)
	if !ok {
		return failResult("还没有计划,先调用 " + setTool), nil
	}
	if args.Index < 1 || args.Index > len(p.Steps) {
		return failResult(fmt.Sprintf("index 超出范围: %d(共 %d 步)", args.Index, len(p.Steps))), nil
	}
	p.Steps[args.Index-1].Status = status
	if err := appendPlan(sess, p, call); err != nil {
		return failResult("写入计划失败: " + err.Error()), nil
	}
	return okResult("已更新计划\n" + planseam.Text(p)), nil
}

func showHandler(ctx context.Context, call tools.CallInfo) (tools.Result, error) {
	sess, bad := sessionOf(ctx, call)
	if sess == nil {
		return bad, nil
	}
	p, ok := latest(sess)
	if !ok {
		return okResult("当前没有计划"), nil
	}
	return okResult(planseam.Text(p)), nil
}

// sessionOf 取调用作用域的会话;显式 SessionID 与会话不一致时拒绝。
func sessionOf(ctx context.Context, call tools.CallInfo) (session.Session, tools.Result) {
	sess, ok := session.FromContext(ctx)
	if !ok || sess == nil {
		return nil, failResult("缺少会话上下文:plan 工具只能由 agent-loop 调用")
	}
	if call.SessionID != "" && call.SessionID != sess.ID() {
		return nil, failResult("会话上下文不一致: " + call.SessionID + " != " + sess.ID())
	}
	return sess, tools.Result{}
}

func appendPlan(sess session.Session, p planseam.Plan, call tools.CallInfo) error {
	_, err := sess.Append(session.Event{
		Type: planseam.EventUpdated, TurnID: call.TurnID, StepID: call.StepID,
		Data: planseam.Encode(p), Source: PluginName, Ignorable: true,
	})
	return err
}

// latest 从会话历史里取最后一条计划事件;不存在或损坏时返回 false。
func latest(sess session.Session) (planseam.Plan, bool) {
	events, err := sess.Events(0, 0)
	if err != nil {
		return planseam.Plan{}, false
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != planseam.EventUpdated {
			continue
		}
		if p, derr := planseam.Decode(events[i].Data); derr == nil {
			return p, true
		}
	}
	return planseam.Plan{}, false
}

func okResult(text string) tools.Result   { return result(tools.StatusOK, text) }
func failResult(text string) tools.Result { return result(tools.StatusError, text) }

func result(status tools.Status, text string) tools.Result {
	return tools.Result{
		Status:  status,
		Content: []llm.Block{{Kind: llm.BlockText, Text: text}},
	}
}
