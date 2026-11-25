// Package shellobserve 是 shell 的观察策略:每次执行后广播 shell/observe。
//
// 观察不是门禁:失败/被拒绝的调用也会记录。priority=250(最外层)。
// 事件里只放 program,不放 args(参数可能含 secret)。
package shellobserve

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// PolicyName 是插件 id。
const PolicyName = "shell-observe"

// Priority 高于 caps-gate(200):记录到被拒绝的调用。
const Priority = 250

// Record 是一次 shell 调用的观察记录。
type Record struct {
	Op       string // run | stream
	Program  string
	ExitCode int
	Bytes    int64
	Duration int64 // 毫秒
	Plugin   string
	Err      string
}

// Emitter 接收记录;插件实现里转成 shell/observe 事件。
type Emitter func(context.Context, Record)

// New 构造观察装饰器;emit 为 nil 或 skipOps 命中时只透传。
func New(inner seamshell.Shell, emit Emitter, skipOps []string) seamshell.Shell {
	skip := make(map[string]bool, len(skipOps))
	for _, op := range skipOps {
		skip[op] = true
	}
	return &observer{inner: inner, emit: emit, skip: skip}
}

type observer struct {
	inner seamshell.Shell
	emit  Emitter
	skip  map[string]bool
}

func (o *observer) record(ctx context.Context, rec Record, err error) {
	if o.emit == nil || o.skip[rec.Op] {
		return
	}
	if id, ok := kernel.Principal(ctx); ok {
		rec.Plugin = id
	}
	if err != nil {
		rec.Err = err.Error()
	}
	o.emit(ctx, rec)
}

func (o *observer) Run(ctx context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	res, err := o.inner.Run(ctx, cmd)
	o.record(ctx, Record{
		Op: "run", Program: cmd.Program, ExitCode: res.ExitCode,
		Bytes:    int64(len(res.Stdout) + len(res.Stderr)),
		Duration: res.Duration.Milliseconds(),
	}, err)
	return res, err
}

func (o *observer) Stream(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	res, err := o.inner.Stream(ctx, cmd, onChunk)
	o.record(ctx, Record{
		Op: "stream", Program: cmd.Program, ExitCode: res.ExitCode,
		Bytes:    int64(len(res.Stdout) + len(res.Stderr)),
		Duration: res.Duration.Milliseconds(),
	}, err)
	return res, err
}

func (o *observer) Env() seamshell.Env { return o.inner.Env() }

type config struct {
	SkipOps []string `json:"skipOps,omitempty"`
}

// Plugin 返回 shell-observe 插件:inject shell seam,注册观察装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "shell-observe: 解析配置失败: %v", err)
		}
		emit := func(_ context.Context, rec Record) {
			payload := map[string]any{
				"op": rec.Op, "program": rec.Program, "exit": rec.ExitCode,
				"bytes": rec.Bytes, "durationMs": rec.Duration, "plugin": rec.Plugin,
			}
			if rec.Err != "" {
				payload["error"] = rec.Err
			}
			ctx.Emit(seamshell.EventObserve, payload)
		}
		return chain.Decorate(func(inner seamshell.Shell) seamshell.Shell {
			return New(inner, emit, conf.SkipOps)
		}, Priority)
	}).WithInject("shell")
}
