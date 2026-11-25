package kernel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// State 是 fiber 的生命周期状态。
type State int

const (
	Pending   State = iota // 依赖未满足,等待中
	Loading                // 正在 Apply
	Active                 // 已装配
	Failed                 // Apply 失败
	Unloading              // 正在回收
	Disposed               // 已终结
)

func (s State) String() string {
	switch s {
	case Pending:
		return "pending"
	case Loading:
		return "loading"
	case Active:
		return "active"
	case Failed:
		return "failed"
	case Unloading:
		return "unloading"
	case Disposed:
		return "disposed"
	}
	return "unknown"
}

// Fiber 是一个插件实例:它拥有一个作用域、一份配置快照、一个状态机。
type Fiber struct {
	id     string
	app    *App
	plugin Plugin
	cfg    Config
	ctx    *Context
	parent *Fiber
	scope  *scope

	mu       sync.Mutex
	state    State
	err      error
	ch       chan struct{} // 每次状态变化关闭并重建,用于等待
	epoch    uint64
	disposed bool
	// tracked 是本 fiber 依赖的服务实现,provided 是本 fiber 注册的服务实现。
	// 两者必须分清:卸载时通知的是「依赖我提供的服务」的 fiber(provided),
	// 而不是「与我依赖同一服务」的兄弟(tracked)。
	tracked     []*svcImpl
	provided    []*svcImpl
	parentEntry *entry // 父作用域里代表自己的条目(主动 Dispose 时摘除)

	// lastActive 是最近一次活动(挂载/服务解析)的 UnixNano;由 atomic 读写,
	// 因为服务解析可能来自多个并发调用方。
	lastActive atomic.Int64
}

// ID 返回实例的唯一标识(形如 "plugin#3")。
func (f *Fiber) ID() string { return f.id }

// Touch 标记一次活动。内核在服务解析时自动调用;宿主也可以手动标记(例如
// 收到外部请求时),用于闲置回收的活性判断。
func (f *Fiber) Touch() { f.lastActive.Store(time.Now().UnixNano()) }

// LastActive 返回最近一次活动时间;零值 fiber(未挂载)返回零时间。
func (f *Fiber) LastActive() time.Time {
	ns := f.lastActive.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Plugin 返回插件定义。
func (f *Fiber) Plugin() Plugin { return f.plugin }

// State 返回当前状态。
func (f *Fiber) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Epoch 返回依赖指纹;它变化意味着需要重载。
func (f *Fiber) Epoch() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch
}

func (f *Fiber) setState(s State, err error) {
	f.mu.Lock()
	prev := f.state
	f.state = s
	f.err = err
	ch := f.ch
	f.ch = make(chan struct{})
	f.mu.Unlock()
	close(ch)
	if prev != s {
		f.app.log.Debug("fiber 状态变化", "fiber", f.id, "from", prev.String(), "to", s.String())
	}
}

// rememberProvided 登记一个由本 fiber 注册的服务实现。
func (f *Fiber) rememberProvided(im *svcImpl) {
	if im == nil {
		return
	}
	f.mu.Lock()
	f.provided = append(f.provided, im)
	f.mu.Unlock()
}

// forgetProvided 在服务提前注销(插件主动调用返回的 Dispose)时同步记账,
// 避免 teardown 为已经不存在的服务去通知依赖方。
func (f *Fiber) forgetProvided(im *svcImpl) {
	f.mu.Lock()
	for i, x := range f.provided {
		if x == im {
			f.provided = append(f.provided[:i], f.provided[i+1:]...)
			break
		}
	}
	f.mu.Unlock()
}

// Ready 等待进入 active;failed 返回装配错误;依赖始终未满足时,
// 到 ctx 截止返回 CodePending。
func (f *Fiber) Ready(ctx context.Context) error {
	for {
		f.mu.Lock()
		st, err, ch := f.state, f.err, f.ch
		f.mu.Unlock()
		switch st {
		case Active:
			return nil
		case Failed:
			return err
		case Disposed:
			return Fail(CodeFiberDisposed, "插件 %s 已卸载", f.plugin.Name())
		}
		select {
		case <-ch:
		case <-ctx.Done():
			if st == Pending {
				// 返回存储的 pending 原因(含缺失的服务名),而不是一句泛泛的
				// 「仍在等待依赖」——诊断时最需要知道的就是「在等谁」。
				if err != nil {
					return err
				}
				return Fail(CodePending, "插件 %s 仍在等待依赖", f.plugin.Name())
			}
			return ctx.Err()
		}
	}
}

