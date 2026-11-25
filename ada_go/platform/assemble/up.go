package assemble

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/sched"
)

// CodeUp 是装配执行层的错误码。
const CodeUp = "EASSEMBLEUP"

// readyTimeout 是单个插件从挂载到 active 的上限。
// 装配是启动路径,不能因为一个插件卡住就无限等待。
const readyTimeout = 10 * time.Second

// Lookup 按清单返回可挂载的 kernel 插件。
// go 插件从内存注册表取;ts 插件由 bridge 从 Artifact 构造(M3 后续)。
type Lookup func(m *plugin.Manifest) (kernel.Plugin, error)

// UpReport 是一次装配的结果。
type UpReport struct {
	Order  []string
	Fibers map[string]string // 插件 id → fiber id
}

// UpError 是装配失败的错误:带插件名、原因与回滚情况。
type UpError struct {
	Code         string
	Plugin       string
	Cause        error
	RolledBack   int
	RollbackErrs []error
}

func (e *UpError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: 插件 %s 装配失败: %v", e.Code, e.Plugin, e.Cause)
	if e.RolledBack > 0 {
		fmt.Fprintf(&b, "(已回滚 %d 个插件", e.RolledBack)
		if len(e.RollbackErrs) > 0 {
			fmt.Fprintf(&b, ",回滚错误: %v", errors.Join(e.RollbackErrs...))
		}
		b.WriteString(")")
	}
	return b.String()
}

func (e *UpError) Unwrap() error { return e.Cause }

// Up 按计划顺序挂载插件。
//
// 事务语义:任一插件挂载或就绪失败,已挂载的插件**按逆序卸载**,不留半装状态。
// 为什么逆序:与内核 effect 栈一致——后装的插件可能依赖先装的,先拆后面的。
func Up(app *kernel.App, manifests []*plugin.Manifest, plan *Report, lookup Lookup) (*UpReport, error) {
	if app == nil {
		return nil, &UpError{Code: CodeUp, Plugin: "-", Cause: errors.New("app 不能为 nil")}
	}
	if plan == nil || !plan.OK() {
		if plan == nil {
			return nil, &UpError{Code: CodeUp, Plugin: "-", Cause: errors.New("缺少装配计划")}
		}
		return nil, plan.Err()
	}
	byID := make(map[string]*plugin.Manifest, len(manifests))
	for _, m := range manifests {
		byID[m.ID] = m
	}

	report := &UpReport{Order: append([]string{}, plan.Order...), Fibers: map[string]string{}}
	mounted := make([]*kernel.Fiber, 0, len(plan.Order))

	rollback := func(id string, cause error) (*UpReport, error) {
		var rollbackErrs []error
		for i := len(mounted) - 1; i >= 0; i-- {
			if err := mounted[i].Dispose(context.Background()); err != nil {
				rollbackErrs = append(rollbackErrs, err)
			}
		}
		return report, &UpError{
			Code: CodeUp, Plugin: id, Cause: cause,
			RolledBack: len(mounted), RollbackErrs: rollbackErrs,
		}
	}

	for _, id := range plan.Order {
		fiber, err := mountOne(app, byID[id], lookup, report)
		if err != nil {
			return rollback(id, err)
		}
		mounted = append(mounted, fiber)
	}
	return report, nil
}

// mountOne 挂载一个插件并等待就绪;失败时返回可读错误。
func mountOne(app *kernel.App, m *plugin.Manifest, lookup Lookup, report *UpReport) (*kernel.Fiber, error) {
	if m == nil {
		return nil, fmt.Errorf("计划里的插件在清单集合中不存在")
	}
	if lookup == nil {
		return nil, fmt.Errorf("没有为插件类型 %q 提供 Lookup", m.Kind)
	}
	p, err := lookup(m)
	if err != nil {
		return nil, fmt.Errorf("解析实现失败: %w", err)
	}
	fiber, err := app.Plugin(p, kernel.NewConfig(m.Config))
	if err != nil {
		return nil, fmt.Errorf("挂载失败: %w", err)
	}
	rctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	readyErr := fiber.Ready(rctx)
	cancel()
	if readyErr != nil {
		state := fiber.State()
		_ = fiber.Dispose(context.Background())
		return nil, fmt.Errorf("未就绪(状态 %s): %w", state, readyErr)
	}
	report.Fibers[m.ID] = fiber.ID()
	return fiber, nil
}

