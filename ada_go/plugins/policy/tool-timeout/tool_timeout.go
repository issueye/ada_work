// Package tooltimeout 是 `policy/tool-timeout`:在 tools/execute 上收紧/放宽单次工具超时。
//
// 它只改 Execution.Timeout,不替换 handler;实际终止仍由 tools seam 负责。
package tooltimeout

import (
	"context"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// PolicyName 是插件 id。
const PolicyName = "tool-timeout"

// Priority 高于 tools 的默认执行。
const Priority = 250

type config struct {
	DefaultMs int            `json:"defaultMs,omitempty"`
	Tools     map[string]int `json:"tools,omitempty"`
}

// Handler 返回 tools/execute waterfall handler。
func Handler(defaultMs int, perTool map[string]int) kernel.WaterfallHandler {
	return func(ctx context.Context, _ *kernel.Context, input any, next kernel.Next) (any, error) {
		ex, ok := input.(*tools.Execution)
		if !ok || ex == nil || ex.Call == nil {
			return next(input)
		}
		ms := 0
		if perTool != nil {
			ms = perTool[ex.Call.Name]
		}
		if ms <= 0 {
			ms = defaultMs
		}
		if ms > 0 {
			ex.Timeout = time.Duration(ms) * time.Millisecond
		}
		return next(input)
	}
}

// Plugin 返回 tool-timeout 插件:inject tools,注册 execute waterfall。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := kernel.Service[*tools.Registry](ctx, "tools")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "tool-timeout: 解析配置失败: %v", err)
		}
		h := Handler(conf.DefaultMs, conf.Tools)
		return ctx.OnWaterfall(tools.EventExecute, h, kernel.WithPriority(Priority)), nil
	}).WithInject("tools")
}
