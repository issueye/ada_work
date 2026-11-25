package flow

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

// KernelRuntime 用 kernel.Context 实现 Runtime:
// 服务调用走 inject 门禁,事件走总线/waterfall,和普通插件调用同一条路径。
type KernelRuntime struct {
	ctx *kernel.Context
}

// NewKernelRuntime 在插件上下文里创建运行时。
func NewKernelRuntime(ctx *kernel.Context) *KernelRuntime {
	return &KernelRuntime{ctx: ctx}
}

// CallService 解析服务并调用它声明的 Invoker 方法。
func (k *KernelRuntime) CallService(ctx context.Context, service, method string, args map[string]any) (any, error) {
	value, err := k.ctx.Service(service)
	if err != nil {
		return nil, err
	}
	invoker, ok := value.(Invoker)
	if !ok {
		return nil, kernel.Fail(kernel.CodeInvalid,
			"flow: 服务 %q 不支持方法调用(需要实现 flow.Invoker)", service)
	}
	return invoker.Invoke(ctx, method, args)
}

// DispatchEvent 按模式派发事件。
func (k *KernelRuntime) DispatchEvent(ctx context.Context, name, mode string, input any) (any, error) {
	if name == "" {
		return nil, kernel.Fail(kernel.CodeInvalid, "flow: 事件名不能为空")
	}
	switch mode {
	case "", "emit":
		k.ctx.EmitContext(ctx, name, input)
		return input, nil
	case "waterfall":
		return k.ctx.Waterfall(ctx, name, input)
	}
	return nil, kernel.Fail(kernel.CodeInvalid, "flow: 未知事件模式 %q", mode)
}
