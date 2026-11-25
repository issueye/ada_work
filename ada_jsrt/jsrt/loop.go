// Package jsrt 的事件循环接入:一次宿主调用 = 一次 loop.Run。
//
// 契约与理由见 README「事件循环」。
// 这里只放「怎么驱动循环」,不能放业务判断(执行层不认识 agent 词汇)。
package jsrt

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
)

//go:embed boot.js boot_timers.js prelude.js fsx.js node_fs.js node_fs_promises.js
var bootFS embed.FS

const (
	// entrySpecifier 是 boot 模块引用插件入口用的虚拟说明符。
	// 它不进 Artifact,只在 Link 时由这里解析,所以不会与插件的相对导入冲突。
	entrySpecifier = "ada:entry"
	// timersSpecifier 是定时器记账模块:必须先于用户模块求值,才能包住它们看到的定时器。
	timersSpecifier = "ada:boot"
	// fsxSpecifier 是 node:fs 补齐层的实现模块(callback / promise / sync 三形态)。
	fsxSpecifier = "ada:fsx"
	// baseFSSpecifier / basePromisesSpecifier 指向 moejs 自带的两个 fs 模块,
	// 包装模块只从它们那里取「已有能力」。
	baseFSSpecifier       = "ada:nodefs"
	basePromisesSpecifier = "ada:nodefs/promises"
	// invokeExport / invokeFnExport 是 boot 模块交给宿主调用的两个入口。
	invokeExport   = "__adaInvoke"
	invokeFnExport = "__adaInvokeFn"
	// internalPrefix 是 boot 模块自己导出的名字前缀,不出现在 Exports() 诊断里。
	internalPrefix = "__ada"
)

// ErrUnsettled 表示入口的 Promise 永远不会结算:循环已经没有别的事可做。
//
// 它不是超时(超时由调用 ctx 的 deadline 走中断),而是「await 了一个等不到的东西」,
// 单独给一个错误码,免得被误读成性能问题。
var ErrUnsettled = errors.New("jsrt: 调用未结算:入口 await 的 promise 永远不会完成")

// bootModules 是进程内编译一次的引导模块。
//
// *moejs.Module 只持有编译结果,求值发生在每个 Runtime 的 Link/Load 里,
// 所以跨运行时共享是安全的(与 nodemods 的做法一致)。
var bootModules struct {
	once        sync.Once
	runtime     *moejs.Module
	timers      *moejs.Module
	fsx         *moejs.Module
	nodeFS      *moejs.Module
	nodeFSAsync *moejs.Module
	err         error
}

// compileBoot 编译并缓存引导模块与 node:fs 包装模块。
func compileBoot() (boot *moejs.Module, timers *moejs.Module, fsx *moejs.Module, nodeFS *moejs.Module, nodeFSAsync *moejs.Module, err error) {
	bootModules.once.Do(func() {
		compile := func(name, file string) *moejs.Module {
			if bootModules.err != nil {
				return nil
			}
			src, readErr := bootFS.ReadFile(file)
			if readErr != nil {
				bootModules.err = fmt.Errorf("jsrt: 读取引导模块 %s 失败: %w", file, readErr)
				return nil
			}
			m, compileErr := moejs.Compile(name, string(src))
			if compileErr != nil {
				bootModules.err = fmt.Errorf("jsrt: 编译引导模块 %s 失败: %w", file, compileErr)
				return nil
			}
			return m
		}
		bootModules.timers = compile(timersSpecifier, "boot_timers.js")
		bootModules.fsx = compile(fsxSpecifier, "fsx.js")
		bootModules.nodeFS = compile("__ada_node_fs.js", "node_fs.js")
		bootModules.nodeFSAsync = compile("__ada_node_fs_promises.js", "node_fs_promises.js")
		bootModules.runtime = compile("__ada_runtime.js", "boot.js")
	})
	return bootModules.runtime, bootModules.timers, bootModules.fsx, bootModules.nodeFS, bootModules.nodeFSAsync, bootModules.err
}

// newLoop 为一个运行时创建事件循环,并安装 Node 形状的定时器。
//
// OnError 只记日志:定时器回调里的异常属于「这次调用的副产物」,不该让主调用失败
// (主调用自己的异常走 boot 的 onErr,那才是调用方的错误)。
func (r *Runtime) newLoop(raw *moejs.Runtime) (*eventloop.Loop, error) {
	loop, err := eventloop.New(raw, eventloop.Options{OnError: r.onLoopError})
	if err != nil {
		return nil, fmt.Errorf("jsrt: 创建事件循环失败: %w", err)
	}
	return loop, nil
}

// onLoopError 是事件循环的回调错误出口;只在循环 goroutine 上被调用。
func (r *Runtime) onLoopError(err error) {
	if r.opts.Logf != nil {
		r.opts.Logf("jsrt: 事件循环回调异常(不影响当前调用): %v", err)
	}
	r.emitEvent(Event{Kind: "uncaught_exception", Err: err})
	r.mu.Lock()
	raw := r.raw
	r.mu.Unlock()
	if raw != nil {
		msg := strings.TrimPrefix(err.Error(), "Error: ")
		obj := raw.Realm().NewError(engine.KindError, "%s", msg)
		r.emitProcess("uncaughtException", engine.ObjectValue(obj))
	}
}

