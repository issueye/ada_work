// Package approval 是 `seam/approval` 的契约层:升级请求、审批决定与审批人注册表。
//
// 契约来源:docs/seam-api.md §5(审批) 与 plugin-kernel-design §12.2(门禁)。
// 默认必须 fail-closed:没有审批人 / 审批人报错 / 超时都应拒绝。
package approval

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// DefaultTimeout 是单次审批的默认等待时间。
const DefaultTimeout = 30 * time.Second

// EventRequested 是审批请求事件:在调用审批人之前广播,供 UI/审计渲染。
const EventRequested = "approval/requested"

// Request 是一次升级审批请求。
type Request struct {
	ID      string
	Source  string // "fs" | "shell" | "policy"
	Op      string
	Target  string
	Reason  string
	Details map[string]any
	Timeout time.Duration // 0 = DefaultTimeout
}

// Decision 是审批结果。
type Decision struct {
	Allow    bool
	Reason   string
	Approver string
}

// Approver 是审批人(UI 弹窗、策略、或默认拒绝)。
type Approver interface {
	Name() string
	Approve(ctx context.Context, req Request) (Decision, error)
}

// Registry 是 seam 插件提供的服务值:审批人注册于此。
//
// 选择规则固定:默认取名字排序后的第一个审批人;没有审批人时 Get 返回错误,
// 调用方必须据此拒绝(fail-closed)。
type Registry struct {
	mu        sync.RWMutex
	approvers map[string]Approver
	notifier  func(context.Context, Request)
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{approvers: make(map[string]Approver)}
}

// Register 注册审批人;dispose 时注销。
func (r *Registry) Register(impl Approver) (kernel.Dispose, error) {
	if impl == nil || impl.Name() == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "approval", "审批人不能为空", false)
	}
	name := impl.Name()
	r.mu.Lock()
	if _, exists := r.approvers[name]; exists {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, name, "同名审批人已注册", false)
	}
	r.approvers[name] = impl
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		if cur, ok := r.approvers[name]; ok && cur == impl {
			delete(r.approvers, name)
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// SetNotifier 设置请求通知器(通常由 seam/approval 插件接到 approval/requested 事件)。
func (r *Registry) SetNotifier(fn func(context.Context, Request)) {
	r.mu.Lock()
	r.notifier = fn
	r.mu.Unlock()
}

// notify 在调用审批人前广播请求;通知器 panic 不影响审批(fail-closed 由审批人决定)。
func (r *Registry) notify(ctx context.Context, req Request) {
	r.mu.RLock()
	fn := r.notifier
	r.mu.RUnlock()
	if fn == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		fn(ctx, req)
	}()
}

// Names 返回已注册审批人(排序)。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.approvers))
	for name := range r.approvers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Get 按名取审批人;name 为空时取默认(名字排序第一个)。
func (r *Registry) Get(name string) (Approver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		names := make([]string, 0, len(r.approvers))
		for n := range r.approvers {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, seamerr.Unimplemented("approval", "没有配置审批人")
		}
		name = names[0]
	}
	impl, ok := r.approvers[name]
	if !ok {
		return nil, seamerr.New(seamerr.ErrUnimplemented, name, "审批人未注册", false)
	}
	return impl, nil
}

const pluginID = "seam-approval"

// Plugin 返回 seam/approval 插件:只提供 approval 服务(Registry)。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry := NewRegistry()
		registry.SetNotifier(func(callCtx context.Context, req Request) {
			payload := map[string]any{
				"id": req.ID, "source": req.Source, "op": req.Op,
				"target": RedactTarget(req.Target), "reason": req.Reason,
			}
			if id, ok := kernel.Principal(callCtx); ok {
				payload["plugin"] = id
			}
			ctx.Emit(EventRequested, payload)
		})
		_, err := ctx.Provide("approval", registry, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("approval")
}

// Ask 是一次带超时的审批调用。
//
// 所有失败路径都返回错误或 Allow=false:没有审批人、审批人报错、超时都视为拒绝,
// 调用方不得把错误当成放行。
func Ask(ctx context.Context, registry *Registry, backend string, req Request) (Decision, error) {
	if registry == nil {
		return Decision{Allow: false, Reason: "没有 approval 服务"}, seamerr.New(seamerr.ErrUnimplemented, "approval", "没有 approval 服务", false)
	}
	approver, err := registry.Get(backend)
	if err != nil {
		return Decision{Allow: false, Reason: err.Error()}, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 先广播「有人请求审批」,再等待决定;UI 可以据此弹窗,审计可以据此落账。
	registry.notify(ctx, req)

	dec, err := approver.Approve(actx, req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(actx.Err(), context.DeadlineExceeded) {
			return Decision{Allow: false, Reason: "审批超时", Approver: approver.Name()},
				&seamerr.Error{Code: seamerr.ErrTimeout, Provider: "approval", Message: "审批超时", Err: err}
		}
		return Decision{Allow: false, Reason: err.Error(), Approver: approver.Name()}, err
	}
	if dec.Approver == "" {
		dec.Approver = approver.Name()
	}
	return dec, nil
}

// RedactTarget 去掉 URL target 的 userinfo、fragment 与 query,避免 secret 进入审批提示/审计。
// 非 URL 目标(文件路径/工具名)原样返回。
func RedactTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	return u.String()
}

// Describe 便于日志。
func Describe(req Request) string {
	target := req.Target
	if target == "" {
		target = req.Op
	}
	return fmt.Sprintf("%s:%s(%s)", req.Source, target, req.Reason)
}
