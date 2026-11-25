package kernel

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Handler 是普通监听器:用于 emit / parallel / serial / bail。
//
// ctx 是本次派发的取消上下文:父上下文取消或本监听器超时时被取消。
// 监听器应当把它当作「该收尾了」的信号并尽快返回;内核无法杀死 goroutine,
// 只能停止等待,所以遵守 ctx 是插件自己的责任。
type Handler func(ctx context.Context, c *Context, args []any) (any, error)

// Next 是 waterfall 链里的「继续」;不调用即短路(用于否决/改写)。
type Next func(any) (any, error)

// WaterfallHandler 是中间件式监听器:可改写输入,也可不调 Next 直接否决。
type WaterfallHandler func(ctx context.Context, c *Context, input any, next Next) (any, error)

// OnOption 调整监听器行为。
type OnOption func(*listener)

// maxListenerTimeout 是单个监听器的执行上限(design §7.3)。
// 超时只能「停止等待」,不能杀死 goroutine;允许把上限设到几分钟,
// 等于把内核的响应时间交给插件自行决定,所以这里截断。
const maxListenerTimeout = 2 * time.Second

// WithPriority 设置优先级:数值越大越先执行;同级按注册顺序。
func WithPriority(p int) OnOption { return func(l *listener) { l.priority = p } }

// WithTimeout 设置单个监听器的执行上限。
func WithTimeout(d time.Duration) OnOption { return func(l *listener) { l.timeout = d } }

// WithOnce 让监听器只触发一次。
func WithOnce() OnOption { return func(l *listener) { l.once = true } }

type listener struct {
	id       uint64
	name     string
	priority int
	timeout  time.Duration
	once     bool
	h        Handler
	w        WaterfallHandler
	owner    string
	mu       sync.Mutex
	fired    bool
}

// listenerResult 是监听器的返回值。它经带缓冲 channel 回传:
// 超时后监听器 goroutine 仍会写一次结果,缓冲保证它不会永久阻塞在发送上。
type listenerResult struct {
	value any
	err   error
}

// eventHub 保存全部监听器。它只做分发,不认识任何事件语义。
type eventHub struct {
	mu         sync.Mutex
	ls         map[string][]*listener
	seq        atomic.Uint64
	defTimeout time.Duration
	sealed     bool // App.Stop 之后封口:不再派发任何事件
}

func newEventHub(defTimeout time.Duration) *eventHub {
	if defTimeout <= 0 {
		defTimeout = 200 * time.Millisecond
	}
	if defTimeout > maxListenerTimeout {
		defTimeout = maxListenerTimeout
	}
	return &eventHub{ls: make(map[string][]*listener), defTimeout: defTimeout}
}

func (h *eventHub) add(name string, l *listener) Dispose {
	h.mu.Lock()
	l.id = h.seq.Add(1)
	l.name = name
	if l.timeout <= 0 {
		l.timeout = h.defTimeout
	}
	if l.timeout > maxListenerTimeout {
		l.timeout = maxListenerTimeout
	}
	h.ls[name] = append(h.ls[name], l)
	h.mu.Unlock()
	return func(context.Context) error {
		h.remove(name, l.id)
		return nil
	}
}

// seal 封口:停止之后不再派发。没有这一步,「已停止」只是几个字段,
// 插件仍能在 Stop 之后收到回调并产生副作用。
func (h *eventHub) seal() {
	h.mu.Lock()
	h.sealed = true
	h.mu.Unlock()
}

// active 返回当前是否仍允许派发。
func (h *eventHub) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.sealed
}

func (h *eventHub) remove(name string, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.ls[name]
	for i, l := range list {
		if l.id == id {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(h.ls, name)
		return
	}
	h.ls[name] = list
}

func (h *eventHub) snapshot(name string) []*listener {
	h.mu.Lock()
	defer h.mu.Unlock()
	src := h.ls[name]
	out := make([]*listener, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].priority != out[j].priority {
			return out[i].priority > out[j].priority
		}
		return out[i].id < out[j].id
	})
	return out
}

