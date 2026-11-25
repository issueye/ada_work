// Package retry 是 `policy/llm-retry`:对可重试的 provider 错误做指数退避重试。
//
// 它只包装 llm provider,不修改请求;重试次数默认 2(共 3 次调用),
// 流式调用一旦已经向调用方发出增量就不再重试(否则会重复输出)。
package retry

import (
	"context"
	"errors"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// PolicyName 是插件 id。
const PolicyName = "llm-retry"

// Priority 是装饰器优先级。
const Priority = 100

type config struct {
	MaxRetries  int      `json:"maxRetries,omitempty"`
	BaseDelayMs int      `json:"baseDelayMs,omitempty"`
	MaxDelayMs  int      `json:"maxDelayMs,omitempty"`
	RetryOn     []string `json:"retryOn,omitempty"`
}

type retrier struct {
	inner    llm.Provider
	attempts int
	base     time.Duration
	max      time.Duration
	codes    map[string]bool
}

// New 构造重试包装;attempts 是重试次数(不含首次)。
func New(inner llm.Provider, attempts int, base, max time.Duration, codes []string) llm.Provider {
	if attempts < 0 {
		attempts = 0
	}
	if base <= 0 {
		base = 200 * time.Millisecond
	}
	if max <= 0 {
		max = 2 * time.Second
	}
	set := map[string]bool{}
	for _, c := range codes {
		set[c] = true
	}
	return &retrier{inner: inner, attempts: attempts, base: base, max: max, codes: set}
}

func (r *retrier) Name() string { return r.inner.Name() }
func (r *retrier) Version() int { return r.inner.Version() }
func (r *retrier) Models(ctx context.Context) ([]llm.ModelInfo, error) {
	return r.inner.Models(ctx)
}

func (r *retrier) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	var last error
	for i := 0; i <= r.attempts; i++ {
		resp, err := r.inner.Complete(ctx, req)
		if err == nil {
			return resp, nil
		}
		last = err
		if i == r.attempts || !r.retryable(err) {
			return resp, err
		}
		if err := r.wait(ctx, i); err != nil {
			return llm.Response{}, err
		}
	}
	return llm.Response{}, last
}

func (r *retrier) Stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	var last error
	for i := 0; i <= r.attempts; i++ {
		emitted := false
		resp, err := r.inner.Stream(ctx, req, func(d llm.Delta) error {
			emitted = true
			return onDelta(d)
		})
		if err == nil {
			return resp, nil
		}
		last = err
		// 已经产生增量后重试会造成重复输出;只在「没发出任何增量」时重试。
		if emitted || i == r.attempts || !r.retryable(err) {
			return resp, err
		}
		if err := r.wait(ctx, i); err != nil {
			return llm.Response{}, err
		}
	}
	return llm.Response{}, last
}

func (r *retrier) wait(ctx context.Context, attempt int) error {
	d := r.base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d > r.max {
			d = r.max
			break
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return seamerr.Wrap(seamerr.ErrCanceled, PolicyName, "重试等待被取消", ctx.Err())
	case <-t.C:
		return nil
	}
}

func (r *retrier) retryable(err error) bool {
	var se *seamerr.Error
	if errors.As(err, &se) {
		if len(r.codes) > 0 && r.codes[string(se.Code)] {
			return true
		}
		if len(r.codes) == 0 {
			return se.Retryable
		}
		return false
	}
	return false
}

// Plugin 返回 llm-retry 插件:inject llm,注册 provider 装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*llm.Registry](ctx, "llm")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "llm-retry: 解析配置失败: %v", err)
		}
		attempts := conf.MaxRetries
		if attempts < 0 {
			attempts = 0
		}
		return registry.Decorate(func(p llm.Provider) llm.Provider {
			return New(p, attempts,
				time.Duration(conf.BaseDelayMs)*time.Millisecond,
				maxDelay(conf.MaxDelayMs), conf.RetryOn)
		}, Priority)
	}).WithInject("llm")
}

func maxDelay(ms int) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}