// Deferred 是延迟激活插件的句柄:宿主在需要时调用 Ensure 挂载。
type Deferred struct {
	app           *kernel.App
	byID          map[string]*plugin.Manifest
	lookup        Lookup
	mounted       map[string]*kernel.Fiber
	triggers      map[string]bool
	eventTriggers map[string]bool

	// mu 保护 mounted / triggers;并发 Ensure 也由它保证只挂载一次。
	mu sync.Mutex
}

// UpLazy 只挂载 activation=startup 的插件,on-demand 插件留给 Deferred.Ensure。
//
// 注意:on-demand 插件如果提供了别人 inject 的服务,依赖它的插件会保持 pending,
// 直到 Ensure 挂载该插件。装配计划仍按依赖图排序,不区分激活方式。
func UpLazy(app *kernel.App, manifests []*plugin.Manifest, plan *Report, lookup Lookup) (*UpReport, *Deferred, error) {
	if app == nil {
		return nil, nil, &UpError{Code: CodeUp, Plugin: "-", Cause: errors.New("app 不能为 nil")}
	}
	if plan == nil || !plan.OK() {
		if plan == nil {
			return nil, nil, &UpError{Code: CodeUp, Plugin: "-", Cause: errors.New("缺少装配计划")}
		}
		return nil, nil, plan.Err()
	}
	byID := make(map[string]*plugin.Manifest, len(manifests))
	for _, m := range manifests {
		byID[m.ID] = m
	}

	report := &UpReport{Order: append([]string{}, plan.Order...), Fibers: map[string]string{}}
	mounted := make([]*kernel.Fiber, 0, len(plan.Order))
	for _, id := range plan.Order {
		m := byID[id]
		if m != nil && m.Activation == "on-demand" {
			continue
		}
		fiber, err := mountOne(app, m, lookup, report)
		if err != nil {
			for i := len(mounted) - 1; i >= 0; i-- {
				_ = mounted[i].Dispose(context.Background())
			}
			return report, nil, &UpError{Code: CodeUp, Plugin: id, Cause: err, RolledBack: len(mounted)}
		}
		mounted = append(mounted, fiber)
	}

	d := &Deferred{app: app, byID: byID, lookup: lookup, mounted: map[string]*kernel.Fiber{}, triggers: map[string]bool{}}
	return report, d, nil
}

// EnsureForService 按服务名触发 on-demand 插件:扫描清单的 provide,找到就挂载。
//
// 返回 (fiber, found, err);找不到返回 (nil,false,nil)。已挂载时 Ensure 幂等。
// 宿主应在挂载消费者之前调用它(消费者 inject 该服务时,内核本身不会自动触发)。
func (d *Deferred) EnsureForService(ctx context.Context, service string) (*kernel.Fiber, bool, error) {
	if service == "" {
		return nil, false, fmt.Errorf("assemble: 服务名不能为空")
	}
	ids := make([]string, 0, len(d.byID))
	for id, m := range d.byID {
		if m == nil || m.Activation != "on-demand" {
			continue
		}
		for _, p := range m.Provide {
			if p == service {
				ids = append(ids, id)
				break
			}
		}
	}
	if len(ids) == 0 {
		return nil, false, nil
	}
	sort.Strings(ids)
	f, err := d.Ensure(ctx, ids[0])
	if err != nil {
		return nil, true, err
	}
	return f, true, nil
}

// EnsureAll 依次挂载全部 on-demand 插件(按 id 排序),返回成功挂载的 id。
//
// 失败即停止并返回已挂载列表,便于宿主诊断「预热到哪一步」。
func (d *Deferred) EnsureAll(ctx context.Context) ([]string, error) {
	ids := d.IDs()
	mounted := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := d.Ensure(ctx, id); err != nil {
			return mounted, err
		}
		mounted = append(mounted, id)
	}
	return mounted, nil
}