func (h *eventHub) names() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.ls))
	for n := range h.ls {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// take 处理 once:返回 true 表示本次可执行,并在执行前从表里摘除。
func (h *eventHub) take(l *listener) bool {
	if !l.once {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fired {
		return false
	}
	l.fired = true
	h.remove(l.name, l.id)
	return true
}

func (h *eventHub) invoke(ctx context.Context, c *Context, l *listener, args []any) (any, error) {
	if !h.take(l) {
		return nil, nil
	}
	sub, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	done := make(chan listenerResult, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- listenerResult{err: Fail(CodePanic, "监听器 panic: %v", rec)}
			}
		}()
		v, err := l.h(sub, c, args)
		done <- listenerResult{value: v, err: err}
	}()

	select {
	case r := <-done:
		return r.value, r.err
	case <-sub.Done():
		// 超时/取消:停止等待。监听器拿到的是同一个子 ctx,应当自行收尾。
		return nil, Fail(CodeTimeout, "监听器 %q 超时(%s)", l.name, l.timeout)
	}
}

func (h *eventHub) emit(ctx context.Context, c *Context, name string, args []any) {
	if !h.active() {
		return
	}
	for _, l := range h.snapshot(name) {
		if _, err := h.invoke(ctx, c, l, args); err != nil {
			// emit 是广播:单个监听器失败不能影响其余监听器,记录即可。
			c.app.log.Warn("事件监听失败", "event", name, "err", err)
		}
	}
}

func (h *eventHub) parallel(ctx context.Context, c *Context, name string, args []any) error {
	if !h.active() {
		return nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, l := range h.snapshot(name) {
		wg.Add(1)
		go func(l *listener) {
			defer wg.Done()
			if _, err := h.invoke(ctx, c, l, args); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (h *eventHub) serial(ctx context.Context, c *Context, name string, args []any) error {
	if !h.active() {
		return nil
	}
	for _, l := range h.snapshot(name) {
		if _, err := h.invoke(ctx, c, l, args); err != nil {
			return err
		}
	}
	return nil
}

func (h *eventHub) bail(ctx context.Context, c *Context, name string, args []any) (any, error) {
	if !h.active() {
		return nil, nil
	}
	for _, l := range h.snapshot(name) {
		res, err := h.invoke(ctx, c, l, args)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
	}
	return nil, nil
}

func (h *eventHub) waterfall(ctx context.Context, c *Context, name string, input any, args []any) (any, error) {
	if !h.active() {
		return input, nil
	}
	ls := h.snapshot(name)
	var run func(i int, in any) (any, error)
	run = func(i int, in any) (any, error) {
		if i >= len(ls) {
			return in, nil
		}
		l := ls[i]
		if !h.take(l) {
			return run(i+1, in)
		}
		sub, cancel := context.WithTimeout(ctx, l.timeout)
		defer cancel()

		// 超时闸门:本次派发一旦超时,Next 一律被拒绝。
		//
		// 为什么不能只「不等了」:waterfall 是策略/拦截的唯一原语(design §7.2),
		// 一个超时的审批/沙箱闸门如果还能把请求放进下游,就是 fail-open,
		// 直接违反 AGENTS P7「门禁必须确定」。这条约束与监听器是否遵守 ctx 无关。
		var (
			mu       sync.Mutex
			timedOut bool
			called   bool
		)
		next := func(v any) (any, error) {
			mu.Lock()
			if timedOut {
				mu.Unlock()
				return nil, Fail(CodeTimeout, "waterfall 监听器 %q 已超时(%s),拒绝继续下游", l.name, l.timeout)
			}
			if called {
				mu.Unlock()
				return nil, Fail(CodeInvalid, "waterfall 监听器 %q 重复调用 Next", l.name)
			}
			// 双保险:以子上下文的截止时间为准,避免「超时与调用 Next」的竞态窗口里
			// 放行下游。截止时间之前调用属于正常放行(此时下游已在执行中)。
			if err := sub.Err(); err != nil {
				timedOut = true
				mu.Unlock()
				return nil, Fail(CodeTimeout, "waterfall 监听器 %q 已超时(%s),拒绝继续下游", l.name, l.timeout)
			}
			called = true
			mu.Unlock()
			return run(i+1, v)
		}

		done := make(chan listenerResult, 1)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					done <- listenerResult{err: Fail(CodePanic, "waterfall 监听器 panic: %v", rec)}
				}
			}()
			v, err := l.w(sub, c, in, next)
			done <- listenerResult{value: v, err: err}
		}()

		select {
		case r := <-done:
			return r.value, r.err
		case <-sub.Done():
			mu.Lock()
			timedOut = true
			mu.Unlock()
			return nil, Fail(CodeTimeout, "waterfall 监听器 %q 超时(%s)", l.name, l.timeout)
		}
	}
	return run(0, input)
}

// 诊断用:当前监听器数量(重载测试据此判断有没有泄漏)。
func (h *eventHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, list := range h.ls {
		n += len(list)
	}
	return n
}
