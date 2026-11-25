// Package shellredact 是 shell 的脱敏策略:执行结果与流式块按正则替换敏感信息。
//
// priority=150:在 shell-observe(250)之内、sandbox(100)之外。
// 模式表与编译逻辑在 platform/redact(与 fs-redact 共用),避免两份模式漂移。
// 注意:流式输出按块脱敏,若密钥被拆到两个块里仍可能漏(见 docs/seam-shell.md)。
package shellredact

import (
	"context"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/redact"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// PolicyName 是插件 id。
const PolicyName = "shell-redact"

// Priority 是装饰器优先级。
const Priority = 150

// DefaultReplacement 是默认替换文本(与 platform/redact 同源)。
const DefaultReplacement = redact.DefaultReplacement

// New 构造脱敏装饰器;patterns 为空时用默认模式。
func New(inner seamshell.Shell, patterns []string, replacement string) (seamshell.Shell, error) {
	rd, err := redact.New(patterns, replacement)
	if err != nil {
		return nil, fmt.Errorf("shell-redact: %w", err)
	}
	return &redactor{inner: inner, rd: rd}, nil
}

type redactor struct {
	inner seamshell.Shell
	rd    *redact.Redactor
}

func (r *redactor) redactResult(res seamshell.Result) seamshell.Result {
	res.Stdout = r.rd.ApplyBytes(res.Stdout)
	res.Stderr = r.rd.ApplyBytes(res.Stderr)
	return res
}

func (r *redactor) Run(ctx context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	res, err := r.inner.Run(ctx, cmd)
	return r.redactResult(res), err
}

func (r *redactor) Stream(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	wrapped := func(chunk seamshell.Chunk) error {
		chunk.Text = r.rd.Apply(chunk.Text)
		return onChunk(chunk)
	}
	res, err := r.inner.Stream(ctx, cmd, wrapped)
	return r.redactResult(res), err
}

func (r *redactor) Env() seamshell.Env { return r.inner.Env() }

type config struct {
	Patterns    []string `json:"patterns,omitempty"`
	Replacement string   `json:"replacement,omitempty"`
}

// Plugin 返回 shell-redact 插件:inject shell seam,注册脱敏装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "shell-redact: 解析配置失败: %v", err)
		}
		rd, err := redact.New(conf.Patterns, conf.Replacement)
		if err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "shell-redact: %v", err)
		}
		return chain.Decorate(func(inner seamshell.Shell) seamshell.Shell {
			return &redactor{inner: inner, rd: rd}
		}, Priority)
	}).WithInject("shell")
}
