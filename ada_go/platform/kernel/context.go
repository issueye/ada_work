package kernel

import (
	"context"
	"log/slog"
)

// EntryMeta 是 fiber 的来源信息，用于日志与诊断。
type EntryMeta struct {
	ID     string
	Source string
}

// ServiceConfig 是叠加在服务上的「服务专属配置」，
// 由提供者读取，用于多实例或分组的差异化行为。
type ServiceConfig map[string]any

type vars struct {
	isolate   map[string]*label
	intercept map[string]ServiceConfig
	entry     EntryMeta
}

// Context 是插件看到的世界。派生操作返回新值，父链只读。
type Context struct {
	app    *App
	parent *Context
	fiber  *Fiber
	v      vars
}

func newRootContext(a *App) *Context {
	return &Context{app: a}
}

// App 返回所属应用。
func (c *Context) App() *App { return c.app }

// Fiber 返回当前上下文所属的插件实例；根上下文返回 nil。
func (c *Context) Fiber() *Fiber { return c.fiber }

// Entry 返回来源信息。
func (c *Context) Entry() EntryMeta { return c.v.entry }

func (c *Context) derive(mut func(*vars)) *Context {
	nv := vars{
		isolate:   make(map[string]*label, len(c.v.isolate)),
		intercept: make(map[string]ServiceConfig, len(c.v.intercept)),
		entry:     c.v.entry,
	}
	for k, v := range c.v.isolate {
		nv.isolate[k] = v
	}
	for k, v := range c.v.intercept {
		nv.intercept[k] = v
	}
	mut(&nv)
	return &Context{app: c.app, parent: c, fiber: c.fiber, v: nv}
}

// Extend 派生一个带来源信息的新作用域。
func (c *Context) Extend(e EntryMeta) *Context {
	return c.derive(func(v *vars) { v.entry = e })
}

// Isolate 让 name 在子作用域里指向独立的服务槽；同名 label 共享同一槽。
func (c *Context) Isolate(name, label string) *Context {
	return c.derive(func(v *vars) { v.isolate[name] = c.app.labelFor(label) })
}

// Intercept 给下层使用的服务叠一层专属配置。
func (c *Context) Intercept(name string, cfg ServiceConfig) *Context {
	return c.derive(func(v *vars) { v.intercept[name] = cfg })
}

func (c *Context) isolateFor(name string) *label {
	if c.v.isolate == nil {
		return nil
	}
	return c.v.isolate[name]
}

// ServiceConfig 返回 name 的有效服务配置（逐祖先合并，越近的覆盖越远的）。
func (c *Context) ServiceConfig(name string) ServiceConfig {
	// 从最远祖先写到最近祖先:反过来写会让外层覆盖内层,
	// 与「子作用域更具体」的直觉相反,而且只配一层时看不出来。
	var chain []*Context
	for cur := c; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	out := ServiceConfig{}
	for i := len(chain) - 1; i >= 0; i-- {
		if cfg, ok := chain[i].v.intercept[name]; ok {
			for k, v := range cfg {
				out[k] = v
			}
		}
	}
	return out
}

// Service 取一个服务值。当前插件必须已在 Inject 中声明该名字，
// 否则即使服务存在也返回错误——这是「可见性」门禁，不是性能优化。
func (c *Context) Service(name string) (any, error) {
	if f := c.fiber; f != nil {
		if !f.plugin.Inject().has(name) {
			return nil, Fail(CodeNotInjected, "插件 %s 未声明依赖服务 %q", f.plugin.Name(), name)
		}
	}
	im := c.app.services.resolve(name, c.isolateFor(name))
	if im == nil {
		return nil, Fail(CodeServiceMissing, "服务 %q 尚未提供", name)
	}
	if c.fiber != nil {
		c.fiber.Touch()
	}
	return im.value, nil
}

// TryService 绕过可见性探测，供「可选依赖」与诊断使用。
func (c *Context) TryService(name string) (any, bool) {
	im := c.app.services.resolve(name, c.isolateFor(name))
	if im == nil {
		return nil, false
	}
	if c.fiber != nil {
		c.fiber.Touch()
	}
	return im.value, true
}

// Principal 返回当前 fiber 的插件名(无 fiber 时为空)。
func (c *Context) Principal() string {
	if c.fiber == nil {
		return ""
	}
	return c.fiber.plugin.Name()
}

// Context 返回携带调用者身份的 context,供跨插件服务调用使用。
//
// 不缓存:身份只取决于 fiber;调用方每次拿一个新 context 更安全(不会被篡改)。
func (c *Context) Context() context.Context {
	return WithPrincipal(context.Background(), c.Principal())
}

// Service 是类型化取服务的辅助函数；类型不符会明确报错而不是返回零值。
func Service[T any](c *Context, name string) (T, error) {
	var zero T
	v, err := c.Service(name)
	if err != nil {
		return zero, err
	}
	t, ok := v.(T)
	if !ok {
		return zero, Fail(CodeInvalid, "服务 %q 的类型是 %T，不是 %T", name, v, zero)
	}
	return t, nil
}

// ProvideOption 调整服务契约。
type ProvideOption func(*ServiceSpec)

// WithVersion 声明服务契约的主版本。
func WithVersion(v int) ProvideOption { return func(s *ServiceSpec) { s.Version = v } }

// WithKind 声明服务形态（value / 未来的 stream、router）。
func WithKind(k string) ProvideOption { return func(s *ServiceSpec) { s.Kind = k } }