// emitProcess 把事件投递给 JS 的 process 事件表（若 process 模块已加载）。
func (r *Runtime) emitProcess(name string, value engine.Value) {
	r.mu.Lock()
	raw := r.raw
	r.mu.Unlock()
	if raw == nil {
		return
	}
	realm := raw.Realm()
	fn, err := realm.Global.GetProp(realm, realm.KeyFromGoString("__adaProcessEmit"))
	if err != nil || !engine.IsCallable(fn) {
		return
	}
	nameVal, err := raw.FromGo(name)
	if err != nil {
		return
	}
	if _, err := realm.Call(fn, engine.Undefined(), []engine.Value{nameVal, value}); err != nil && r.opts.Logf != nil {
		r.opts.Logf("jsrt: process.%s 监听器异常: %v", name, err)
	}
}

// returnConvError 表示调用结果无法转成 Go 值。
//
// 单独成类型是为了让回调类调用(emit / effect 清理)能沿用历史语义:
// 它们的返回值对宿主没有意义,转换失败按「没有返回值」处理,而不是让事件失败。
type returnConvError struct{ err error }

func (e *returnConvError) Error() string {
	return fmt.Sprintf("jsrt: 返回值无法转成 Go 值: %v", e.err)
}

func (e *returnConvError) Unwrap() error { return e.err }

// loopCall 是一次「进入 JS 并等它结算」的结局槽。
//
// 只有第一次结算算数:入口正常返回、Promise 结算、或失败回调,三者只可能发生一次。
type loopCall struct {
	res  chan callResult
	once sync.Once
}

func (c *loopCall) settle(v any, err error) {
	c.once.Do(func() { c.res <- callResult{value: v, err: err} })
}

// runCall 在事件循环上执行一次调用,等入口结算后返回。
//
// build 必须在事件循环上把调用挂上去(只调用一次 raw.Call);它返回的错误表示
// 「挂上去这一步就失败了」(例如参数转换失败),此时不会有结算回调。
func (r *Runtime) runCall(build func(rt *moejs.Runtime, ok, fail moejs.Value) error) (any, error) {
	r.mu.Lock()
	loop, raw, broken := r.loop, r.raw, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return nil, ErrBroken
	case loop == nil || raw == nil:
		return nil, ErrClosed
	}

	c := &loopCall{res: make(chan callResult, 1)}
	runErr := loop.Run(func(rt *moejs.Runtime) error {
		ok := rt.Function("__adaOK", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			var v any
			if len(args) > 0 {
				got, err := rt.ToGo(args[0])
				if err != nil {
					// 转换失败也走结算槽:调用方需要的是一个错误,不是「没结果」。
					c.settle(nil, &returnConvError{err: err})
					return engine.Undefined(), nil
				}
				v = got
			}
			c.settle(v, nil)
			return engine.Undefined(), nil
		})
		fail := rt.Function("__adaFail", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			msg := "未知错误"
			if len(args) > 0 {
				if s, err := rt.ToGo(args[0]); err == nil {
					msg = fmt.Sprint(s)
				}
			}
			c.settle(nil, fmt.Errorf("jsrt: JS 异常: %s", msg))
			return engine.Undefined(), nil
		})
		return build(rt, ok, fail)
	})

	// 结算优先:循环在结算之后还可能因为其他原因结束(回调异常、Terminate),
	// 那些都不该覆盖掉已经拿到手的调用结果。
	select {
	case got := <-c.res:
		return got.value, got.err
	default:
	}
	switch {
	case runErr == nil:
		return nil, ErrUnsettled
	case errors.Is(runErr, eventloop.ErrTerminated):
		return nil, ErrClosed
	default:
		return nil, r.wrap(raw, runErr)
	}
}

// runOnLoop 在事件循环上执行一段不需要 await 的同步 JS(waterfall 中间件)。
//
// 用它的入口在语义上就是同步的:middleware 必须在返回前给出值,才能把 next 链续下去。
func (r *Runtime) runOnLoop(fn func(*moejs.Runtime) error) error {
	r.mu.Lock()
	loop, raw, broken := r.loop, r.raw, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return ErrBroken
	case loop == nil || raw == nil:
		return ErrClosed
	}
	if err := loop.Run(fn); err != nil {
		if errors.Is(err, eventloop.ErrTerminated) {
			return ErrClosed
		}
		return r.wrap(raw, err)
	}
	return nil
}

// joinPayload 把逐个 JSON 编码的参数拼成一个数组字面量。
//
// 每个 payload 已经是合法 JSON,拼接即可;不重新序列化是为了不在边界上引入
// 第二次编码(数字精度、HTML 转义、字段顺序都可能因此改变)。
func joinPayload(payload [][]byte) string {
	if len(payload) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.Grow(len(payload)*8 + 2)
	b.WriteByte('[')
	for i, p := range payload {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(p)
	}
	b.WriteByte(']')
	return b.String()
}

// terminateLoop 结束本运行时的事件循环:定时器、立即任务与未结算的 promise 一起丢弃。
//
// 必须在循环之外(调用方 goroutine)调用;Terminate 会等正在跑的循环收尾。
func (r *Runtime) terminateLoop() {
	r.mu.Lock()
	loop := r.loop
	r.loop = nil
	r.mu.Unlock()
	if loop != nil {
		loop.Terminate()
	}
}
