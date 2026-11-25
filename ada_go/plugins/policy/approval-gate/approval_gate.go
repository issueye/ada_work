// Package approvalgate 是 `policy/approval-gate`:在 tools/pre-execute 里对高风险工具
// 做 fail-closed 审批。
//
// 审批不是 tools 的一部分(seam-api §3.4):gate 只在 waterfall 里问 approval 服务,
// 拒绝时返回错误,工具 handler 不会被调用。
package approvalgate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// PolicyName 是插件 id。
const PolicyName = "approval-gate"

// Priority 高于其它 pre-execute 策略(先审批,再改写参数)。
const Priority = 300

type config struct {
	Tools      []string `json:"tools,omitempty"`
	RequireAll bool     `json:"requireAll,omitempty"`
	Backend    string   `json:"backend,omitempty"`
	TimeoutMs  int      `json:"timeoutMs,omitempty"`
}

// Handler 返回 tools/pre-execute 的 waterfall handler。
//
// approval 为 nil 时,需要审批的工具一律拒绝(fail-closed)。
func Handler(approval *seamapproval.Registry, backend string, require []string, all bool, timeout time.Duration) kernel.WaterfallHandler {
	set := map[string]bool{}
	for _, name := range require {
		set[strings.TrimSpace(name)] = true
	}
	return func(ctx context.Context, c *kernel.Context, input any, next kernel.Next) (any, error) {
		call, ok := input.(*tools.CallInfo)
		if !ok || call == nil {
			return next(input)
		}
		if !all && !set[call.Name] {
			return next(input)
		}
		if approval == nil {
			return nil, seamerr.New(seamerr.ErrDenied, PolicyName, "没有 approval 服务,按拒绝处理", false)
		}
		req := seamapproval.Request{
			ID: "tool:" + call.Name, Source: "tool", Op: "tool.invoke",
			Target: call.Name, Reason: "工具需要审批: " + call.Name,
		}
		if timeout > 0 {
			req.Timeout = timeout
		}
		dec, err := seamapproval.Ask(ctx, approval, backend, req)
		if c != nil {
			allow := dec.Allow
			reason := dec.Reason
			if err != nil {
				allow, reason = false, err.Error()
			}
			payload := map[string]any{
				"source": "tool", "op": "tool.invoke", "target": call.Name,
				"allow": allow, "approver": dec.Approver, "reason": reason,
			}
			if id, ok := kernel.Principal(ctx); ok {
				payload["plugin"] = id
			}
			c.EmitContext(ctx, "approval/decided", payload)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", PolicyName, err)
		}
		if !dec.Allow {
			return nil, seamerr.New(seamerr.ErrDenied, PolicyName,
				"审批拒绝: "+call.Name+"("+dec.Reason+")", false)
		}
		return next(input)
	}
}

// Plugin 返回 approval-gate 插件:inject tools,可选 approval。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "approval-gate: 解析配置失败: %v", err)
		}
		var reg *seamapproval.Registry
		if v, ok := ctx.TryService("approval"); ok {
			reg, _ = v.(*seamapproval.Registry)
		}
		h := Handler(reg, conf.Backend, conf.Tools, conf.RequireAll, time.Duration(conf.TimeoutMs)*time.Millisecond)
		return ctx.OnWaterfall(tools.EventPreExecute, h, kernel.WithPriority(Priority)), nil
	}).WithInject("tools").WithOptional("approval")
}
