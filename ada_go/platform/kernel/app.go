package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Options 是应用级配置。
type Options struct {
	Logger        *slog.Logger  // 缺省丢弃日志；宿主应注入自己的 logger
	EffectTimeout time.Duration // 单个副作用回收上限，缺省 5s
	EventTimeout  time.Duration // 单个监听器执行上限，缺省 200ms
}

// App 是一个内核实例：根作用域、服务表、事件总线、fiber 注册表。
//
// 并发模型（M1）：生命周期操作（挂载/卸载/重载/分发）由单一控制协程调用；
// 服务表与 fiber 表本身加锁，保证读取方（Inspect、测试）安全。
type App struct {
	opts Options
	log  *slog.Logger

	root  *Context
	scope *scope

	services *serviceTable
	events   *eventHub

	mu      sync.RWMutex
	fibers  map[string]*Fiber
	stopped bool
	seq     atomic.Uint64

	labelMu sync.Mutex
	labels  map[string]*label

	// stateMu 只保护「重算状态」两个字段，绝不在它持有时调用插件回调：
	// Apply 里的 Provide/Plugin 会重入 recompute，持有长锁会直接死锁。
	stateMu     sync.Mutex
	inRecompute bool
	dirty       bool
}

// New 创建应用。它不启动任何协程，也不读磁盘。
func New(opts Options) *App {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.EffectTimeout <= 0 {
		opts.EffectTimeout = 5 * time.Second
	}
	if opts.EventTimeout <= 0 {
		opts.EventTimeout = 200 * time.Millisecond
	}
	a := &App{
		opts:     opts,
		log:      opts.Logger,
		scope:    newScope(),
		services: newServiceTable(),
		events:   newEventHub(opts.EventTimeout),
		fibers:   make(map[string]*Fiber),
		labels:   make(map[string]*label),
	}
	a.root = newRootContext(a)
	return a
}

// Root 返回根上下文；宿主在这里挂载顶层插件。
func (a *App) Root() *Context { return a.root }

// Plugin 在根作用域挂载一个插件。
func (a *App) Plugin(p Plugin, cfg Config) (*Fiber, error) { return a.mount(a.root, p, cfg) }

func (a *App) labelFor(name string) *label {
	a.labelMu.Lock()
	defer a.labelMu.Unlock()
	if l, ok := a.labels[name]; ok {
		return l
	}
	l := &label{name: name}
	a.labels[name] = l
	return l
}

func (a *App) scopeFor(c *Context) *scope {
	for cur := c; cur != nil; cur = cur.parent {
		if cur.fiber != nil {
			return cur.fiber.scope
		}
	}
	return a.scope
}

// mount 是唯一创建 fiber 的地方。它先把配置补全校验，再建实例，
// 最后交给 reconcile 决定「立即装配还是等待依赖」。
func (a *App) mount(parent *Context, p Plugin, cfg Config) (*Fiber, error) {
	if p == nil {
		return nil, Fail(CodeInvalid, "插件不能为 nil")
	}
	if parent == nil {
		parent = a.root
	}
	raw := cfg.Raw()
	if s := p.Config(); s != nil {
		applied, err := s.Apply(raw)
		if err != nil {
			return nil, Wrap(CodeConfig, err, "插件 %s 的配置无效", p.Name())
		}
		m, ok := applied.(map[string]any)
		if !ok {
			return nil, Fail(CodeConfig, "插件 %s 的配置必须是对象", p.Name())
		}
		raw = m
	}

	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil, Fail(CodeInvalid, "应用已停止，无法挂载插件 %s", p.Name())
	}
	id := fmt.Sprintf("%s#%d", p.Name(), a.seq.Add(1))
	f := &Fiber{
		id:     id,
		app:    a,
		plugin: p,
		cfg:    NewConfig(raw),
		scope:  newScope(),
		state:  Pending,
		ch:     make(chan struct{}),
		parent: parent.fiber,
	}
	f.lastActive.Store(time.Now().UnixNano())
	f.ctx = parent.derive(func(v *vars) { v.entry = EntryMeta{ID: id, Source: "memory"} })
	f.ctx.fiber = f
	a.fibers[id] = f
	a.mu.Unlock()

	// 子 fiber 的生命周期挂在父作用域上：父卸载自动级联。
	e, err := a.scopeFor(parent).add("plugin:"+id, func(ctx context.Context) error {
		return f.Dispose(ctx)
	}, a.opts.EffectTimeout)
	if err != nil {
		a.mu.Lock()
		delete(a.fibers, id)
		a.mu.Unlock()
		return nil, err
	}
	f.parentEntry = e

	f.reconcile(context.Background())
	a.recompute()
	return f, nil
}