// TriggerOnCron 注册定时任务:到点挂载指定的 on-demand 插件。
//
// 返回的 Dispose 用于停止任务;cron/policy 的语义由 sched 负责。任务 id 固定为
// "assemble-ensure-<插件 id>",重复注册同一插件会由 sched 报冲突。
func (d *Deferred) TriggerOnCron(s *sched.Scheduler, store sched.JobStore, id, cron string, policy sched.JobPolicy) (kernel.Dispose, error) {
	if s == nil {
		return nil, fmt.Errorf("assemble: scheduler 不能为 nil")
	}
	if store == nil {
		return nil, fmt.Errorf("assemble: job store 不能为 nil")
	}
	m, ok := d.byID[id]
	if !ok {
		return nil, fmt.Errorf("assemble: 未知的延迟插件 %q", id)
	}
	if m.Activation != "on-demand" {
		return nil, fmt.Errorf("assemble: 插件 %q 不是 on-demand(activation=%q)", id, m.Activation)
	}
	d.mu.Lock()
	if d.triggers == nil {
		d.triggers = map[string]bool{}
	}
	if d.triggers[id] {
		d.mu.Unlock()
		return nil, fmt.Errorf("assemble: 插件 %q 已注册过定时触发", id)
	}
	d.triggers[id] = true
	d.mu.Unlock()

	dispose, err := s.Job(sched.JobSpec{
		ID:     "assemble-ensure-" + id,
		Cron:   cron,
		Policy: policy,
		Handler: func(ctx context.Context) error {
			_, err := d.Ensure(ctx, id)
			return err
		},
	}, store)
	if err != nil {
		d.mu.Lock()
		delete(d.triggers, id)
		d.mu.Unlock()
		return nil, err
	}
	return func(ctx context.Context) error {
		d.mu.Lock()
		delete(d.triggers, id)
		d.mu.Unlock()
		return dispose(ctx)
	}, nil
}

// TriggerOnEvent 订阅一个根事件,事件到达时 Ensure 指定的 on-demand 插件。
//
// 用于 onPage/onCommand 等宿主事件;返回 Dispose 取消订阅。事件处理器里的错误
// 会返回给事件总线(不会回滚已挂载插件)。
func (d *Deferred) TriggerOnEvent(event, id string) (kernel.Dispose, error) {
	if event == "" {
		return nil, fmt.Errorf("assemble: 事件名不能为空")
	}
	m, ok := d.byID[id]
	if !ok {
		return nil, fmt.Errorf("assemble: 未知的延迟插件 %q", id)
	}
	if m.Activation != "on-demand" {
		return nil, fmt.Errorf("assemble: 插件 %q 不是 on-demand(activation=%q)", id, m.Activation)
	}
	key := event + "|" + id
	d.mu.Lock()
	if d.eventTriggers == nil {
		d.eventTriggers = map[string]bool{}
	}
	if d.eventTriggers[key] {
		d.mu.Unlock()
		return nil, fmt.Errorf("assemble: 事件 %q 已订阅过插件 %q", event, id)
	}
	d.eventTriggers[key] = true
	d.mu.Unlock()

	dispose := d.app.Root().On(event, func(ctx context.Context, _ *kernel.Context, _ []any) (any, error) {
		if _, err := d.Ensure(ctx, id); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return func(ctx context.Context) error {
		d.mu.Lock()
		delete(d.eventTriggers, key)
		d.mu.Unlock()
		return dispose(ctx)
	}, nil
}

// IDs 返回可延迟激活的插件 id(按计划顺序)。
func (d *Deferred) IDs() []string {
	out := make([]string, 0, len(d.byID))
	// 保持插入顺序意义不大,调用方主要用它诊断;按 id 排序更稳定。
	ids := make([]string, 0, len(d.byID))
	for id, m := range d.byID {
		if m != nil && m.Activation == "on-demand" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out = append(out, ids...)
	return out
}

// Ensure 挂载一个 on-demand 插件并等待就绪;重复调用返回同一个 fiber。
//
// 并发安全:整段挂载在互斥锁内,32 个 goroutine 同时 Ensure 也只会挂载一次。
func (d *Deferred) Ensure(ctx context.Context, id string) (*kernel.Fiber, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.mounted[id]; ok && f != nil && f.State() != kernel.Disposed {
		return f, nil
	}
	m, ok := d.byID[id]
	if !ok {
		return nil, fmt.Errorf("assemble: 未知的延迟插件 %q", id)
	}
	if m.Activation != "on-demand" {
		return nil, fmt.Errorf("assemble: 插件 %q 不是 on-demand(activation=%q)", id, m.Activation)
	}
	if d.lookup == nil {
		return nil, fmt.Errorf("assemble: 没有为插件类型 %q 提供 Lookup", m.Kind)
	}
	p, err := d.lookup(m)
	if err != nil {
		return nil, fmt.Errorf("assemble: 解析 %q 实现失败: %w", id, err)
	}
	f, err := d.app.Plugin(p, kernel.NewConfig(m.Config))
	if err != nil {
		return nil, fmt.Errorf("assemble: 挂载 %q 失败: %w", id, err)
	}
	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	readyErr := f.Ready(rctx)
	cancel()
	if readyErr != nil {
		_ = f.Dispose(context.Background())
		return nil, fmt.Errorf("assemble: %q 未就绪(状态 %s): %w", id, f.State(), readyErr)
	}
	d.mounted[id] = f
	return f, nil
}