// Reload 强制重载:先卸载再按当前依赖重新装配。
func (f *Fiber) Reload(ctx context.Context) error {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return Fail(CodeFiberDisposed, "插件 %s 已卸载", f.plugin.Name())
	}
	f.epoch = 0
	f.mu.Unlock()
	f.reconcile(ctx)
	if err := f.Ready(ctx); err != nil && !IsCode(err, CodePending) {
		return err
	}
	return nil
}

// Update 应用新配置:先用 schema 校验,再整体重载。
// 校验失败时旧配置继续生效——半生效的配置比没有配置更难排查。
func (f *Fiber) Update(cfg Config) error {
	raw := cfg.Raw()
	if s := f.plugin.Config(); s != nil {
		applied, err := s.Apply(raw)
		if err != nil {
			return Wrap(CodeConfig, err, "插件 %s 的配置无效", f.plugin.Name())
		}
		m, ok := applied.(map[string]any)
		if !ok {
			return Fail(CodeConfig, "插件 %s 的配置必须是对象", f.plugin.Name())
		}
		raw = m
	}
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return Fail(CodeFiberDisposed, "插件 %s 已卸载", f.plugin.Name())
	}
	f.cfg = NewConfig(raw)
	f.epoch = 0
	f.mu.Unlock()
	f.reconcile(context.Background())
	return nil
}

// Dispose 卸载插件:先让依赖它提供的服务的 fiber 停机,再逆序回收本 fiber 的副作用。
//
// 返回值是副作用回收的聚合错误:单个 disposer 失败不会中断其余回收,但必须上报。
// 「卸载悄悄失败」会让插件作者以为资源已经释放,排查时没有任何线索。
func (f *Fiber) Dispose(ctx context.Context) error {
	f.mu.Lock()
	if f.disposed {
		f.mu.Unlock()
		return nil
	}
	f.disposed = true
	f.mu.Unlock()

	err := f.teardown(ctx)
	f.setState(Disposed, nil)
	// 主动卸载时要把「挂在父作用域上的那一条」也摘掉;否则根作用域会随
	// 反复挂载/卸载无限增长(父作用域关闭时这条已执行,调用无副作用)。
	if f.parentEntry != nil {
		f.parentEntry.detach()
	}
	// 从注册表移除:不删的话每次挂载/卸载都会留下一个僵尸 fiber,
	// Inspect 与每轮 recompute 都会一直遍历它们。
	f.app.forgetFiber(f)
	if err != nil {
		f.app.log.Warn("插件卸载时副作用回收失败", "plugin", f.plugin.Name(), "fiber", f.id, "err", err)
	}
	f.app.recompute()
	return err
}

// reconcile 是唯一的决策点:依赖是否满足、epoch 是否变化、要不要重载。
func (f *Fiber) reconcile(ctx context.Context) {
	f.mu.Lock()
	// Loading 必须直接返回:Apply 内部会 Provide/挂子插件,那会触发重入的
	// recompute;若允许在此重载,同一个 fiber 会在 Apply 未返回时被再次 Apply。
	if f.disposed || f.state == Loading || f.state == Unloading || f.state == Disposed {
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()

	deps, epoch, missing := f.app.resolveDeps(f.plugin.Inject(), f.ctx)
	if missing != "" {
		if f.loaded() {
			_ = f.teardown(ctx)
		}
		f.setState(Pending, Fail(CodePending, "等待服务 %q", missing))
		return
	}

	f.mu.Lock()
	// 失败的实例在同一依赖指纹下不自动重试:否则每次 recompute 都会重跑 Apply,
	// 一个必然失败的插件会把整个内核拖进死循环。重试由 Reload/Update/依赖变化触发。
	same := (f.state == Active || f.state == Failed) && f.epoch == epoch
	f.mu.Unlock()
	if same {
		return
	}
	f.reload(ctx, epoch, deps)
}

func (f *Fiber) loaded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state == Active || f.state == Loading
}