// resolveDeps 计算依赖指纹并返回已解析的实现。
// 返回的 missing 非空表示必需依赖未满足；此时 fiber 必须停在 pending。
func (a *App) resolveDeps(inj Inject, c *Context) (deps []*svcImpl, epoch uint64, missing string) {
	epoch = 1469598103934665603
	for _, name := range inj.Names {
		im := a.services.resolve(name, c.isolateFor(name))
		if im == nil || im.spec.Version < inj.minVersion(name) {
			return nil, 0, name
		}
		deps = append(deps, im)
		epoch = mixUID(epoch, im.uid)
	}
	for _, name := range inj.Optional {
		im := a.services.resolve(name, c.isolateFor(name))
		if im == nil || im.spec.Version < inj.minVersion(name) {
			continue
		}
		deps = append(deps, im)
		epoch = mixUID(epoch, im.uid)
	}
	return deps, epoch, ""
}

func mixUID(h, uid uint64) uint64 {
	return (h ^ uid) * 1099511628211
}

// recompute 重新评估所有 fiber。它会被重入调用（Apply 里挂子插件、Provide 触发），
// 用 dirty 标记把重入折叠成外层循环，避免无限递归。
//
// 注意：循环体不持有任何锁——折叠靠 inRecompute/dirty 两个字段，
// 它们由 stateMu 保护，但每次只做短暂读写。
func (a *App) recompute() {
	a.stateMu.Lock()
	if a.inRecompute {
		a.dirty = true
		a.stateMu.Unlock()
		return
	}
	a.inRecompute = true
	a.stateMu.Unlock()

	for {
		a.stateMu.Lock()
		a.dirty = false
		a.stateMu.Unlock()

		for _, f := range a.snapshotFibers() {
			f.reconcile(context.Background())
		}

		a.stateMu.Lock()
		done := !a.dirty
		a.stateMu.Unlock()
		if done {
			break
		}
	}

	a.stateMu.Lock()
	a.inRecompute = false
	a.stateMu.Unlock()
}

// forgetFiber 从注册表移除一个已卸载的 fiber。
// 只删自己那一条:同一个 id 不可能被复用,防止误删后续实例。
func (a *App) forgetFiber(f *Fiber) {
	a.mu.Lock()
	if cur, ok := a.fibers[f.id]; ok && cur == f {
		delete(a.fibers, f.id)
	}
	a.mu.Unlock()
}

