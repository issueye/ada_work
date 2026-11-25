package kernel

import (
	"context"
	"errors"
	"sync"
	"time"
)

// scope 是「托管副作用」的栈：所有注册都入栈，关闭时逆序执行。
//
// 为什么逆序：后创建的资源可能依赖先创建的（监听器依赖连接、子插件依赖父服务），
// 按创建的反序回收，依赖方总是先于被依赖方消失。
type scope struct {
	mu      sync.Mutex
	entries []*entry
	closed  bool
}

type entry struct {
	label   string
	timeout time.Duration
	d       Dispose
	scope   *scope

	mu   sync.Mutex
	done bool // 已执行或已摘除；用互斥而不是 sync.Once，避免重入时自锁
	err  error
}

func newScope() *scope { return &scope{} }

// add 登记一个副作用，返回它的条目（调用方据此主动摘除）与错误。
func (s *scope) add(label string, d Dispose, timeout time.Duration) (*entry, error) {
	if d == nil {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	e := &entry{label: label, timeout: timeout, d: d, scope: s}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, Fail(CodeEffectDisposed, "作用域已关闭，无法注册副作用 %q", label)
	}
	s.entries = append(s.entries, e)
	s.mu.Unlock()
	return e, nil
}

// Dispose 返回一个幂等的撤销函数，并把它从栈里摘除。
func (e *entry) Dispose(ctx context.Context) error {
	err := e.run(ctx)
	e.scope.remove(e)
	return err
}

// run 执行一次析构。重入安全：先置 done，再执行。
func (e *entry) run(ctx context.Context) error {
	e.mu.Lock()
	if e.done {
		err := e.err
		e.mu.Unlock()
		return err
	}
	e.done = true
	e.mu.Unlock()

	c, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	err := safeDispose(c, e.d)

	e.mu.Lock()
	e.err = err
	e.mu.Unlock()
	return err
}

// detach 把条目从作用域摘除且不执行：用于「子级主动 Dispose、父作用域仍在」的场景，
// 否则父作用域要等到整体关闭才回收这条记录，长期挂载/卸载会无限增长。
func (e *entry) detach() {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
	e.scope.remove(e)
}

// close 逆序执行全部副作用。单个失败不影响其余，错误聚合返回。
func (s *scope) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	entries := append([]*entry(nil), s.entries...)
	s.entries = nil
	s.mu.Unlock()

	var errs []error
	for i := len(entries) - 1; i >= 0; i-- {
		if err := entries[i].run(ctx); err != nil {
			errs = append(errs, wrapDisposeError(entries[i].label, err))
		}
	}
	return errors.Join(errs...)
}

// wrapDisposeError 给回收错误附上副作用标签,但保留原有错误码。
// 一律改写成 EINVALID 会把「disposer panic(EPANIC)」「超时(ETIMEOUT)」
// 抹平成同一种错误,调用方再也无法按码分流。
func wrapDisposeError(label string, err error) error {
	var ke *Error
	if errors.As(err, &ke) {
		return Wrap(ke.Code, err, "副作用 %q 回收失败", label)
	}
	return Wrap(CodeInvalid, err, "副作用 %q 回收失败", label)
}

// remove 从栈中摘除一个条目。
func (s *scope) remove(e *entry) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.entries {
		if x == e {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return
		}
	}
}

// safeDispose 把 panic 转成错误，保证一个坏副作用不会中断整轮回收。
func safeDispose(ctx context.Context, d Dispose) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = Fail(CodePanic, "副作用 panic: %v", rec)
		}
	}()
	return d(ctx)
}

// size 用于诊断与测试（如「重载后副作用数量必须为 0」）。
func (s *scope) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// labels 返回当前登记的副作用标签(按登记顺序),供 Inspect 诊断使用。
//
// 只暴露标签,不暴露 disposer:诊断需要的是「有什么、属于谁」,
// 而不是拿到一个能提前触发回收的函数。
func (s *scope) labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.label)
	}
	return out
}