// Provide 注册一个服务实现。只有插件可以 provide；
// 注册本身是一个副作用，fiber 卸载时自动注销。
func (c *Context) Provide(name string, v any, opts ...ProvideOption) (Dispose, error) {
	if c.fiber == nil {
		return nil, Fail(CodeNotProvider, "只有插件可以提供服务 %q", name)
	}
	if v == nil {
		return nil, Fail(CodeInvalid, "服务 %q 的值不能为 nil", name)
	}
	spec := ServiceSpec{Name: name, Version: 1, Kind: "value"}
	for _, o := range opts {
		o(&spec)
	}
	im, err := c.app.services.provide(c.fiber, c.isolateFor(name), spec, v)
	if err != nil {
		return nil, err
	}
	// 登记「我提供的实现」:卸载时要按它通知依赖方,而不是按我自己的依赖。
	c.fiber.rememberProvided(im)
	e, addErr := c.fiber.scope.add("provide:"+name, func(context.Context) error {
		c.app.services.release(im)
		c.fiber.forgetProvided(im)
		return nil
	}, c.app.opts.EffectTimeout)
	if addErr != nil {
		c.app.services.release(im)
		c.fiber.forgetProvided(im)
		return nil, addErr
	}
	// 新服务可能让等待中的插件就绪。
	c.app.recompute()
	return e.Dispose, nil
}

// Set 更新服务值，只允许提供者自己调用（单写者）。
func (c *Context) Set(name string, v any) error {
	if c.fiber == nil {
		return Fail(CodeNotProvider, "只有插件可以更新服务 %q", name)
	}
	im := c.app.services.resolve(name, c.isolateFor(name))
	if im == nil {
		return Fail(CodeServiceMissing, "服务 %q 尚未提供", name)
	}
	return c.app.services.set(im, c.fiber, v)
}

// Effect 登记一个托管副作用：立即执行注册逻辑，返回的撤销函数入栈，
// fiber 卸载时按后进先出回收。
func (c *Context) Effect(label string, fn func() (Dispose, error)) (Dispose, error) {
	if c.fiber == nil {
		return nil, Fail(CodeInvalid, "只有插件可以注册副作用 %q", label)
	}
	d, err := fn()
	if err != nil {
		return nil, err
	}
	e, err := c.fiber.scope.add(label, d, c.app.opts.EffectTimeout)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return Nop, nil
	}
	return e.Dispose, nil
}

// On 注册普通监听器。
func (c *Context) On(name string, fn Handler, opts ...OnOption) Dispose {
	l := &listener{h: fn, owner: c.ownerName()}
	for _, o := range opts {
		o(l)
	}
	d := c.app.events.add(name, l)
	c.hold(d)
	return d
}

// OnWaterfall 注册中间件式监听器。
func (c *Context) OnWaterfall(name string, fn WaterfallHandler, opts ...OnOption) Dispose {
	l := &listener{w: fn, owner: c.ownerName()}
	for _, o := range opts {
		o(l)
	}
	d := c.app.events.add(name, l)
	c.hold(d)
	return d
}

// hold 把「注销监听器」挂到当前 fiber 的作用域上；
// 根上下文没有 fiber，调用方需自行持有返回的 disposer。
func (c *Context) hold(d Dispose) {
	if c.fiber == nil {
		return
	}
	if _, err := c.fiber.scope.add("listener", d, c.app.opts.EffectTimeout); err != nil {
		c.app.log.Warn("注册监听器失败", "err", err)
	}
}

func (c *Context) ownerName() string {
	if c.fiber != nil {
		return c.fiber.id
	}
	return "root"
}

// Emit 广播事件：监听器失败只记录，不影响其余监听器。
func (c *Context) Emit(name string, args ...any) {
	c.EmitContext(context.Background(), name, args...)
}

// EmitContext 用调用方上下文广播事件:ctx 取消时,监听器收到的子上下文同样被取消。
func (c *Context) EmitContext(ctx context.Context, name string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.app.events.emit(ctx, c, name, args)
}

// Parallel 并发执行全部监听器并聚合错误。
func (c *Context) Parallel(ctx context.Context, name string, args ...any) error {
	return c.app.events.parallel(ctx, c, name, args)
}

// Serial 串行执行，遇到第一个错误即停。
func (c *Context) Serial(ctx context.Context, name string, args ...any) error {
	return c.app.events.serial(ctx, c, name, args)
}

// Bail 串行执行，第一个返回非 nil 的监听器胜出。
func (c *Context) Bail(ctx context.Context, name string, args ...any) (any, error) {
	return c.app.events.bail(ctx, c, name, args)
}

// Waterfall 以中间件链方式执行：监听器可改写输入，也可不调 Next 直接短路。
func (c *Context) Waterfall(ctx context.Context, name string, input any, args ...any) (any, error) {
	return c.app.events.waterfall(ctx, c, name, input, args)
}

// Log 返回带插件命名空间的日志器。
func (c *Context) Log() *slog.Logger {
	if c.fiber == nil {
		return c.app.log
	}
	return c.app.log.With("plugin", c.fiber.plugin.Name(), "fiber", c.fiber.id)
}

// Config 返回当前插件的配置快照；根上下文返回空配置。
func (c *Context) Config() Config {
	if c.fiber == nil {
		return NewConfig(nil)
	}
	return c.fiber.cfg
}

// Plugin 在当前作用域下挂载一个子插件。
func (c *Context) Plugin(p Plugin, cfg Config) (*Fiber, error) {
	return c.app.mount(c, p, cfg)
}

// Dispose 回收当前 fiber（根上下文则停止整个应用）。
func (c *Context) Dispose(ctx context.Context) error {
	if c.fiber != nil {
		return c.fiber.Dispose(ctx)
	}
	return c.app.Stop(ctx)
}