func (a *App) snapshotFibers() []*Fiber {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Fiber, 0, len(a.fibers))
	for _, f := range a.fibers {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// Fiber 按 id 查找实例。
func (a *App) Fiber(id string) (*Fiber, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	f, ok := a.fibers[id]
	return f, ok
}

// FiberInfo 是诊断快照里的一项。
type FiberInfo struct {
	ID     string
	Plugin string
	State  string
	Epoch  uint64
	Parent string // 父 fiber 的 ID;根作用域上的插件为空
}

// ServiceInfo 是服务表快照里的一项。
type ServiceInfo struct {
	Name       string
	Version    int
	Kind       string
	Provider   string
	UID        uint64
	Dependents []string
}

// Snapshot 是 Inspect 的返回值，用于测试与 ada inspect。
type Snapshot struct {
	Fibers   []FiberInfo
	Services []ServiceInfo
	Effects  []EffectInfo
	Events   []string
}

// EffectInfo 是诊断快照里的一个副作用登记项。
//
// 只给标签与归属,不给 disposer:Inspect 是只读视图,不能顺手回收别人的资源。
type EffectInfo struct {
	Fiber  string
	Plugin string
	Label  string
}

// ReclaimIdle 回收「无活动超过 ttl 且没有依赖者」的 fiber,返回被回收的 id。
//
// now 由调用方显式传入:回收策略需要可测试,不能依赖真实时钟。
// 有 dependents 的提供者本轮跳过(其依赖者若也被回收,下一次调用再把提供者回收),
// 因此回收是「分轮收敛」的,不会出现一次调用里把依赖链拆散的情况。
func (a *App) ReclaimIdle(ctx context.Context, ttl time.Duration, now time.Time) ([]string, error) {
	if ttl <= 0 {
		return nil, Fail(CodeInvalid, "kernel: ReclaimIdle 的 ttl 必须为正,得到 %s", ttl)
	}
	snap := a.Inspect()
	depended := map[string]bool{}
	for _, s := range snap.Services {
		if s.Provider != "" && len(s.Dependents) > 0 {
			depended[s.Provider] = true
		}
	}

	var reclaimed []string
	var errs []error
	for _, f := range a.snapshotFibers() {
		if f.State() != Active || depended[f.ID()] {
			continue
		}
		if now.Sub(f.LastActive()) < ttl {
			continue
		}
		if err := f.Dispose(ctx); err != nil {
			errs = append(errs, err)
			continue
		}
		reclaimed = append(reclaimed, f.ID())
	}
	return reclaimed, errors.Join(errs...)
}

// Inspect 返回当前运行态快照。
func (a *App) Inspect() Snapshot {
	a.mu.RLock()
	svc := a.services
	events := a.events
	a.mu.RUnlock()

	snap := Snapshot{}
	for _, f := range a.snapshotFibers() {
		f.mu.Lock()
		st, epoch := f.state, f.epoch
		f.mu.Unlock()
		parent := ""
		if f.parent != nil {
			parent = f.parent.id
		}
		snap.Fibers = append(snap.Fibers, FiberInfo{ID: f.id, Plugin: f.plugin.Name(), State: st.String(), Epoch: epoch, Parent: parent})
		for _, label := range f.effectLabels() {
			snap.Effects = append(snap.Effects, EffectInfo{Fiber: f.id, Plugin: f.plugin.Name(), Label: label})
		}
	}
	snap.Services = svc.snapshot()
	sort.Slice(snap.Services, func(i, j int) bool { return snap.Services[i].Name < snap.Services[j].Name })
	snap.Events = events.names()
	return snap
}

// Stop 停止应用：逆序回收根作用域上的全部插件。
func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	a.mu.Unlock()

	err := a.scope.close(ctx)
	for _, f := range a.snapshotFibers() {
		f.mu.Lock()
		f.disposed = true
		if f.state != Disposed {
			f.state = Disposed
			f.err = nil
			close(f.ch)
			f.ch = make(chan struct{})
		}
		f.mu.Unlock()
	}
	// 停机后不留残留:注册表清空,事件总线封口。
	// 否则 Stop 之后仍能派发事件、Inspect 仍会列出已死的 fiber。
	a.mu.Lock()
	a.fibers = make(map[string]*Fiber)
	a.mu.Unlock()
	a.events.seal()
	return err
}

// ListenerCount 返回当前监听器数量，供重载测试判断是否泄漏。
func (a *App) ListenerCount() int { return a.events.count() }

// EffectCount 返回根作用域上的副作用数量。
func (a *App) EffectCount() int { return a.scope.size() }
