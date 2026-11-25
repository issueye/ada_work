// Package shellguard 是 shell 的策略装饰器:先问 sandbox,再做白名单与 shell 门。
//
// 判定拆成三层:沙箱(操作是否允许)→ shell 门(是否显式授权 shell 模式)→
// 只读白名单(读操作允许哪些程序)。这样可以复用 sandbox 后端,而不是各写一套模式。
package shellguard

import (
	"context"
	"fmt"
	"strings"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// PolicyName 是插件 id。
const PolicyName = "shell-guard"

// Priority 是装饰器优先级(高者在外层)。
const Priority = 100

// New 构造装饰器。approval 为 nil 表示没有审批服务:shell 门/白名单拒绝一律不放行。
func New(inner seamshell.Shell, registry *seamsandbox.Registry, backend string, allow []string, allowShell bool, approvalRegistry *seamapproval.Registry, approvalBackend string, emit func(string, any)) seamshell.Shell {
	return NewWithCaps(inner, registry, backend, allow, allowShell, approvalRegistry, approvalBackend, emit, nil)
}

// NewWithCaps 额外注入 caps.Broker:非 nil 时每条命令要求 principal + exec 能力。
func NewWithCaps(inner seamshell.Shell, registry *seamsandbox.Registry, backend string, allow []string, allowShell bool, approvalRegistry *seamapproval.Registry, approvalBackend string, emit func(string, any), broker *caps.Broker) seamshell.Shell {
	allowed := make(map[string]bool, len(allow))
	for _, p := range allow {
		allowed[p] = true
	}
	return &guard{
		inner: inner, registry: registry, backend: backend,
		allow: allowed, allowShell: allowShell,
		approval: approvalRegistry, approvalBackend: approvalBackend, emit: emit,
		broker: broker,
	}
}

type guard struct {
	inner      seamshell.Shell
	registry   *seamsandbox.Registry
	backend    string
	allow      map[string]bool
	allowShell bool
	broker     *caps.Broker

	approval        *seamapproval.Registry
	approvalBackend string
	emit            func(string, any)
}

func (g *guard) Env() seamshell.Env { return g.inner.Env() }

func (g *guard) Run(ctx context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	if err := g.check(ctx, cmd); err != nil {
		return seamshell.Result{}, err
	}
	return g.inner.Run(ctx, cmd)
}

func (g *guard) Stream(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	if err := g.check(ctx, cmd); err != nil {
		return seamshell.Result{}, err
	}
	return g.inner.Stream(ctx, cmd, onChunk)
}

func (g *guard) check(ctx context.Context, cmd seamshell.Command) error {
	if g.registry == nil {
		return seamshell.Denied("sandbox", "sandbox 服务不可用")
	}
	sb, err := g.registry.Get(g.backend)
	if err != nil {
		return seamshell.Denied("sandbox", "没有可用的沙箱后端: "+err.Error())
	}
	decision, err := sb.Resolve(ctx, seamsandbox.Request{
		Op: seamsandbox.OpShellExec, Program: cmd.Program, Args: cmd.Args,
	})
	if err != nil {
		return err
	}
	if !decision.Allow {
		reason := strings.Join(decision.Reasons, "; ")
		if reason == "" {
			reason = "被沙箱拒绝"
		}
		return seamshell.Denied("sandbox", reason)
	}

	if g.broker != nil {
		id, ok := kernel.Principal(ctx)
		if !ok {
			return seamshell.Denied("cap", "缺少调用者身份:请使用 ctx.Context()")
		}
		if err := g.broker.Authorize(ctx, id, "exec", cmd.Program); err != nil {
			return seamshell.Denied("cap", err.Error())
		}
	}

	gateReason := ""
	if cmd.Shell && !g.allowShell {
		gateReason = fmt.Sprintf("shell 模式未授权,拒绝执行 %q", cmd.Program)
	}
	if gateReason == "" && sb.Mode(ctx, "") == seamsandbox.ModeReadOnly && !g.allow[cmd.Program] {
		gateReason = fmt.Sprintf("只读模式只允许白名单命令,拒绝 %q", cmd.Program)
	}
	if gateReason == "" {
		return nil
	}

	// 策略层的拒绝也可升级:有审批人才问,否则 fail-closed。
	if g.approval != nil {
		dec, err := seamapproval.Ask(ctx, g.approval, g.approvalBackend, seamapproval.Request{
			Source: "shell", Op: "shell.exec", Target: cmd.Program, Reason: gateReason,
		})
		if g.emit != nil {
			allow := dec.Allow
			reason := dec.Reason
			if err != nil {
				reason = err.Error()
				allow = false
			}
			g.emit("approval/decided", map[string]any{
				"source": "shell", "target": cmd.Program, "allow": allow, "approver": dec.Approver, "reason": reason,
			})
		}
		if err != nil {
			return seamshell.Denied("approval", "审批失败: "+err.Error())
		}
		if dec.Allow {
			return nil
		}
		return seamshell.Denied("approval", dec.Reason)
	}
	return seamshell.Denied("sandbox", gateReason)
}

// Plugin 返回 shell-guard 插件:inject shell 与 sandbox,注册策略装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		registry, err := kernel.Service[*seamsandbox.Registry](ctx, "sandbox")
		if err != nil {
			return nil, err
		}
		backend := cfg.String("backend")
		var broker *caps.Broker
		if v, ok := ctx.TryService("caps"); ok {
			broker, _ = v.(*caps.Broker)
		}
		var approvalRegistry *seamapproval.Registry
		if v, ok := ctx.TryService("approval"); ok {
			if reg, ok := v.(*seamapproval.Registry); ok {
				approvalRegistry = reg
			}
		}
		return chain.Decorate(func(inner seamshell.Shell) seamshell.Shell {
			return NewWithCaps(inner, registry, backend, cfg.Strings("allow"), cfg.Bool("allowShell"),
				approvalRegistry, cfg.String("approvalBackend"),
				func(name string, payload any) { ctx.EmitContext(context.Background(), name, payload) }, broker)
		}, Priority)
	}).WithOptional("caps").WithInject("shell", "sandbox").WithOptional("approval")
}