// reload 先回收旧实例,再用新依赖重新 Apply。
func (f *Fiber) reload(ctx context.Context, epoch uint64, deps []*svcImpl) {
	if err := f.teardown(ctx); err != nil {
		f.app.log.Warn("重载前回收出错", "plugin", f.plugin.Name(), "fiber", f.id, "err", err)
	}

	f.mu.Lock()
	f.epoch = epoch
	f.mu.Unlock()
	f.setState(Loading, nil)

	d, err := safeApply(f, f.ctx, f.cfg)
	if err != nil {
		// 失败也必须走 teardown:它关闭已注册的副作用并把 scope 换新。
		// 否则 scope 停在「已关闭」状态,后续 Reload/Update 一注册就报
		// EEFFECTDISPOSED,失败的插件再也回不来。
		f.failAfter(ctx, err)
		return
	}
	if d != nil {
		if _, aerr := f.scope.add("apply", d, f.app.opts.EffectTimeout); aerr != nil {
			f.failAfter(ctx, aerr)
			return
		}
	}

	// 依赖跟踪必须在激活之后登记:失败实例不应让提供者以为有人依赖它。
	f.mu.Lock()
	f.tracked = deps
	f.mu.Unlock()
	for _, im := range deps {
		f.app.services.depend(im, f)
	}

	f.setState(Active, nil)
	f.app.recompute()
}

// failAfter 把装配失败收敛成「回收 + failed + 重算」三步,避免两处分支漂移。
func (f *Fiber) failAfter(ctx context.Context, cause error) {
	if err := f.teardown(ctx); err != nil {
		f.app.log.Warn("装配失败后回收出错", "plugin", f.plugin.Name(), "fiber", f.id, "err", err)
	}
	f.setState(Failed, cause)
	f.app.recompute()
}

// teardown 回收本 fiber 的作用域并解除依赖跟踪。可重复调用。
//
// 顺序是语义的一部分:
//  1. 先通知「依赖我提供的服务」的 fiber,让它们在本 fiber 的服务消失之前停机;
//  2. 再解除我自己对上游服务的依赖;
//  3. 最后逆序回收副作用(服务的注销发生在这一步)。
//
// 第 1 步若写成「通知与我依赖同一服务的兄弟」,一次无关插件的卸载就会
// 把兄弟插件全部拆掉并重跑 Apply(实测 apply 次数 1→2,并继续级联)。
func (f *Fiber) teardown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	f.mu.Lock()
	wasLoaded := f.state == Active || f.state == Loading
	tracked := f.tracked
	provided := f.provided
	f.tracked = nil
	f.provided = nil
	f.mu.Unlock()
	if !wasLoaded && len(tracked) == 0 && len(provided) == 0 {
		return nil
	}

	f.setState(Unloading, nil)

	for _, im := range provided {
		for _, dep := range f.app.services.dependentsOf(im) {
			if dep.id == f.id {
				continue
			}
			dep.markDependencyLost(ctx)
		}
	}
	for _, im := range tracked {
		f.app.services.undepend(im, f)
	}

	err := f.scope.close(ctx)
	// 作用域关闭后必须换新的:重载要往同一个 fiber 上重新注册副作用,
	// 继续复用已关闭的作用域会让第二次 Apply 直接失败。
	f.mu.Lock()
	f.scope = newScope()
	f.mu.Unlock()
	f.setState(Pending, nil)
	return err
}

// markDependencyLost 由提供者调用:本 fiber 依赖的服务没了,先停机。
func (f *Fiber) markDependencyLost(ctx context.Context) {
	f.mu.Lock()
	loaded := f.state == Active || f.state == Loading
	f.mu.Unlock()
	if loaded {
		if err := f.teardown(ctx); err != nil {
			f.app.log.Warn("依赖服务下线时回收出错", "plugin", f.plugin.Name(), "fiber", f.id, "err", err)
		}
		f.setState(Pending, Fail(CodePending, "依赖的服务已下线"))
	}
}

// effectLabels 返回当前作用域上的副作用标签(诊断用)。
func (f *Fiber) effectLabels() []string {
	f.mu.Lock()
	sc := f.scope
	f.mu.Unlock()
	if sc == nil {
		return nil
	}
	return sc.labels()
}

// safeApply 把插件回调的 panic 转成错误,绝不让它穿过内核。
func safeApply(f *Fiber, ctx *Context, cfg Config) (d Dispose, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = Fail(CodePanic, "插件 %s 装配时 panic: %v", f.plugin.Name(), rec)
		}
	}()
	return f.plugin.Apply(ctx, cfg)
}
