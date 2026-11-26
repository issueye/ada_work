// Package jsrt 是脚本执行层:每个实例一个 moejs Runtime、一个专属 goroutine。
//
// 边界写死:
//   - Runtime 只能在其专属 goroutine 上使用;只有 Interrupt/ClearInterrupt 可跨协程;
//   - 跨边界只传 JSON,不传函数、Promise、生成器;
//   - 字节码不落盘、不缓存,装配失败只能重新编译加载。
package jsrt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
	"github.com/MoeForge/moejs/eventloop"
	mojsnode "github.com/MoeForge/moejs/node"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt/nodemods"
)

// interruptGrace 是取消后等待 worker 收尾的上限。
// 超过它说明 JS 没有响应中断(例如卡在宿主函数里),运行时视为损坏。
const interruptGrace = 2 * time.Second

var (
	// ErrClosed 表示运行时已关闭或尚未就绪。
	ErrClosed = errors.New("jsrt: 运行时已关闭")
	// ErrBroken 表示运行时在中断后未能收尾,不应再被复用。
	ErrBroken = errors.New("jsrt: 运行时已损坏")
	// ErrStartupTimeout 表示入口（含顶层 await）在启动上限内没有完成。
	ErrStartupTimeout = errors.New("jsrt: 启动超时")
)

// ExitError 是 ExitPolicy=ExitRequest 时 process.exit 的结果。
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	if e == nil {
		return "jsrt: 进程退出"
	}
	return fmt.Sprintf("jsrt: 进程退出(code=%d)", e.Code)
}

// Service 是 JS 侧 ctx.provide 的产物,也是 Go 侧拿到的服务值。
//
// 对象上的函数成员变成方法(调用转发到 JS worker),其余成员按 JSON 转成 Data;
// 非对象值(标量/数组)放在 Value 里。方法调用与普通调用共用中断/超时语义。
type Service struct {
	rt      *Runtime
	name    string
	methods []string
	data    map[string]any
	value   any
	ids     map[string]uint64
}

// Name 返回服务名。
func (s *Service) Name() string { return s.name }

// Methods 返回方法名(排序后的副本)。
func (s *Service) Methods() []string {
	out := make([]string, len(s.methods))
	copy(out, s.methods)
	return out
}

// Data 返回非函数成员。返回值只读,调用方不得修改。
func (s *Service) Data() map[string]any { return s.data }

// Value 返回非对象服务的原始值;对象服务返回 nil。
func (s *Service) Value() any { return s.value }

// Call 调用服务方法。参数与返回值只走 JSON,与宿主调用 JS 的其他入口一致。
func (s *Service) Call(ctx context.Context, method string, args ...any) (any, error) {
	id, ok := s.ids[method]
	if !ok {
		return nil, fmt.Errorf("jsrt: 服务 %q 没有方法 %q", s.name, method)
	}
	payload := make([][]byte, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("jsrt: 方法 %q 的参数 %d 无法序列化: %w", method, i, err)
		}
		payload[i] = b
	}
	return s.rt.invokeMethod(ctx, id, payload)
}

// handlerEntry 是登记在 worker 上的一次 JS 函数调用:函数本身与 this。
type handlerEntry struct {
	fn   moejs.Value
	this moejs.Value
}

// Host 是 JS 侧 ctx 对象能调到的宿主能力。
//
// 回调在 JS 的 worker goroutine 上执行,所以实现方必须自己保证线程安全,
// 并且**不得**直接操作宿主状态——桥接层负责把请求暂存,回到宿主协程再提交。
// 回调返回 error 会变成 JS 异常;nil 表示该能力未启用。
type Host struct {
	Provide func(name string, svc *Service) error
	Emit    func(event string, payload any) error
	// On 注册「宿主 → JS」的回调。invoke 可以在任意 goroutine 上调用:
	// 它会把请求投递到 worker 并在 ctx 取消时中断 JS。
	On func(event string, invoke func(ctx context.Context, payload any) (any, error)) error
	// Effect 登记一个 JS 清理函数;桥接层负责在卸载时按逆序调用 dispose。
	Effect func(label string, dispose func(ctx context.Context) error) error
	// Waterfall 注册一个可改写/否决的中间件。invoke 接收宿主的 next,
	// JS 侧通过同步调用 next 继续下游并拿到结果。
	Waterfall func(event string, invoke func(ctx context.Context, input any, next func(any) (any, error)) (any, error)) error
	// Shell 是 JS 侧 ctx.shell / 注入模块的宿主实现。
	Shell func(ctx context.Context, program string, args []string, dir string) (string, error)
	// ShellExec 是 ada:shell.exec 的宿主实现:options 是 JSON 字符串,
	// 返回值也是 JSON 字符串(stdout/stderr/exitCode/truncated/durationMs 等)。
	// 与 Shell 分开是为了不破坏已有脚本插件的字符串返回契约。
	ShellExec func(ctx context.Context, program string, args []string, dir string, options string) (string, error)
	// Tool 是 JS 侧 ctx.tool(spec, handler) 的宿主实现;invoke 在工具被调用时执行 JS handler。
	// timeout 来自 spec.timeoutMs,0 表示使用 tools seam 的默认超时。
	Tool func(name, summary string, input map[string]any, readOnly bool, timeout time.Duration, invoke func(ctx context.Context, args any) (any, error)) error
	// FSRead/FSWrite 是注入模块 ada:fs 的宿主实现。
	FSRead   func(ctx context.Context, path string) (string, error)
	FSWrite  func(ctx context.Context, path, content string, createDirs bool) error
	FSEdit   func(ctx context.Context, path, old, new string, all bool) (int, error)
	FSSearch func(ctx context.Context, path, query string, limit int) (string, error)
	FSList   func(ctx context.Context, path string, recursive bool, limit int) (string, error)
	// PlanGet/PlanSet/PlanUpdate 是注入模块 ada:plan 的宿主实现。
	// 计划属于会话:实现从调用 ctx 取会话,没有会话时返回错误。
	PlanGet    func(ctx context.Context) (string, error)
	PlanSet    func(ctx context.Context, planJSON string) (string, error)
	PlanUpdate func(ctx context.Context, index int, status string) (string, error)
	// SessionCurrent/SessionAppend 是注入模块 ada:session 的宿主实现。
	// Current 返回会话 JSON 或 "null";Append 只能写插件自有事件(kind 由宿主加前缀)。
	SessionCurrent func(ctx context.Context) (string, error)
	SessionAppend  func(ctx context.Context, kind, dataJSON string) (uint64, error)
	// SessionHistory/SessionRecent 是 ada:session 的只读事件接口:
	// 返回事件数组 JSON;读取范围与大小上限由宿主决定。
	SessionHistory func(ctx context.Context, from uint64, limit int) (string, error)
	SessionRecent  func(ctx context.Context, limit int) (string, error)
	// StoreGet/StoreSet/StoreDelete/StoreList 是 ctx.store / ada:store 的宿主实现。
	// 值是 JSON 字符串;Get 在键不存在时返回 "null"。
	StoreGet    func(ctx context.Context, key string) (string, error)
	StoreSet    func(ctx context.Context, key, valueJSON string) error
	StoreDelete func(ctx context.Context, key string) error
	StoreList   func(ctx context.Context, prefix string) (string, error)
	// HTTPDo 执行一次 HTTP(S) 请求;spec 与返回值都是 JSON 字符串。
	// 是否受网络策略约束由宿主决定,jsrt 只负责转发。
	HTTPDo func(ctx context.Context, spec string) (string, error)
	// DNSLookup 解析域名;op 决定记录类型,返回值是 JSON 字符串。
	DNSLookup func(ctx context.Context, op, host, rrtype string) (string, error)
	// NetDial 建立一条 TCP 连接;连接句柄由运行时登记,Close 时统一关闭。
	NetDial func(ctx context.Context, network, address string, timeout time.Duration) (NetConn, error)
	// TLSDial 建立一条 TLS 连接;ServerName/Insecure/ALPN 来自 node:tls 的 options。
	// 未实现时 node:tls 模块不可用。
	TLSDial func(ctx context.Context, network, address string, opts TLSOptions) (NetConn, error)
	// NetListen 监听一个 TCP 地址;未实现时 net.createServer 不可用。
	NetListen func(ctx context.Context, network, address string) (NetListener, error)
	// TLSListen 监听一个 TLS 地址;未实现时 tls.createServer/https 服务端不可用。
	TLSListen func(ctx context.Context, network, address string, opts TLSOptions) (NetListener, error)
	// Secrets 按名读取宿主的 secret。实现由宿主接到自己的 secret 服务,
	// 授权与审计由 policy 装饰器负责;没有 secrets 服务时返回错误而不是空串,
	// 让调用方知道「不能读」与「读到了空值」是两件事。
	Secrets func(ctx context.Context, name string) (string, error)
}

// ErrReentrant 表示同一运行时在自己的回调链里被再次调用。
// 此时 worker 正阻塞等待宿主响应 next,再投递任务必然死锁,所以直接失败。
var ErrReentrant = errors.New("jsrt: 同一运行时的嵌套调用会死锁")

// NodeOptions 控制 Node 内置模块的安装。
//
// node:path / node:os / node:buffer 始终可用;node:fs / node:fs/promises
// 只有给出 FS 根时才可读写。Root 是宿主真实目录,Mount 是脚本看到的虚拟根。
type NodeOptions struct {
	FS     *NodeFSOptions
	Buffer bool
}

// NodeFSOptions 是 node:fs 的根与写权限。
type NodeFSOptions struct {
	Root    string
	Mount   string
	Cwd     string
	Write   bool
	Workers int
}

// Options 描述一次运行时装配。
type Options struct {
	Artifact *compile.Artifact
	// MemoryLimit 是 JS 堆上限(字节),0 表示不限。
	// Limits.MemoryBytes 是同义的新配置;两者都为零表示不限。
	MemoryLimit int64
	// Mode 选择事件循环模式；零值保持现有的调用边界模型。
	Mode LoopMode
	// Permissions 是文件/网络/子进程/环境变量的静态白名单；零值全部拒绝。
	Permissions Permissions
	// Limits 是资源限制；零值表示不额外限制。
	Limits Limits
	// ExitPolicy 控制 process.exit；零值是 ExitDeny。
	ExitPolicy ExitPolicy
	// DisableDynamicCode 关闭 eval 与 Function 构造器。
	DisableDynamicCode bool
	// Logf 接收 JS 侧 ctx.log / __adaLog 的日志;nil 表示丢弃。
	Logf func(format string, args ...any)
	// OnEvent 接收权限、异常、退出等可观测事件;nil 表示丢弃。
	OnEvent func(Event)
	// Host 提供 ctx.provide / ctx.emit 的落地实现。
	Host Host
	// Modules 是宿主注入的 moejs 模块(如 "ada:shell");Link 时优先于相对路径。
	Modules map[string]*moejs.Module
	// Globals 是加载前注入的全局变量(宿主能力对象)。
	Globals map[string]any
	// Node 配置 Node 内置模块;nil 表示只装默认的 path/os/buffer。
	// Deprecated: 文件权限请使用 Permissions.FS；该字段保留用于兼容旧调用。
	Node *NodeOptions
}

// Runtime 是一个插件实例的 JS 世界。
type Runtime struct {
	art  *compile.Artifact
	opts Options

	// 权限与限制（编译后的策略不可变，可并发使用）。
	mode        LoopMode
	perms       Permissions
	fsPerm      FSPermission
	fsPolicy    *fsPolicy
	fsEnforce   bool
	netPolicy   *netPolicy
	limits      Limits
	exitPolicy  ExitPolicy
	exitCode    int
	exitErr     error
	exited      bool
	onEvent     func(Event)
	memoryLimit int64

	jobs    chan func()
	quit    chan struct{}
	stopped chan struct{}
	quitOne sync.Once
	done    chan struct{}
	doneOne sync.Once

	// resident 表示事件循环已 Start；callMu 让常驻模式下的 Call 串行执行，
	// 保持现有 callCtx/handlers 的单调用假设。
	resident bool
	loopHold func(func(*moejs.Runtime))
	callMu   sync.Mutex

	mu      sync.Mutex
	raw     *moejs.Runtime
	nodeAPI *mojsnode.API
	hooks   map[string]moejs.Hook
	host    moejs.Value // 传给 apply 的 ctx 对象;只在 worker goroutine 上使用
	// loop 是这个运行时的事件循环(见 loop.go)。它在 worker goroutine 上以
	// 「一次调用一次 Run」的方式驱动,不后台常驻。
	loop   *eventloop.Loop
	broken bool
	closed bool

	// nodeRoot 是 node:fs 的工作区根(补齐层也用它);nodeMount/nodeWrite 是根对应的
	// 虚拟挂载点与写权限。三者只在 node:fs 安装成功时非零(见 nodefsx.go)。
	nodeRoot     *os.Root
	nodeHostRoot string // Root 的绝对路径,用于 fsx 的符号链接解析
	nodeMount    string
	nodeWrite    bool

	// clockStart 是运行时启动时刻,给 __adaHost.monoMs(单调时钟)做基准。
	clockStart time.Time

	// fds 是 node:fs 的文件描述符表(见 nodefsfd.go);用 fdMu 保护,
	// 因为异步读写在 goroutine 里执行。
	fdMu  sync.Mutex
	fdSeq uint64
	fds   map[uint64]*fdEntry

	// handlers 是 ctx.on 与 JS 服务方法共用的函数表。只在 worker goroutine 上读写:
	// 注册与调用都发生在 worker 上。
	handlers map[uint64]handlerEntry
	nextID   uint64
	// inWaterfall 记录「宿主正在等本运行时的 waterfall 处理器」。
	// 大于 0 时禁止再次调用本运行时,否则 worker 会在等待宿主响应 next 时死锁。
	inWaterfall int
	// inHost 记录「worker 正在回调宿主」。此时宿主要求本运行时执行回调,
	// 会绕回正在忙碌的 worker 而死锁,所以必须快速失败。
	inHost int
	// callCtx 是当前 worker 任务对应的调用 ctx。只在 worker goroutine 上读写:
	// 原生 shell/fs 回调必须沿用它,才能拿到调用级 workspace 绑定与取消信号。
	callCtx context.Context

	// netMu 保护 node:net 的连接句柄表。句柄由 worker 分配,
	// 但 Close 可能在任意协程上关闭全部连接,所以必须加锁。
	netMu        sync.Mutex
	netSeq       uint64
	netConns     map[uint64]NetConn
	listenerSeq  uint64
	netListeners map[uint64]NetListener
	// netPending 是已预留但尚未登记/失败的连接数，用于 MaxConnections 并发限流。
	netPending int

	// zlibStreams 是 node:zlib 的流式句柄（见 nodezlib.go）。
	zlibMu      sync.Mutex
	zlibSeq     uint64
	zlibStreams map[uint64]*zlibStream
}

// enterHost/leaveHost 标记 worker 正在进入宿主回调。
func (r *Runtime) enterHost() {
	r.mu.Lock()
	r.inHost++
	r.mu.Unlock()
}

func (r *Runtime) leaveHost() {
	r.mu.Lock()
	r.inHost--
	r.mu.Unlock()
}

// reentrant 报告「当前调用是否来自本运行时自己的宿主回调」。
func (r *Runtime) reentrant() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inHost > 0
}

// New 装配并加载 Artifact,返回可调用的运行时。
//
// ctx 只约束装配本身;调用期的取消请用 Call 的 ctx。
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Artifact == nil {
		return nil, fmt.Errorf("jsrt: Artifact 不能为 nil")
	}
	if err := opts.Artifact.Verify(); err != nil {
		return nil, err
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	fsPerm, err := opts.fsPermission()
	if err != nil {
		return nil, err
	}
	fsPolicy, err := newFSPolicy(fsPerm)
	if err != nil {
		return nil, err
	}
	netPolicy, err := newNetPolicy(opts.Permissions.Net)
	if err != nil {
		return nil, err
	}
	memoryLimit := opts.MemoryLimit
	if memoryLimit == 0 {
		memoryLimit = opts.Limits.MemoryBytes
	}
	r := &Runtime{
		art:          opts.Artifact,
		opts:         opts,
		mode:         opts.Mode,
		perms:        opts.Permissions,
		fsPerm:       fsPerm,
		fsPolicy:     fsPolicy,
		fsEnforce:    opts.Permissions.FS.Enabled,
		netPolicy:    netPolicy,
		limits:       opts.Limits,
		exitPolicy:   opts.ExitPolicy,
		onEvent:      opts.OnEvent,
		memoryLimit:  memoryLimit,
		jobs:         make(chan func()),
		quit:         make(chan struct{}),
		stopped:      make(chan struct{}),
		done:         make(chan struct{}),
		handlers:     make(map[uint64]handlerEntry),
		netConns:     make(map[uint64]NetConn),
		netListeners: make(map[uint64]NetListener),
		zlibStreams:  make(map[uint64]*zlibStream),
		fds:          make(map[uint64]*fdEntry),
		clockStart:   time.Now(),
	}
	go r.worker()

	ready := make(chan error, 1)
	if err := r.enqueue(ctx, func() { ready <- r.load(ctx) }); err != nil {
		r.shutdown()
		return nil, err
	}
	select {
	case err := <-ready:
		if err != nil {
			r.shutdown()
			return nil, err
		}
		return r, nil
	case <-ctx.Done():
		r.shutdown()
		return nil, fmt.Errorf("jsrt: 装配被取消: %w", ctx.Err())
	}
}

// worker 是唯一接触 moejs.Runtime 的 goroutine。
func (r *Runtime) worker() {
	defer close(r.stopped)
	for {
		select {
		case job := <-r.jobs:
			job()
		case <-r.quit:
			return
		}
	}
}

func (r *Runtime) enqueue(ctx context.Context, job func()) error {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return ErrClosed
	}
	// 在 worker 执行任务期间保存调用 ctx;原生 shell/fs 回调据此继承
	// workspace 绑定、取消与超时,而不是退回 context.Background。
	wrapped := func() {
		// Scoped 模式的任务在 worker 上跑，callCtx 在这里设置/清理；
		// Resident 的调用在 loop 上跑，由 callResident / load 自己设置。
		if r.mode != LoopResident {
			r.callCtx = ctx
			defer func() { r.callCtx = nil }()
		}
		job()
	}
	select {
	case r.jobs <- wrapped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.stopped:
		return ErrClosed
	}
}

// currentContext 返回当前 JS 任务对应的 ctx;只在 worker goroutine 上调用。
func (r *Runtime) currentContext() context.Context {
	if r.callCtx != nil {
		return r.callCtx
	}
	return context.Background()
}

// startupTimeout 是启动（含顶层 await）的上限；未配置时用 30s 兜底。
func (r *Runtime) startupTimeout() time.Duration {
	if r.limits.StartupTimeout > 0 {
		return r.limits.StartupTimeout
	}
	return 30 * time.Second
}

// postToLoop 把任务投递到常驻循环；只在 LoopResident 且循环可用时成功。
func (r *Runtime) postToLoop(fn func(*moejs.Runtime)) bool {
	r.mu.Lock()
	loop, resident, broken, closed := r.loop, r.resident, r.broken, r.closed
	r.mu.Unlock()
	if loop == nil || !resident || broken || closed {
		return false
	}
	return loop.RunOnLoop(fn)
}

// finish 关闭 Wait 的等待信号；可重复调用。
func (r *Runtime) finish() {
	r.doneOne.Do(func() { close(r.done) })
}

// Wait 等待常驻运行时关闭。当前没有 beforeExit，只有 Close 或退出请求会唤醒它。
func (r *Runtime) Wait(ctx context.Context) error {
	if r.mode != LoopResident {
		return errors.New("jsrt: Wait 只适用于 LoopResident")
	}
	select {
	case <-r.done:
		r.mu.Lock()
		err := r.exitErr
		r.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) closeTimeout() time.Duration {
	if r.limits.CloseTimeout > 0 {
		return r.limits.CloseTimeout
	}
	return 5 * time.Second
}

// requestExit 响应 process.exit(ExitRequest)：设置退出码并异步走 Close。
func (r *Runtime) requestExit(code int) {
	r.mu.Lock()
	if r.exited {
		r.mu.Unlock()
		return
	}
	r.exited = true
	r.exitCode = code
	r.exitErr = &ExitError{Code: code}
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), r.closeTimeout())
		defer cancel()
		_ = r.Close(ctx)
	}()
}

// load 在 worker 上执行:编译 Artifact 的每个模块 → Link → Load。
func (r *Runtime) load(ctx context.Context) error {
	raw := moejs.NewRuntime(moejs.Options{
		MemoryLimit:        r.memoryLimit,
		DisableDynamicCode: r.opts.DisableDynamicCode,
	})
	// 未处理的 Promise 拒绝至少要被宿主看见（Node 的 unhandledRejection）。
	// process 事件面在后续阶段接入；这里先统一走 OnEvent，避免静默丢失。
	raw.SetPromiseRejectionTracker(func(p moejs.Value, op moejs.PromiseRejectionOperation) {
		if op != moejs.PromiseRejectionReject {
			return
		}
		_, reason, _ := moejs.PromiseResult(p)
		r.emitEvent(Event{Kind: "unhandled_rejection", Err: r.rejectionError(reason)})
		r.emitProcess("unhandledRejection", reason)
	})
	// node:fs 的异步回调会从 worker 池回到这个 runtime;先登记 raw。
	r.mu.Lock()
	r.raw = raw
	r.mu.Unlock()
	// 事件循环必须先于 node 安装:异步 fs 的 Hold/Post 都挂在它上面。
	loop, err := r.newLoop(raw)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.loop = loop
	r.mu.Unlock()
	if err := r.installNode(raw, loop); err != nil {
		return err
	}
	// 宿主能力必须在 Load 之前注入:模块顶层就可能调用它们。
	// 裸 Runtime 没有 console(C4),所以日志必须显式给。
	//
	// 这里直接写 Realm 的 SetProp 而不是 SetGlobal:SetGlobal 会走 FromGo,
	// 而 FromGo 不认 engine.Value(函数值),结果会把它变成空对象。
	realm := raw.Realm()
	ctxObj := realm.NewObject()
	setFn := func(name string, fn moejs.NativeFunc) error {
		return ctxObj.SetProp(realm, realm.KeyFromGoString(name), raw.Function(name, 2, fn))
	}
	if err := setFn("log", r.nativeLog(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.log 失败: %w", err)
	}
	if err := setFn("provide", r.nativeProvide(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.provide 失败: %w", err)
	}
	if err := setFn("emit", r.nativeEmit(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.emit 失败: %w", err)
	}
	if err := setFn("on", r.nativeOn(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.on 失败: %w", err)
	}
	if err := setFn("effect", r.nativeEffect(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.effect 失败: %w", err)
	}
	if err := setFn("waterfall", r.nativeWaterfall(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.waterfall 失败: %w", err)
	}
	if err := setFn("tool", r.nativeTool(raw)); err != nil {
		return fmt.Errorf("jsrt: 注入 ctx.tool 失败: %w", err)
	}
	if r.opts.Host.StoreGet != nil || r.opts.Host.StoreSet != nil || r.opts.Host.StoreDelete != nil || r.opts.Host.StoreList != nil {
		storeObj := realm.NewObject()
		setStoreFn := func(name string, length int, fn moejs.NativeFunc) error {
			return storeObj.SetProp(realm, realm.KeyFromGoString(name), raw.Function(name, length, fn))
		}
		if r.opts.Host.StoreGet != nil {
			if err := setStoreFn("get", 1, r.nativeStoreGet(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 ctx.store.get 失败: %w", err)
			}
		}
		if r.opts.Host.StoreSet != nil {
			if err := setStoreFn("set", 2, r.nativeStoreSet(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 ctx.store.set 失败: %w", err)
			}
		}
		if r.opts.Host.StoreDelete != nil {
			if err := setStoreFn("delete", 1, r.nativeStoreDelete(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 ctx.store.delete 失败: %w", err)
			}
		}
		if r.opts.Host.StoreList != nil {
			if err := setStoreFn("list", 1, r.nativeStoreList(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 ctx.store.list 失败: %w", err)
			}
		}
		if err := ctxObj.SetProp(realm, realm.KeyFromGoString("store"), engine.ObjectValue(storeObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 ctx.store 失败: %w", err)
		}
	}
	for name, val := range r.opts.Globals {
		if err := raw.SetGlobal(name, val); err != nil {
			return fmt.Errorf("jsrt: 注入全局 %s 失败: %w", name, err)
		}
	}
	host := engine.ObjectValue(ctxObj)
	// __adaHost 是注入 moejs 模块可用的宿主能力对象。
	hostObj := realm.NewObject()
	if r.opts.Host.Shell != nil {
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("shell"), raw.Function("shell", 3, r.nativeShell(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.shell 失败: %w", err)
		}
	}
	if r.opts.Host.ShellExec != nil {
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("shellExec"), raw.Function("shellExec", 4, r.nativeShellExec(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.shellExec 失败: %w", err)
		}
	}
	if r.opts.Host.FSRead != nil || r.opts.Host.FSWrite != nil || r.opts.Host.FSEdit != nil || r.opts.Host.FSSearch != nil || r.opts.Host.FSList != nil {
		fsObj := realm.NewObject()
		if r.opts.Host.FSRead != nil {
			if err := fsObj.SetProp(realm, realm.KeyFromGoString("read"), raw.Function("read", 1, r.nativeFSRead(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.fs.read 失败: %w", err)
			}
		}
		if r.opts.Host.FSWrite != nil {
			if err := fsObj.SetProp(realm, realm.KeyFromGoString("write"), raw.Function("write", 3, r.nativeFSWrite(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.fs.write 失败: %w", err)
			}
		}
		if r.opts.Host.FSEdit != nil {
			if err := fsObj.SetProp(realm, realm.KeyFromGoString("edit"), raw.Function("edit", 4, r.nativeFSEdit(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.fs.edit 失败: %w", err)
			}
		}
		if r.opts.Host.FSSearch != nil {
			if err := fsObj.SetProp(realm, realm.KeyFromGoString("search"), raw.Function("search", 3, r.nativeFSSearch(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.fs.search 失败: %w", err)
			}
		}
		if r.opts.Host.FSList != nil {
			if err := fsObj.SetProp(realm, realm.KeyFromGoString("list"), raw.Function("list", 3, r.nativeFSList(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.fs.list 失败: %w", err)
			}
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("fs"), engine.ObjectValue(fsObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.fs 失败: %w", err)
		}
	}
	if r.opts.Host.PlanGet != nil || r.opts.Host.PlanSet != nil || r.opts.Host.PlanUpdate != nil {
		planObj := realm.NewObject()
		if r.opts.Host.PlanGet != nil {
			if err := planObj.SetProp(realm, realm.KeyFromGoString("get"), raw.Function("get", 0, r.nativePlanGet(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.plan.get 失败: %w", err)
			}
		}
		if r.opts.Host.PlanSet != nil {
			if err := planObj.SetProp(realm, realm.KeyFromGoString("set"), raw.Function("set", 1, r.nativePlanSet(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.plan.set 失败: %w", err)
			}
		}
		if r.opts.Host.PlanUpdate != nil {
			if err := planObj.SetProp(realm, realm.KeyFromGoString("update"), raw.Function("update", 2, r.nativePlanUpdate(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.plan.update 失败: %w", err)
			}
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("plan"), engine.ObjectValue(planObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.plan 失败: %w", err)
		}
	}
	if r.opts.Host.SessionCurrent != nil || r.opts.Host.SessionAppend != nil {
		sessionObj := realm.NewObject()
		if r.opts.Host.SessionCurrent != nil {
			if err := sessionObj.SetProp(realm, realm.KeyFromGoString("current"), raw.Function("current", 0, r.nativeSessionCurrent(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.session.current 失败: %w", err)
			}
		}
		if r.opts.Host.SessionAppend != nil {
			if err := sessionObj.SetProp(realm, realm.KeyFromGoString("append"), raw.Function("append", 2, r.nativeSessionAppend(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.session.append 失败: %w", err)
			}
		}
		if r.opts.Host.SessionHistory != nil {
			if err := sessionObj.SetProp(realm, realm.KeyFromGoString("history"), raw.Function("history", 2, r.nativeSessionHistory(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.session.history 失败: %w", err)
			}
		}
		if r.opts.Host.SessionRecent != nil {
			if err := sessionObj.SetProp(realm, realm.KeyFromGoString("recent"), raw.Function("recent", 1, r.nativeSessionRecent(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.session.recent 失败: %w", err)
			}
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("session"), engine.ObjectValue(sessionObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.session 失败: %w", err)
		}
	}
	if r.opts.Host.StoreGet != nil || r.opts.Host.StoreSet != nil || r.opts.Host.StoreDelete != nil || r.opts.Host.StoreList != nil {
		storeObj := realm.NewObject()
		if r.opts.Host.StoreGet != nil {
			if err := storeObj.SetProp(realm, realm.KeyFromGoString("get"), raw.Function("get", 1, r.nativeStoreGet(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.store.get 失败: %w", err)
			}
		}
		if r.opts.Host.StoreSet != nil {
			if err := storeObj.SetProp(realm, realm.KeyFromGoString("set"), raw.Function("set", 2, r.nativeStoreSet(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.store.set 失败: %w", err)
			}
		}
		if r.opts.Host.StoreDelete != nil {
			if err := storeObj.SetProp(realm, realm.KeyFromGoString("delete"), raw.Function("delete", 1, r.nativeStoreDelete(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.store.delete 失败: %w", err)
			}
		}
		if r.opts.Host.StoreList != nil {
			if err := storeObj.SetProp(realm, realm.KeyFromGoString("list"), raw.Function("list", 1, r.nativeStoreList(raw))); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.store.list 失败: %w", err)
			}
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("store"), engine.ObjectValue(storeObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.store 失败: %w", err)
		}
	}
	// 网络能力不做策略限制,但仍然是宿主注入:
	// 脚本拿到的只是 JSON 进出的函数,没有直接 syscall 的路径。
	if r.opts.Host.HTTPDo != nil {
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("httpDo"), raw.Function("httpDo", 1, r.nativeHTTPDo(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.httpDo 失败: %w", err)
		}
	}
	if r.opts.Host.DNSLookup != nil {
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("dnsLookup"), raw.Function("dnsLookup", 3, r.nativeDNSLookup(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.dnsLookup 失败: %w", err)
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("dnsLookupAsync"), raw.Function("dnsLookupAsync", 3, r.nativeDNSLookupAsync(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.dnsLookupAsync 失败: %w", err)
		}
	}
	if r.opts.Host.NetDial != nil || r.opts.Host.TLSDial != nil || r.opts.Host.NetListen != nil || r.opts.Host.TLSListen != nil {
		netObj := realm.NewObject()
		setNetFn := func(name string, length int, fn moejs.NativeFunc) error {
			return netObj.SetProp(realm, realm.KeyFromGoString(name), raw.Function(name, length, fn))
		}
		if err := setNetFn("dial", 2, r.nativeNetDial(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netDial 失败: %w", err)
		}
		if err := setNetFn("dialAsync", 2, r.nativeNetDialAsync(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netDialAsync 失败: %w", err)
		}
		if err := setNetFn("read", 2, r.nativeNetRead(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netRead 失败: %w", err)
		}
		if err := setNetFn("readAsync", 2, r.nativeNetReadAsync(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netReadAsync 失败: %w", err)
		}
		if err := setNetFn("write", 2, r.nativeNetWrite(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netWrite 失败: %w", err)
		}
		if err := setNetFn("close", 1, r.nativeNetClose(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netClose 失败: %w", err)
		}
		if err := setNetFn("addr", 1, r.nativeNetAddr(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.netAddr 失败: %w", err)
		}
		if err := setNetFn("tlsDialAsync", 3, r.nativeTLSDialAsync(raw)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.net.tlsDialAsync 失败: %w", err)
		}
		if r.opts.Host.NetListen != nil {
			if err := setNetFn("listenAsync", 1, r.nativeNetListenAsync(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.net.listenAsync 失败: %w", err)
			}
		}
		if r.opts.Host.NetListen != nil || r.opts.Host.TLSListen != nil {
			if err := setNetFn("acceptAsync", 1, r.nativeNetAcceptAsync(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.net.acceptAsync 失败: %w", err)
			}
			if err := setNetFn("closeListener", 1, r.nativeNetListenerClose(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.net.closeListener 失败: %w", err)
			}
			if err := setNetFn("listenerAddr", 1, r.nativeNetListenerAddr(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.net.listenerAddr 失败: %w", err)
			}
		}
		if r.opts.Host.TLSListen != nil {
			if err := setNetFn("tlsListenAsync", 2, r.nativeTLSListenAsync(raw)); err != nil {
				return fmt.Errorf("jsrt: 注入 __adaHost.net.tlsListenAsync 失败: %w", err)
			}
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("net"), engine.ObjectValue(netObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.net 失败: %w", err)
		}
	}
	// 哈希/HMAC/PBKDF2/随机数与策略无关(不读写文件、不发请求),与 node:path 同级默认安装。
	// node:crypto 模块只把这些能力包成 Node 形状,跨边界仍然是 base64 字符串。
	cryptoObj := realm.NewObject()
	cryptoNatives := []struct {
		name  string
		arity int
		fn    moejs.NativeFunc
	}{
		{"hash", 2, r.nativeCryptoHash(raw)},
		{"hmac", 3, r.nativeCryptoHMAC(raw)},
		{"pbkdf2", 5, r.nativeCryptoPBKDF2(raw)},
		{"pbkdf2Async", 5, r.nativeCryptoPBKDF2Async(raw)},
		{"randomBytes", 1, r.nativeCryptoRandomBytes(raw)},
		{"timingSafeEqual", 2, r.nativeCryptoTimingSafeEqual(raw)},
	}
	for _, item := range cryptoNatives {
		if err := cryptoObj.SetProp(realm, realm.KeyFromGoString(item.name), raw.Function(item.name, item.arity, item.fn)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.crypto.%s 失败: %w", item.name, err)
		}
	}
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("crypto"), engine.ObjectValue(cryptoObj)); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.crypto 失败: %w", err)
	}
	// 压缩与解压同样与策略无关:它不碰文件、不发请求,只是把字节换一种表示。
	// 解压的输出上限在 nativeZlib 里,避免 zip bomb 在 JS heap 里展开。
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("zlib"), raw.Function("zlib", 2, r.nativeZlib(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.zlib 失败: %w", err)
	}
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("zlibStreamNew"), raw.Function("zlibStreamNew", 1, r.nativeZlibStreamNew(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.zlibStreamNew 失败: %w", err)
	}
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("zlibStreamWrite"), raw.Function("zlibStreamWrite", 2, r.nativeZlibStreamWrite(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.zlibStreamWrite 失败: %w", err)
	}
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("zlibStreamEnd"), raw.Function("zlibStreamEnd", 1, r.nativeZlibStreamEnd(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.zlibStreamEnd 失败: %w", err)
	}
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("zlibStreamClose"), raw.Function("zlibStreamClose", 1, r.nativeZlibStreamClose(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.zlibStreamClose 失败: %w", err)
	}
	// 单调时钟:node:perf_hooks 的 now() 用它,而不是会跳变的 Date.now()。
	if err := hostObj.SetProp(realm, realm.KeyFromGoString("monoMs"), raw.Function("monoMs", 0, r.nativeMonoMs(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost.monoMs 失败: %w", err)
	}
	// secrets 只按名读取;来源、白名单与授权都在 seam/secrets 的 provider 与 policy。
	if r.opts.Host.Secrets != nil {
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("secretsGet"), raw.Function("secretsGet", 1, r.nativeSecretsGet(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.secretsGet 失败: %w", err)
		}
	}
	// node:fs 的补齐层(mkdtemp/symlink/link/chmod/chown/truncate/utimes):
	// 只有装了 node:fs(有工作区根)时才注入,否则这几个名字在 JS 侧也不会被求值。
	if r.nodeRootOf() != nil {
		fsxObj := realm.NewObject()
		if err := fsxObj.SetProp(realm, realm.KeyFromGoString("call"), raw.Function("call", 2, r.nativeFSXCall(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.fsx.call 失败: %w", err)
		}
		if err := fsxObj.SetProp(realm, realm.KeyFromGoString("callAsync"), raw.Function("callAsync", 2, r.nativeFSXCallAsync(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.fsx.callAsync 失败: %w", err)
		}
		if err := fsxObj.SetProp(realm, realm.KeyFromGoString("watchCheck"), raw.Function("watchCheck", 1, r.nativeFSXWatchCheck(raw))); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.fsx.watchCheck 失败: %w", err)
		}
		if err := hostObj.SetProp(realm, realm.KeyFromGoString("fsx"), engine.ObjectValue(fsxObj)); err != nil {
			return fmt.Errorf("jsrt: 注入 __adaHost.fsx 失败: %w", err)
		}
	}
	if err := realm.Global.SetProp(realm, realm.KeyFromGoString("__adaHost"), engine.ObjectValue(hostObj)); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHost 失败: %w", err)
	}
	// __adaCtx 只是调试入口;插件作者应该用 apply(ctx, config) 的第一个参数。
	if err := realm.Global.SetProp(realm, realm.KeyFromGoString("__adaCtx"), host); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaCtx 失败: %w", err)
	}
	if err := realm.Global.SetProp(realm, realm.KeyFromGoString("__adaLog"), raw.Function("__adaLog", 2, r.nativeLog(raw))); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaLog 失败: %w", err)
	}
	r.host = host

	mods := make(map[string]*moejs.Module, len(r.art.Files))
	for name, code := range r.art.Files {
		m, err := moejs.Compile(name, code)
		if err != nil {
			return fmt.Errorf("jsrt: 编译 %s 失败: %w", name, err)
		}
		mods[name] = m
	}
	entry, ok := mods[r.art.Entry]
	if !ok {
		return fmt.Errorf("jsrt: 入口 %q 不存在", r.art.Entry)
	}
	// 引导模块提供两个能力:await 入口的返回值、结算时清掉遗留定时器。
	// 它成为 Link 的根,入口模块经 ada:entry 被它引用(见 loop.go)。
	boot, bootTimers, fsxMod, nodeFSMod, nodeFSAsyncMod, err := compileBoot()
	if err != nil {
		return err
	}
	linked, err := moejs.Link(boot, func(ref moejs.Referrer, spec string) (*moejs.Module, error) {
		// 虚拟说明符只在这里解析:它们不进 Artifact,也不占用插件的导入空间。
		switch spec {
		case entrySpecifier:
			return entry, nil
		case timersSpecifier:
			return bootTimers, nil
		case fsxSpecifier:
			return fsxMod, nil
		// node:fs 走我们的包装:在 moejs 的实现之上补齐缺失的元数据类 API
		// (见 nodefsx.go)。包装模块内部用下面两个别名取回上游能力。
		case "node:fs", "fs":
			return nodeFSMod, nil
		case "node:fs/promises", "fs/promises":
			return nodeFSAsyncMod, nil
		case baseFSSpecifier:
			return mojsnode.Resolver(nil)(ref, "node:fs")
		case basePromisesSpecifier:
			return mojsnode.Resolver(nil)(ref, "node:fs/promises")
		}
		if m, ok := r.opts.Modules[spec]; ok {
			return m, nil
		}
		// Node 内置模块优先:node:path / node:os 是宿主源码,node:fs / node:buffer
		// 由 moejs 的 node 包提供;未知 node:* 交给它给出明确的链接期错误。
		if src, ok := nodemods.Source(spec); ok {
			return compileNodeModule(spec, src)
		}
		if strings.HasPrefix(spec, "node:") || isNodeAlias(spec) {
			return mojsnode.Resolver(nil)(ref, spec)
		}
		name, ok := r.art.Resolve(ref.Name(), spec)
		if !ok {
			return nil, fmt.Errorf("jsrt: 无法解析 %q(来自 %s);只允许相对导入或宿主注入模块", spec, ref.Name())
		}
		m, ok := mods[name]
		if !ok {
			return nil, fmt.Errorf("jsrt: 解析到不存在的模块 %q", name)
		}
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("jsrt: 链接失败: %w", err)
	}
	// 常驻模式不清调用边界遗留的定时器；boot.js 依据这个标记决定。
	if err := raw.SetGlobal("__adaResident", r.mode == LoopResident); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaResident 失败: %w", err)
	}
	// 加载在循环里跑:模块顶层若调用 setTimeout(以及在支持的形态下 await),
	// 必须在有循环的前提下求值,否则会出现「模块看到的是半装好的运行时」。
	preludeBytes, readErr := bootFS.ReadFile("prelude.js")
	if readErr != nil {
		return fmt.Errorf("jsrt: 读取 prelude 失败: %w", readErr)
	}
	preludeScript, compileErr := moejs.CompileScript("ada:prelude", string(preludeBytes))
	if compileErr != nil {
		return fmt.Errorf("jsrt: 编译 prelude 失败: %w", compileErr)
	}
	readyCh := make(chan struct{})
	var readyOnce sync.Once
	readyRealm := raw.Realm()
	// FS 配额供 JS 包装层（核心 readFile/writeFile/readdir）使用。
	if err := raw.SetGlobal("__adaFSLimits", map[string]any{
		"maxFileBytes":  r.fsPerm.MaxFileBytes,
		"maxDirEntries": r.fsPerm.MaxDirEntries,
		"maxOpenFiles":  r.fsPerm.MaxOpenFiles,
	}); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaFSLimits 失败: %w", err)
	}
	// Env 只暴露白名单键；写权限由 __adaEnvSet 控制（Proxy 在 process.js 里）。
	envMap := make(map[string]string)
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if ok && r.perms.Env.check(name) {
			envMap[name] = value
		}
	}
	if err := raw.SetGlobal("__adaEnv", envMap); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaEnv 失败: %w", err)
	}
	if err := raw.SetGlobal("__adaEnvSet", r.perms.Env.Set); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaEnvSet 失败: %w", err)
	}
	maxReq, maxResp := int64(0), int64(0)
	if r.netPolicy != nil {
		maxReq = r.netPolicy.perm.MaxRequestBytes
		maxResp = r.netPolicy.perm.MaxResponseBytes
	}
	if err := raw.SetGlobal("__adaHTTPLimits", map[string]any{
		"maxRequestBytes":  maxReq,
		"maxResponseBytes": maxResp,
	}); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaHTTPLimits 失败: %w", err)
	}
	exitVal := raw.Function("__adaExit", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		code := 0
		if len(args) > 0 {
			if v, err := raw.ToGo(args[0]); err == nil {
				switch n := v.(type) {
				case int64:
					code = int(n)
				case float64:
					code = int(n)
				case int:
					code = n
				}
			}
		}
		if r.exitPolicy != ExitRequest {
			return engine.Undefined(), fmt.Errorf("process.exit 被禁用")
		}
		if r.mode != LoopResident {
			return engine.Undefined(), fmt.Errorf("process.exit 需要 LoopResident")
		}
		r.requestExit(code)
		return engine.Undefined(), nil
	})
	if err := readyRealm.Global.SetProp(readyRealm, readyRealm.KeyFromGoString("__adaExit"), exitVal); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaExit 失败: %w", err)
	}
	readyVal := raw.Function("__adaReady", 0, func(_ *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		readyOnce.Do(func() { close(readyCh) })
		return engine.Undefined(), nil
	})
	if err := readyRealm.Global.SetProp(readyRealm, readyRealm.KeyFromGoString("__adaReady"), readyVal); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaReady 失败: %w", err)
	}
	loadOnLoop := func(rt *moejs.Runtime) error {
		if _, err := rt.RunScript(preludeScript); err != nil {
			return fmt.Errorf("jsrt: 执行 prelude 失败: %w", err)
		}
		err := rt.Load(linked)
		if errors.Is(err, moejs.ErrModulePending) {
			// 顶层 await：求值继续，boot 模块体会在依赖结算后调用 __adaReady。
			return nil
		}
		return err
	}

	if r.mode == LoopResident {
		// 常驻循环需要一个持续 hold：否则 Start 在启动任务跑完、暂无定时器时
		// 就会自然退出，后续 RunOnLoop 投递的任务不会被执行。
		hold := loop.Hold()
		r.mu.Lock()
		r.loopHold = hold
		r.resident = true
		r.mu.Unlock()
		if err := loop.Start(); err != nil {
			return fmt.Errorf("jsrt: 启动事件循环失败: %w", err)
		}
		done := make(chan error, 1)
		if !r.postToLoop(func(rt *moejs.Runtime) {
			r.callCtx = ctx
			defer func() { r.callCtx = nil }()
			done <- loadOnLoop(rt)
		}) {
			return ErrClosed
		}
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("jsrt: 加载失败: %w", err)
			}
		case <-ctx.Done():
			return fmt.Errorf("jsrt: 装配被取消: %w", ctx.Err())
		}
		timeout := r.startupTimeout()
		select {
		case <-readyCh:
		case <-time.After(timeout):
			return fmt.Errorf("%w: 入口在 %s 内未完成", ErrStartupTimeout, timeout)
		case <-ctx.Done():
			return fmt.Errorf("jsrt: 装配被取消: %w", ctx.Err())
		}
	} else {
		releaseHold := loop.Hold()
		var releaseOnce sync.Once
		release := func() {
			releaseOnce.Do(func() { releaseHold(nil) })
		}
		loadDone := make(chan struct{})
		stopWatch := make(chan struct{})
		go func() {
			select {
			case <-readyCh:
				release()
			case <-loadDone:
			}
		}()
		go func() {
			select {
			case <-readyCh:
			case <-time.After(r.startupTimeout()):
				loop.Interrupt(ErrStartupTimeout)
			case <-ctx.Done():
				loop.Interrupt(ctx.Err())
			case <-stopWatch:
			}
		}()
		runErr := loop.Run(func(rt *moejs.Runtime) error {
			if err := loadOnLoop(rt); err != nil {
				release()
				return err
			}
			return nil
		})
		close(stopWatch)
		close(loadDone)
		if runErr != nil {
			release()
			return fmt.Errorf("jsrt: 加载失败: %w", runErr)
		}
		select {
		case <-readyCh:
		default:
			release()
			if ctx.Err() != nil {
				return fmt.Errorf("jsrt: 装配被取消: %w", ctx.Err())
			}
			return fmt.Errorf("%w: 入口在 %s 内未完成", ErrStartupTimeout, r.startupTimeout())
		}
		release()
	}

	hooks := make(map[string]moejs.Hook, 8)
	for _, name := range linked.Exports() {
		if h, err := linked.Hook(name); err == nil {
			hooks[name] = h
		}
	}
	r.mu.Lock()
	r.raw = raw
	r.hooks = hooks
	r.mu.Unlock()
	return nil
}

// nodeModuleCache 缓存宿主自建 Node 模块;node:path 与 path 必须解析到同一实例。
var nodeModuleCache sync.Map // string → *moejs.Module

func compileNodeModule(spec, src string) (*moejs.Module, error) {
	key := strings.TrimPrefix(spec, "node:")
	if v, ok := nodeModuleCache.Load(key); ok {
		return v.(*moejs.Module), nil
	}
	m, err := moejs.Compile("node:"+key, src)
	if err != nil {
		return nil, err
	}
	nodeModuleCache.Store(key, m)
	return m, nil
}

// isNodeAlias 是 moejs node 包支持的裸模块名。
func isNodeAlias(spec string) bool {
	switch spec {
	case "fs", "fs/promises", "buffer":
		return true
	}
	return false
}

// installNode 安装 Node 内置模块。
//
// node:path / node:os / node:buffer 始终可用;node:fs 只有给出 FS 根时才会安装。
// 写权限由调用方(桥接层)按策略决定,jsrt 不自行放宽。
//
// 用 InstallLoop 而不是 Install:node:fs 的 promise/回调形状需要一个真实的循环
// 来做 Hold/Post,否则「异步」只是形状,结算时间无法保证。
func (r *Runtime) installNode(raw *moejs.Runtime, loop *eventloop.Loop) error {
	info := map[string]any{
		"platform": runtime.GOOS,
		"arch":     runtime.GOARCH,
		"type":     runtime.GOOS,
		"release":  "",
		"version":  "",
		"cpus":     runtime.NumCPU(),
		"totalmem": 0,
		"freemem":  0,
		"eol":      "\n",
	}
	if runtime.GOOS == "windows" {
		info["eol"] = "\r\n"
	}
	if err := raw.SetGlobal("__adaOSInfo", info); err != nil {
		return fmt.Errorf("jsrt: 注入 __adaOSInfo 失败: %w", err)
	}

	opts := mojsnode.Options{Buffer: true}
	if n := r.opts.Node; n != nil {
		opts.Buffer = n.Buffer
	}
	if r.fsPerm.Enabled {
		mount := r.fsPerm.Mount
		if mount == "" {
			mount = "/workspace"
		}
		cwd := r.fsPerm.Cwd
		if cwd == "" {
			cwd = mount
		}
		workers := 0
		if n := r.opts.Node; n != nil && n.FS != nil {
			workers = n.FS.Workers
		}
		if r.limits.FSWorkers > 0 {
			workers = r.limits.FSWorkers
		}
		fsOpts := &mojsnode.FSOptions{
			Root: r.fsPerm.Root, Mount: mount, Cwd: cwd,
			Write: r.fsPerm.Access == FSReadWrite, Workers: workers,
		}
		// 只有新的 Permissions.FS 才启用路径级 Allow；旧 Node.FS 保持原有行为。
		if r.fsEnforce {
			fsOpts.Allow = r.nodeFSAllow
		}
		opts.FS = fsOpts
		// os.tmpdir() 返回 /workspace/.ada-tmp;让它在 node:fs 根内真实存在。
		_ = os.MkdirAll(filepath.Join(r.fsPerm.Root, ".ada-tmp"), 0o755)
	}
	api, err := mojsnode.InstallLoop(loop, opts)
	if err != nil {
		return fmt.Errorf("jsrt: 安装 Node 内置模块失败: %w", err)
	}
	r.mu.Lock()
	r.nodeAPI = api
	r.mu.Unlock()
	// 补齐层(node:fs 的 mkdtemp/symlink/chmod/...)需要一个根句柄;
	// 没有工作区根时也不会有 node:fs,所以这里返回 nil 即可。
	return r.openNodeRoot()
}

// nodeFSAllow 是 moejs FSOptions.Allow 的实现：把路径级权限接到核心 fs。
//
// moejs 传入的 path 是已解析符号链接的虚拟绝对路径，op 是 "read"/"write"。
// 返回值会被 moejs 映射成 Node errno：EROFS 表示只读，其余拒绝是 EACCES。
func (r *Runtime) nodeFSAllow(op, p string) error {
	if r.fsPolicy == nil {
		return nil
	}
	err := r.fsPolicy.check(op, p)
	if err == nil {
		return nil
	}
	var pe *PermissionError
	if errors.As(err, &pe) {
		r.emitEvent(Event{
			Kind: "permission", Err: err, Scope: "fs", Op: op,
			Resource: p, Allowed: false,
		})
		if pe.Code == "EROFS" {
			return syscall.EROFS
		}
		return syscall.EACCES
	}
	return err
}

// nativeMonoMs 实现 __adaHost.monoMs():相对运行时启动的单调毫秒数。
//
// 用单调时钟而不是墙钟:性能统计不能被系统时间调整打断;精度到微秒,
// 对插件里的耗时测量足够,也不暴露宿主的绝对时间。
func (r *Runtime) nativeMonoMs(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		ms := float64(time.Since(r.clockStart).Microseconds()) / 1000
		return raw.FromGo(ms)
	}
}

// nativeLog 把 JS 的 ctx.log(...args) 转成宿主日志。
func (r *Runtime) nativeLog(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Logf == nil || len(args) == 0 {
			return engine.Undefined(), nil
		}
		parts := make([]any, 0, len(args))
		for _, a := range args {
			v, err := raw.ToGo(a)
			if err != nil {
				v = "<无法转换>"
			}
			parts = append(parts, v)
		}
		if len(parts) == 1 {
			r.opts.Logf("%v", parts[0])
		} else {
			r.opts.Logf("%v", parts)
		}
		return engine.Undefined(), nil
	}
}

// nativeProvide 实现 ctx.provide(name, value):值经 JSON 转成 Go 值后交给桥接层。
func (r *Runtime) nativeProvide(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Provide == nil {
			return engine.Undefined(), fmt.Errorf("ctx.provide 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("ctx.provide(name, value) 需要两个参数")
		}
		rawName, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.provide 的服务名无法转换: %w", err)
		}
		name, ok := rawName.(string)
		if !ok || name == "" {
			return engine.Undefined(), fmt.Errorf("ctx.provide 的服务名必须是非空字符串")
		}
		svc, err := r.describeService(raw, name, args[1])
		if err != nil {
			return engine.Undefined(), err
		}
		if err := r.opts.Host.Provide(name, svc); err != nil {
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// nativeShell 实现宿主注入模块用的 __adaHost.shell(program, args, dir)。
//
// 返回值是 stdout 字符串;非零退出与 stderr 由宿主实现合并后返回。
func (r *Runtime) nativeShell(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Shell == nil {
			return engine.Undefined(), fmt.Errorf("shell 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("shell(program, args, dir) 至少需要 program")
		}
		program, err := toString(raw, args[0])
		if err != nil || program == "" {
			return engine.Undefined(), fmt.Errorf("shell program 必须是非空字符串")
		}
		if r.opts.Permissions.Child.Enabled {
			if cerr := r.opts.Permissions.Child.check(program); cerr != nil {
				r.emitPermission("child", "exec", program, cerr)
				return engine.Undefined(), cerr
			}
		}
		var argv []string
		if len(args) > 1 {
			v, convErr := raw.ToGo(args[1])
			if convErr != nil {
				return engine.Undefined(), fmt.Errorf("shell args 无法转换: %w", convErr)
			}
			list, ok := v.([]any)
			if !ok {
				return engine.Undefined(), fmt.Errorf("shell args 必须是数组")
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok {
					return engine.Undefined(), fmt.Errorf("shell args 只能包含字符串")
				}
				argv = append(argv, s)
			}
		}
		dir := "/workspace"
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil && s != "" {
				dir = s
			}
		}
		// 用当前 worker 任务的 ctx:工具调用链路的取消、超时与 workspace 绑定都随它传播。
		out, err := r.opts.Host.Shell(r.currentContext(), program, argv, dir)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeShellExec 实现 __adaHost.shellExec(program, args, dir, options)。
//
// 它只做参数解码与转发;结构化结果由宿主构造,避免 jsrt 依赖 seam/shell 类型。
func (r *Runtime) nativeShellExec(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.ShellExec == nil {
			return engine.Undefined(), fmt.Errorf("shell.exec 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("shell.exec(program, args, dir, options) 至少需要 program")
		}
		program, err := toString(raw, args[0])
		if err != nil || program == "" {
			return engine.Undefined(), fmt.Errorf("shell.exec program 必须是非空字符串")
		}
		if r.opts.Permissions.Child.Enabled {
			if cerr := r.opts.Permissions.Child.check(program); cerr != nil {
				r.emitPermission("child", "exec", program, cerr)
				return engine.Undefined(), cerr
			}
		}
		var argv []string
		if len(args) > 1 {
			v, convErr := raw.ToGo(args[1])
			if convErr != nil {
				return engine.Undefined(), fmt.Errorf("shell.exec args 无法转换: %w", convErr)
			}
			list, ok := v.([]any)
			if !ok {
				return engine.Undefined(), fmt.Errorf("shell.exec args 必须是数组")
			}
			for _, item := range list {
				s, ok := item.(string)
				if !ok {
					return engine.Undefined(), fmt.Errorf("shell.exec args 只能包含字符串")
				}
				argv = append(argv, s)
			}
		}
		dir := "/workspace"
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil && s != "" {
				dir = s
			}
		}
		options := "{}"
		if len(args) > 3 {
			if s, convErr := toString(raw, args[3]); convErr == nil && s != "" {
				options = s
			}
		}
		out, err := r.opts.Host.ShellExec(r.currentContext(), program, argv, dir, options)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeFSRead 实现 __adaHost.fs.read(path)。
func (r *Runtime) nativeFSRead(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.FSRead == nil {
			return engine.Undefined(), fmt.Errorf("fs.read 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("fs.read(path) 需要路径")
		}
		p, err := toString(raw, args[0])
		if err != nil || p == "" {
			return engine.Undefined(), fmt.Errorf("fs.read 路径必须是非空字符串")
		}
		out, err := r.opts.Host.FSRead(r.currentContext(), p)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeFSWrite 实现 __adaHost.fs.write(path, content, createDirs)。
func (r *Runtime) nativeFSWrite(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.FSWrite == nil {
			return engine.Undefined(), fmt.Errorf("fs.write 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("fs.write(path, content, createDirs) 需要路径与内容")
		}
		p, err := toString(raw, args[0])
		if err != nil || p == "" {
			return engine.Undefined(), fmt.Errorf("fs.write 路径必须是非空字符串")
		}
		content, err := toString(raw, args[1])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("fs.write 内容必须是字符串")
		}
		createDirs := true
		if len(args) > 2 {
			if v, convErr := raw.ToGo(args[2]); convErr == nil {
				if b, ok := v.(bool); ok {
					createDirs = b
				}
			}
		}
		if err := r.opts.Host.FSWrite(r.currentContext(), p, content, createDirs); err != nil {
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// nativeFSEdit 实现 __adaHost.fs.edit(path, old, new, all)。
func (r *Runtime) nativeFSEdit(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.FSEdit == nil {
			return engine.Undefined(), fmt.Errorf("fs.edit 未启用")
		}
		if len(args) < 3 {
			return engine.Undefined(), fmt.Errorf("fs.edit(path, old, new, all) 至少需要三个参数")
		}
		p, err := toString(raw, args[0])
		if err != nil || p == "" {
			return engine.Undefined(), fmt.Errorf("fs.edit 路径非法")
		}
		old, err := toString(raw, args[1])
		if err != nil || old == "" {
			return engine.Undefined(), fmt.Errorf("fs.edit old 不能为空")
		}
		newText, err := toString(raw, args[2])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("fs.edit new 必须是字符串")
		}
		all := false
		if len(args) > 3 {
			if v, convErr := raw.ToGo(args[3]); convErr == nil {
				if b, ok := v.(bool); ok {
					all = b
				}
			}
		}
		n, err := r.opts.Host.FSEdit(r.currentContext(), p, old, newText, all)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(n)
	}
}

// nativeFSSearch 实现 __adaHost.fs.search(path, query, limit):返回 JSON 字符串。
func (r *Runtime) nativeFSSearch(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.FSSearch == nil {
			return engine.Undefined(), fmt.Errorf("fs.search 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("fs.search(query) 需要 query")
		}
		query, err := toString(raw, args[0])
		if err != nil || query == "" {
			return engine.Undefined(), fmt.Errorf("fs.search query 不能为空")
		}
		dir := "/workspace"
		if len(args) > 1 {
			if s, convErr := toString(raw, args[1]); convErr == nil && s != "" {
				dir = s
			}
		}
		limit := 0
		if len(args) > 2 {
			if v, convErr := raw.ToGo(args[2]); convErr == nil {
				if n, ok := toInt64(v); ok {
					limit = int(n)
				}
			}
		}
		out, err := r.opts.Host.FSSearch(r.currentContext(), dir, query, limit)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeFSList 实现 __adaHost.fs.list(path, recursive, limit):返回 JSON 字符串。
func (r *Runtime) nativeFSList(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.FSList == nil {
			return engine.Undefined(), fmt.Errorf("fs.list 未启用")
		}
		dir := "/workspace"
		if len(args) > 0 {
			if s, convErr := toString(raw, args[0]); convErr == nil && s != "" {
				dir = s
			}
		}
		recursive := false
		if len(args) > 1 {
			if v, convErr := raw.ToGo(args[1]); convErr == nil {
				if b, ok := v.(bool); ok {
					recursive = b
				}
			}
		}
		limit := 0
		if len(args) > 2 {
			if v, convErr := raw.ToGo(args[2]); convErr == nil {
				if n, ok := toInt64(v); ok {
					limit = int(n)
				}
			}
		}
		out, err := r.opts.Host.FSList(r.currentContext(), dir, recursive, limit)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	}
	return 0, false
}

// nativeTool 实现 ctx.tool(spec, handler):handler 是 JS 函数,工具调用时回调。
// nativePlanGet 实现 __adaHost.plan.get():返回计划 JSON,"null" 表示没有计划。
func (r *Runtime) nativePlanGet(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		if r.opts.Host.PlanGet == nil {
			return engine.Undefined(), fmt.Errorf("plan.get 未启用")
		}
		out, err := r.opts.Host.PlanGet(r.currentContext())
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativePlanSet 实现 __adaHost.plan.set(planJSON)。
func (r *Runtime) nativePlanSet(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.PlanSet == nil {
			return engine.Undefined(), fmt.Errorf("plan.set 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("plan.set(planJSON) 需要参数")
		}
		body, err := toString(raw, args[0])
		if err != nil || body == "" {
			return engine.Undefined(), fmt.Errorf("plan.set 参数必须是非空 JSON 字符串")
		}
		out, err := r.opts.Host.PlanSet(r.currentContext(), body)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativePlanUpdate 实现 __adaHost.plan.update(index, status)。
func (r *Runtime) nativePlanUpdate(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.PlanUpdate == nil {
			return engine.Undefined(), fmt.Errorf("plan.update 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("plan.update(index, status) 需要两个参数")
		}
		got, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("plan.update 的 index 无法转换: %w", err)
		}
		index, ok := toInt64(got)
		if !ok || index < 1 {
			return engine.Undefined(), fmt.Errorf("plan.update 的 index 必须是从 1 开始的整数")
		}
		status, err := toString(raw, args[1])
		if err != nil || status == "" {
			return engine.Undefined(), fmt.Errorf("plan.update 的 status 必须是非空字符串")
		}
		out, err := r.opts.Host.PlanUpdate(r.currentContext(), int(index), status)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeSessionCurrent 实现 __adaHost.session.current():返回会话 JSON 或 "null"。
func (r *Runtime) nativeSessionCurrent(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, _ []engine.Value) (engine.Value, error) {
		if r.opts.Host.SessionCurrent == nil {
			return engine.Undefined(), fmt.Errorf("session.current 未启用")
		}
		out, err := r.opts.Host.SessionCurrent(r.currentContext())
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeSessionAppend 实现 __adaHost.session.append(kind, dataJSON)。
func (r *Runtime) nativeSessionAppend(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.SessionAppend == nil {
			return engine.Undefined(), fmt.Errorf("session.append 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("session.append(kind, data) 需要两个参数")
		}
		kind, err := toString(raw, args[0])
		if err != nil || kind == "" {
			return engine.Undefined(), fmt.Errorf("session.append 的 kind 必须是非空字符串")
		}
		data, err := toString(raw, args[1])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("session.append 的 data 必须是 JSON 字符串")
		}
		seq, err := r.opts.Host.SessionAppend(r.currentContext(), kind, data)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(int64(seq))
	}
}

// nativeSessionHistory 实现 __adaHost.session.history(from, limit)。
func (r *Runtime) nativeSessionHistory(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.SessionHistory == nil {
			return engine.Undefined(), fmt.Errorf("session.history 未启用")
		}
		var from int64
		if len(args) > 0 {
			got, err := raw.ToGo(args[0])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("session.history 的 from 无法转换: %w", err)
			}
			n, ok := toInt64(got)
			if !ok || n < 0 {
				return engine.Undefined(), fmt.Errorf("session.history 的 from 必须是非负整数")
			}
			from = n
		}
		limit := 0
		if len(args) > 1 {
			got, err := raw.ToGo(args[1])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("session.history 的 limit 无法转换: %w", err)
			}
			n, ok := toInt64(got)
			if !ok || n < 0 {
				return engine.Undefined(), fmt.Errorf("session.history 的 limit 必须是非负整数")
			}
			limit = int(n)
		}
		out, err := r.opts.Host.SessionHistory(r.currentContext(), uint64(from), limit)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeSessionRecent 实现 __adaHost.session.recent(limit)。
func (r *Runtime) nativeSessionRecent(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.SessionRecent == nil {
			return engine.Undefined(), fmt.Errorf("session.recent 未启用")
		}
		limit := 0
		if len(args) > 0 {
			got, err := raw.ToGo(args[0])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("session.recent 的 limit 无法转换: %w", err)
			}
			n, ok := toInt64(got)
			if !ok || n < 0 {
				return engine.Undefined(), fmt.Errorf("session.recent 的 limit 必须是非负整数")
			}
			limit = int(n)
		}
		out, err := r.opts.Host.SessionRecent(r.currentContext(), limit)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeStoreGet 实现 ctx.store.get / __adaHost.store.get(key)。
func (r *Runtime) nativeStoreGet(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.StoreGet == nil {
			return engine.Undefined(), fmt.Errorf("store.get 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("store.get(key) 需要键")
		}
		key, err := toString(raw, args[0])
		if err != nil || key == "" {
			return engine.Undefined(), fmt.Errorf("store.get 的 key 必须是非空字符串")
		}
		body, err := r.opts.Host.StoreGet(r.currentContext(), key)
		if err != nil {
			return engine.Undefined(), err
		}
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			return engine.Undefined(), fmt.Errorf("store.get 的返回值不是 JSON: %w", err)
		}
		return raw.FromGo(v)
	}
}

// nativeStoreSet 实现 ctx.store.set / __adaHost.store.set(key, value)。
func (r *Runtime) nativeStoreSet(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.StoreSet == nil {
			return engine.Undefined(), fmt.Errorf("store.set 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("store.set(key, value) 需要两个参数")
		}
		key, err := toString(raw, args[0])
		if err != nil || key == "" {
			return engine.Undefined(), fmt.Errorf("store.set 的 key 必须是非空字符串")
		}
		value, err := raw.ToGo(args[1])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("store.set 的 value 无法转换: %w", err)
		}
		body, err := json.Marshal(value)
		if err != nil {
			return engine.Undefined(), fmt.Errorf("store.set 的 value 无法编码: %w", err)
		}
		if err := r.opts.Host.StoreSet(r.currentContext(), key, string(body)); err != nil {
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// nativeStoreDelete 实现 ctx.store.delete / __adaHost.store.delete(key)。
func (r *Runtime) nativeStoreDelete(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.StoreDelete == nil {
			return engine.Undefined(), fmt.Errorf("store.delete 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("store.delete(key) 需要键")
		}
		key, err := toString(raw, args[0])
		if err != nil || key == "" {
			return engine.Undefined(), fmt.Errorf("store.delete 的 key 必须是非空字符串")
		}
		if err := r.opts.Host.StoreDelete(r.currentContext(), key); err != nil {
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// nativeStoreList 实现 ctx.store.list / __adaHost.store.list(prefix)。
func (r *Runtime) nativeStoreList(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.StoreList == nil {
			return engine.Undefined(), fmt.Errorf("store.list 未启用")
		}
		prefix := ""
		if len(args) > 0 {
			if p, err := toString(raw, args[0]); err == nil {
				prefix = p
			}
		}
		body, err := r.opts.Host.StoreList(r.currentContext(), prefix)
		if err != nil {
			return engine.Undefined(), err
		}
		var keys []string
		if err := json.Unmarshal([]byte(body), &keys); err != nil {
			return engine.Undefined(), fmt.Errorf("store.list 的返回值不是 JSON 数组: %w", err)
		}
		return raw.FromGo(keys)
	}
}

func (r *Runtime) nativeTool(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Tool == nil {
			return engine.Undefined(), fmt.Errorf("ctx.tool 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("ctx.tool(spec, handler) 需要两个参数")
		}
		if !engine.IsCallable(args[1]) {
			return engine.Undefined(), fmt.Errorf("ctx.tool 的第二个参数必须是函数")
		}
		spec, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.tool 的 spec 无法转换: %w", err)
		}
		obj, ok := spec.(map[string]any)
		if !ok {
			return engine.Undefined(), fmt.Errorf("ctx.tool 的 spec 必须是对象")
		}
		name, _ := obj["name"].(string)
		summary, _ := obj["summary"].(string)
		input, _ := obj["input"].(map[string]any)
		readOnly, _ := obj["readOnly"].(bool)
		if name == "" {
			return engine.Undefined(), fmt.Errorf("ctx.tool 需要非空 name")
		}
		var timeout time.Duration
		if v, ok := obj["timeoutMs"]; ok {
			if n, ok := toInt64(v); ok && n > 0 {
				timeout = time.Duration(n) * time.Millisecond
			}
		}
		r.nextID++
		id := r.nextID
		r.handlers[id] = handlerEntry{fn: args[1], this: engine.Undefined()}
		invoke := func(ctx context.Context, a any) (any, error) {
			return r.invokeHandler(ctx, id, a)
		}
		if err := r.opts.Host.Tool(name, summary, input, readOnly, timeout, invoke); err != nil {
			delete(r.handlers, id)
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

func toString(raw *moejs.Runtime, v engine.Value) (string, error) {
	got, err := raw.ToGo(v)
	if err != nil {
		return "", err
	}
	s, ok := got.(string)
	if !ok {
		return "", fmt.Errorf("不是字符串")
	}
	return s, nil
}

// nativeEmit 实现 ctx.emit(event, payload):桥接层负责把它排到宿主协程上派发。
func (r *Runtime) nativeEmit(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Emit == nil {
			return engine.Undefined(), fmt.Errorf("ctx.emit 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("ctx.emit(event, payload) 至少需要一个参数")
		}
		rawEvent, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.emit 的事件名无法转换: %w", err)
		}
		event, ok := rawEvent.(string)
		if !ok || event == "" {
			return engine.Undefined(), fmt.Errorf("ctx.emit 的事件名必须是非空字符串")
		}
		var payload any
		if len(args) > 1 {
			v, err := raw.ToGo(args[1])
			if err != nil {
				return engine.Undefined(), fmt.Errorf("ctx.emit 的载荷无法转换: %w", err)
			}
			payload = v
		}
		// 宿主可能同步派发事件;若监听器又调回本运行时,靠 inHost 快速失败而不是死锁。
		r.enterHost()
		err = r.opts.Host.Emit(event, payload)
		r.leaveHost()
		if err != nil {
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// CallResult 是 Call 的返回值容器,避免与调用方共享变量。
type callResult struct {
	value any
	err   error
}

// Call 调用入口模块导出的函数。args 逐个经 JSON 编解码,跨边界不传引用。
//
// ctx 取消时会 Interrupt 正在运行的 JS;若 JS 在 interruptGrace 内仍未收尾,
// 运行时被标记为损坏,后续调用直接失败——把可能半执行的运行时继续复用更危险。
func (r *Runtime) Call(ctx context.Context, export string, args ...any) (any, error) {
	return r.call(ctx, export, false, args...)
}

// CallWithHost 在参数最前面插入 ctx 对象。
// 插件入口约定写成 apply(ctx, config)。
func (r *Runtime) CallWithHost(ctx context.Context, export string, args ...any) (any, error) {
	return r.call(ctx, export, true, args...)
}

func (r *Runtime) call(ctx context.Context, export string, withHost bool, args ...any) (any, error) {
	payload := make([][]byte, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("jsrt: 参数 %d 无法序列化: %w", i, err)
		}
		payload[i] = b
	}
	if r.mode == LoopResident {
		return r.callResident(ctx, export, payload, withHost)
	}

	res := make(chan callResult, 1)
	job := func() { res <- r.callOnWorker(export, payload, withHost) }
	if err := r.enqueue(ctx, job); err != nil {
		return nil, err
	}

	select {
	case got := <-res:
		return got.value, got.err
	case <-ctx.Done():
		r.interrupt(ctx.Err())
		select {
		case got := <-res:
			// 拿到被中断调用的结局后清掉中断标志,保证运行时还能继续用。
			r.clearInterrupt()
			if got.err != nil {
				return nil, got.err
			}
			return got.value, fmt.Errorf("jsrt: 调用 %q 已取消: %w", export, ctx.Err())
		case <-time.After(interruptGrace):
			r.mu.Lock()
			r.broken = true
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: 调用 %q 在 %s 内未响应取消", ErrBroken, export, interruptGrace)
		}
	}
}

// callOnWorker 只在 worker goroutine 上执行。
func (r *Runtime) callOnWorker(export string, payload [][]byte, withHost bool) callResult {
	r.mu.Lock()
	raw, hooks, broken := r.raw, r.hooks, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return callResult{err: ErrBroken}
	case raw == nil:
		return callResult{err: ErrClosed}
	}
	// 导出名只用来校验存在性:实际调用经 boot 的 __adaInvoke,才能 await 返回值。
	if _, ok := hooks[export]; !ok {
		return callResult{err: fmt.Errorf("jsrt: 模块未导出 %q", export)}
	}
	invoke, ok := hooks[invokeExport]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: 运行时引导模块缺少 %s", invokeExport)}
	}
	nameVal, err := raw.FromGo(export)
	if err != nil {
		return callResult{err: fmt.Errorf("jsrt: 导出名无法转换: %w", err)}
	}
	argsVal, err := raw.FromGo(joinPayload(payload))
	if err != nil {
		return callResult{err: fmt.Errorf("jsrt: 参数无法转换: %w", err)}
	}
	host := engine.Undefined()
	if withHost {
		host = r.host
	}
	v, err := r.runCall(func(rt *moejs.Runtime, ok, fail moejs.Value) error {
		_, callErr := rt.Call(invoke, nameVal, host, argsVal, ok, fail)
		return callErr
	})
	if err != nil {
		return callResult{err: err}
	}
	return callResult{value: v}
}

// callResident 在常驻循环上发起一次调用。
//
// 与 Scoped 的差别：任务只负责「把调用挂上去」，结果由 boot 的 onOK/onFail
// 回调结算；调用方在循环之外等待。Call 之间用 callMu 串行，保持现有
// callCtx / handlers 的单调用假设。
func (r *Runtime) callResident(ctx context.Context, export string, payload [][]byte, withHost bool) (any, error) {
	if r.reentrant() {
		return nil, ErrReentrant
	}
	r.callMu.Lock()
	defer r.callMu.Unlock()

	r.mu.Lock()
	raw, hooks, broken, resident := r.raw, r.hooks, r.broken, r.resident
	r.mu.Unlock()
	switch {
	case !resident || raw == nil:
		return nil, ErrClosed
	case broken:
		return nil, ErrBroken
	}
	if _, ok := hooks[export]; !ok {
		return nil, fmt.Errorf("jsrt: 模块未导出 %q", export)
	}
	invoke, ok := hooks[invokeExport]
	if !ok {
		return nil, fmt.Errorf("jsrt: 运行时引导模块缺少 %s", invokeExport)
	}

	c := &loopCall{res: make(chan callResult, 1)}
	posted := r.postToLoop(func(rt *moejs.Runtime) {
		r.callCtx = ctx
		clearCtx := func() { r.callCtx = nil }
		okFn := rt.Function("__adaOK", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			clearCtx()
			var v any
			if len(args) > 0 {
				got, err := rt.ToGo(args[0])
				if err != nil {
					c.settle(nil, &returnConvError{err: err})
					return engine.Undefined(), nil
				}
				v = got
			}
			c.settle(v, nil)
			return engine.Undefined(), nil
		})
		failFn := rt.Function("__adaFail", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
			clearCtx()
			msg := "未知错误"
			if len(args) > 0 {
				if s, err := rt.ToGo(args[0]); err == nil {
					msg = fmt.Sprint(s)
				}
			}
			c.settle(nil, fmt.Errorf("jsrt: JS 异常: %s", msg))
			return engine.Undefined(), nil
		})
		nameVal, err := rt.FromGo(export)
		if err != nil {
			clearCtx()
			c.settle(nil, fmt.Errorf("jsrt: 导出名无法转换: %w", err))
			return
		}
		argsVal, err := rt.FromGo(joinPayload(payload))
		if err != nil {
			clearCtx()
			c.settle(nil, fmt.Errorf("jsrt: 参数无法转换: %w", err))
			return
		}
		host := engine.Undefined()
		if withHost {
			host = r.host
		}
		if _, err := rt.Call(invoke, nameVal, host, argsVal, okFn, failFn); err != nil {
			clearCtx()
			c.settle(nil, err)
		}
	})
	if !posted {
		return nil, ErrClosed
	}

	select {
	case got := <-c.res:
		return got.value, got.err
	case <-ctx.Done():
		r.interrupt(ctx.Err())
		select {
		case <-c.res:
		case <-time.After(interruptGrace):
		}
		r.mu.Lock()
		r.broken = true
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: 常驻调用 %q 已取消(%v)", ErrBroken, export, ctx.Err())
	}
}

// wrap 把 moejs 的错误翻译成带堆栈的宿主错误。
func (r *Runtime) wrap(raw *moejs.Runtime, err error) error {
	var interrupted *moejs.InterruptedError
	if errors.As(err, &interrupted) {
		return fmt.Errorf("jsrt: JS 执行被中断: %w", err)
	}
	var exc *moejs.Exception
	if errors.As(err, &exc) {
		return fmt.Errorf("jsrt: JS 异常: %s", r.mapStack(raw.StackTrace(exc)))
	}
	return fmt.Errorf("jsrt: JS 调用失败: %w", err)
}

var stackFrameRe = regexp.MustCompile(`([A-Za-z0-9_./\\-]+\.(?:[cm]?[jt]sx?)):(\d+):(\d+)`)

// mapStack 用 Artifact.SourceMap 把生成代码的行列映射回源文件。
// 解析失败或找不到映射时保留原栈，不影响错误本身。
func (r *Runtime) mapStack(stack string) string {
	if r.art == nil || len(r.art.SourceMap) == 0 {
		return stack
	}
	cache := make(map[string]*compile.SourceMap)
	return stackFrameRe.ReplaceAllStringFunc(stack, func(frame string) string {
		parts := stackFrameRe.FindStringSubmatch(frame)
		if len(parts) != 4 {
			return frame
		}
		file, lineText, colText := parts[1], parts[2], parts[3]
		data, ok := r.art.SourceMap[file]
		if !ok {
			return frame
		}
		sm := cache[file]
		if sm == nil {
			parsed, err := compile.ParseSourceMap([]byte(data))
			if err != nil {
				return frame
			}
			cache[file] = parsed
			sm = parsed
		}
		line, err := strconv.Atoi(lineText)
		if err != nil {
			return frame
		}
		col, err := strconv.Atoi(colText)
		if err != nil {
			return frame
		}
		source, origLine, origCol, ok := sm.Lookup(line, col)
		if !ok {
			return frame
		}
		return source + ":" + strconv.Itoa(origLine) + ":" + strconv.Itoa(origCol)
	})
}

func (r *Runtime) interrupt(reason error) {
	r.mu.Lock()
	loop, raw := r.loop, r.raw
	r.mu.Unlock()
	// 有循环时用 Loop.Interrupt:只有它能唤醒正在睡眠等定时器/任务的循环,
	// 否则一次「取消」要等到下一个 timer 到期才生效。
	if loop != nil {
		loop.Interrupt(reason)
		return
	}
	if raw != nil {
		raw.Interrupt(reason)
	}
}

func (r *Runtime) clearInterrupt() {
	r.mu.Lock()
	raw := r.raw
	r.mu.Unlock()
	if raw != nil {
		raw.ClearInterrupt()
	}
}

// nativeOn 实现 ctx.on(event, handler):把 JS 函数登记进 handlers,
// 并把「如何调用它」交给桥接层。登记发生在 worker 上,所以 handlers 无需加锁。
func (r *Runtime) nativeOn(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.On == nil {
			return engine.Undefined(), fmt.Errorf("ctx.on 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("ctx.on(event, handler) 需要两个参数")
		}
		rawEvent, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.on 的事件名无法转换: %w", err)
		}
		event, ok := rawEvent.(string)
		if !ok || event == "" {
			return engine.Undefined(), fmt.Errorf("ctx.on 的事件名必须是非空字符串")
		}
		if !engine.IsCallable(args[1]) {
			return engine.Undefined(), fmt.Errorf("ctx.on 的第二个参数必须是函数")
		}
		r.nextID++
		id := r.nextID
		r.handlers[id] = handlerEntry{fn: args[1], this: engine.Undefined()}
		if err := r.opts.Host.On(event, func(ctx context.Context, payload any) (any, error) {
			return r.invokeHandler(ctx, id, payload)
		}); err != nil {
			delete(r.handlers, id)
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// nativeEffect 实现 ctx.effect(label, cleanup):登记清理函数,卸载时按逆序调用。
func (r *Runtime) nativeEffect(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Effect == nil {
			return engine.Undefined(), fmt.Errorf("ctx.effect 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("ctx.effect(label, cleanup) 需要两个参数")
		}
		rawLabel, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.effect 的标签无法转换: %w", err)
		}
		label, ok := rawLabel.(string)
		if !ok || label == "" {
			return engine.Undefined(), fmt.Errorf("ctx.effect 的标签必须是非空字符串")
		}
		if !engine.IsCallable(args[1]) {
			return engine.Undefined(), fmt.Errorf("ctx.effect 的第二个参数必须是函数")
		}
		r.nextID++
		id := r.nextID
		r.handlers[id] = handlerEntry{fn: args[1], this: engine.Undefined()}
		if err := r.opts.Host.Effect(label, func(ctx context.Context) error {
			_, err := r.invokeCallback(ctx, id, nil)
			return err
		}); err != nil {
			delete(r.handlers, id)
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// invokeHandler 调用一个 ctx.on 注册的 JS 函数;可从任意 goroutine 调用。
func (r *Runtime) invokeHandler(ctx context.Context, id uint64, payload any) (any, error) {
	if r.reentrant() {
		return nil, ErrReentrant
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("jsrt: 事件载荷无法序列化: %w", err)
	}
	return r.invokeCallback(ctx, id, [][]byte{b})
}

// invokeCallback 在 worker 上调用一个已登记的函数;payload 为逐个 JSON 编码的参数。
func (r *Runtime) invokeCallback(ctx context.Context, id uint64, payload [][]byte) (any, error) {
	if r.reentrant() {
		return nil, ErrReentrant
	}
	res := make(chan callResult, 1)
	if err := r.enqueue(ctx, func() { res <- r.invokeOnWorker(id, payload) }); err != nil {
		return nil, err
	}
	select {
	case got := <-res:
		return got.value, got.err
	case <-ctx.Done():
		r.interrupt(ctx.Err())
		select {
		case <-res:
			r.clearInterrupt()
			return nil, ctx.Err()
		case <-time.After(interruptGrace):
			r.mu.Lock()
			r.broken = true
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: 回调 %d 在 %s 内未响应取消", ErrBroken, id, interruptGrace)
		}
	}
}

func (r *Runtime) invokeOnWorker(id uint64, payload [][]byte) callResult {
	r.mu.Lock()
	raw, hooks, broken := r.raw, r.hooks, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return callResult{err: ErrBroken}
	case raw == nil:
		return callResult{err: ErrClosed}
	}
	entry, ok := r.handlers[id]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: 回调 %d 不存在", id)}
	}
	invokeFn, ok := hooks[invokeFnExport]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: 运行时引导模块缺少 %s", invokeFnExport)}
	}
	argsVal, err := raw.FromGo(joinPayload(payload))
	if err != nil {
		return callResult{err: fmt.Errorf("jsrt: 回调参数无法转换: %w", err)}
	}
	v, err := r.runCall(func(rt *moejs.Runtime, ok, fail moejs.Value) error {
		_, callErr := rt.Call(invokeFn, entry.fn, engine.Undefined(), argsVal, ok, fail)
		return callErr
	})
	if err != nil {
		// 回调的返回值对宿主没有意义(emit / 清理语义),转换失败不应让调用失败。
		var conv *returnConvError
		if errors.As(err, &conv) {
			return callResult{value: nil}
		}
		return callResult{err: err}
	}
	return callResult{value: v}
}

// describeService 把 JS 侧给 ctx.provide 的值转成 Go 侧可用的 Service。
//
// 只认对象自身的属性:函数成为方法,其余按 JSON 成为数据。
// 深层嵌套里的函数不会变成方法——「跨边界不传递函数」是 C3 的硬约束。
func (r *Runtime) describeService(raw *moejs.Runtime, name string, v moejs.Value) (*Service, error) {
	svc := &Service{rt: r, name: name, ids: map[string]uint64{}}
	if !v.IsObject() {
		data, err := raw.ToGo(v)
		if err != nil {
			return nil, fmt.Errorf("jsrt: 服务 %q 的值无法转换: %w", name, err)
		}
		svc.value = data
		return svc, nil
	}
	realm := raw.Realm()
	obj := v.AsObject()
	svc.data = map[string]any{}
	for _, key := range obj.OwnPropertyKeys() {
		if key.IsSymbol() || key.IsPrivate() {
			continue
		}
		keyName := key.GoString()
		val, err := obj.GetProp(realm, key)
		if err != nil {
			return nil, fmt.Errorf("jsrt: 服务 %q 的属性 %q 读取失败: %w", name, keyName, err)
		}
		if engine.IsCallable(val) {
			r.nextID++
			id := r.nextID
			// this 绑定到服务对象本身:方法里的 this.x 才能读到兄弟成员。
			r.handlers[id] = handlerEntry{fn: val, this: v}
			svc.ids[keyName] = id
			svc.methods = append(svc.methods, keyName)
			continue
		}
		data, err := raw.ToGo(val)
		if err != nil {
			return nil, fmt.Errorf("jsrt: 服务 %q 的属性 %q 无法转换(深层函数不能跨边界): %w", name, keyName, err)
		}
		svc.data[keyName] = data
	}
	sort.Strings(svc.methods)
	return svc, nil
}

// invokeMethod 调用 JS 服务方法;可从任意 goroutine 调用。
func (r *Runtime) invokeMethod(ctx context.Context, id uint64, payload [][]byte) (any, error) {
	if r.reentrant() {
		return nil, ErrReentrant
	}
	res := make(chan callResult, 1)
	if err := r.enqueue(ctx, func() { res <- r.invokeMethodOnWorker(id, payload) }); err != nil {
		return nil, err
	}
	select {
	case got := <-res:
		return got.value, got.err
	case <-ctx.Done():
		r.interrupt(ctx.Err())
		select {
		case <-res:
			r.clearInterrupt()
			return nil, ctx.Err()
		case <-time.After(interruptGrace):
			r.mu.Lock()
			r.broken = true
			r.mu.Unlock()
			return nil, fmt.Errorf("%w: 方法调用 %d 在 %s 内未响应取消", ErrBroken, id, interruptGrace)
		}
	}
}

func (r *Runtime) invokeMethodOnWorker(id uint64, payload [][]byte) callResult {
	r.mu.Lock()
	raw, hooks, broken := r.raw, r.hooks, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return callResult{err: ErrBroken}
	case raw == nil:
		return callResult{err: ErrClosed}
	}
	entry, ok := r.handlers[id]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: 服务方法 %d 不存在", id)}
	}
	invokeFn, ok := hooks[invokeFnExport]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: 运行时引导模块缺少 %s", invokeFnExport)}
	}
	argsVal, err := raw.FromGo(joinPayload(payload))
	if err != nil {
		return callResult{err: fmt.Errorf("jsrt: 方法参数无法转换: %w", err)}
	}
	// this 绑定到服务对象本身:方法里的 this.x 才能读到兄弟成员。
	v, err := r.runCall(func(rt *moejs.Runtime, ok, fail moejs.Value) error {
		_, callErr := rt.Call(invokeFn, entry.fn, entry.this, argsVal, ok, fail)
		return callErr
	})
	if err != nil {
		return callResult{err: err}
	}
	return callResult{value: v}
}

// nativeWaterfall 实现 ctx.waterfall(event, handler):登记为宿主 waterfall 监听器。
func (r *Runtime) nativeWaterfall(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.Waterfall == nil {
			return engine.Undefined(), fmt.Errorf("ctx.waterfall 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("ctx.waterfall(event, handler) 需要两个参数")
		}
		rawEvent, err := raw.ToGo(args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("ctx.waterfall 的事件名无法转换: %w", err)
		}
		event, ok := rawEvent.(string)
		if !ok || event == "" {
			return engine.Undefined(), fmt.Errorf("ctx.waterfall 的事件名必须是非空字符串")
		}
		if !engine.IsCallable(args[1]) {
			return engine.Undefined(), fmt.Errorf("ctx.waterfall 的第二个参数必须是函数")
		}
		r.nextID++
		id := r.nextID
		r.handlers[id] = handlerEntry{fn: args[1], this: engine.Undefined()}
		if err := r.opts.Host.Waterfall(event, func(ctx context.Context, input any, next func(any) (any, error)) (any, error) {
			return r.invokeWaterfall(ctx, id, input, next)
		}); err != nil {
			delete(r.handlers, id)
			return engine.Undefined(), err
		}
		return engine.Undefined(), nil
	}
}

// wfValue 是 waterfall 处理器与宿主之间的一次往返结果。
type wfValue struct {
	value any
	err   error
}

// invokeWaterfall 在 worker 上执行 JS 中间件;JS 调 next 时在这里回落到宿主。
//
// 环的规避:同一运行时不允许嵌套(ErrReentrant)。跨运行时的链是安全的——
// 每个运行时有自己的 worker,宿主负责把 next 调用串起来。
func (r *Runtime) invokeWaterfall(ctx context.Context, id uint64, input any, next func(any) (any, error)) (any, error) {
	r.mu.Lock()
	if r.inWaterfall > 0 || r.inHost > 0 {
		r.mu.Unlock()
		return nil, ErrReentrant
	}
	r.inWaterfall++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inWaterfall--
		r.mu.Unlock()
	}()

	b, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("jsrt: waterfall 输入无法序列化: %w", err)
	}
	res := make(chan callResult, 1)
	nextReq := make(chan wfValue, 1)
	nextResp := make(chan wfValue, 1)
	if err := r.enqueue(ctx, func() { res <- r.invokeWaterfallOnWorker(id, b, nextReq, nextResp) }); err != nil {
		return nil, err
	}
	for {
		select {
		case got := <-res:
			if got.err != nil {
				return got.value, got.err
			}
			// JS 只能返回 JSON;还原成输入类型后,宿主的 `v.(*T)` 才能看到改写。
			return coerceWaterfall(input, got.value), nil
		case req := <-nextReq:
			// 在监听器所在的 goroutine 上继续下游,与宿主 waterfall 语义一致。
			// 下游如果是 Go 处理器,它期望的是类型化输入,因此 next 参数也先还原。
			v, nextErr := next(coerceWaterfall(input, req.value))
			nextResp <- wfValue{value: v, err: nextErr}
		case <-ctx.Done():
			r.interrupt(ctx.Err())
			// 若 worker 正阻塞在 next 上,用一条错误结果把它放出来,避免 goroutine 泄漏。
			select {
			case nextResp <- wfValue{err: ctx.Err()}:
			default:
			}
			select {
			case <-res:
				r.clearInterrupt()
				return nil, ctx.Err()
			case <-time.After(interruptGrace):
				r.mu.Lock()
				r.broken = true
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: waterfall 处理器 %d 在 %s 内未响应取消", ErrBroken, id, interruptGrace)
			}
		}
	}
}

func (r *Runtime) invokeWaterfallOnWorker(id uint64, payload []byte, nextReq, nextResp chan wfValue) callResult {
	r.mu.Lock()
	raw, broken := r.raw, r.broken
	r.mu.Unlock()
	switch {
	case broken:
		return callResult{err: ErrBroken}
	case raw == nil:
		return callResult{err: ErrClosed}
	}
	entry, ok := r.handlers[id]
	if !ok {
		return callResult{err: fmt.Errorf("jsrt: waterfall 处理器 %d 不存在", id)}
	}
	input, err := raw.ParseJSON(payload)
	if err != nil {
		return callResult{err: fmt.Errorf("jsrt: waterfall 输入不是合法 JSON: %w", err)}
	}
	var called bool
	nextFn := raw.Function("next", 1, func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if called {
			return engine.Undefined(), fmt.Errorf("waterfall 处理器重复调用 next")
		}
		called = true
		var v any
		if len(args) > 0 {
			converted, convErr := raw.ToGo(args[0])
			if convErr != nil {
				return engine.Undefined(), fmt.Errorf("next 的参数无法转换: %w", convErr)
			}
			v = converted
		}
		nextReq <- wfValue{value: v}
		got := <-nextResp
		if got.err != nil {
			return engine.Undefined(), got.err
		}
		out, fromErr := raw.FromGo(got.value)
		if fromErr != nil {
			return engine.Undefined(), fmt.Errorf("next 的结果无法转换: %w", fromErr)
		}
		return out, nil
	})
	// waterfall 中间件必须是同步的:它要在返回前给出值,next 链才能续下去。
	// 因此这里只把调用放进循环(让定时器与微任务可用),不 await 返回值。
	var out moejs.Value
	var callErr error
	if err := r.runOnLoop(func(rt *moejs.Runtime) error {
		out, callErr = rt.CallFunction(entry.fn, entry.this, input, nextFn)
		return nil
	}); err != nil {
		return callResult{err: err}
	}
	if callErr != nil {
		return callResult{err: r.wrap(raw, callErr)}
	}
	v, convErr := raw.ToGo(out)
	if convErr != nil {
		return callResult{value: nil}
	}
	return callResult{value: v}
}

// Exports 返回入口模块的导出名(排序),用于诊断与装配报告。
func (r *Runtime) Exports() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.hooks))
	for name := range r.hooks {
		// 引导模块自己的两个入口(await/回调)不是插件的表面,诊断里不该出现。
		if strings.HasPrefix(name, internalPrefix) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// rejectionError 把 Promise 拒绝原因转成可读错误（优先 Error.message）。
func (r *Runtime) rejectionError(reason moejs.Value) error {
	if reason.IsObject() {
		r.mu.Lock()
		raw := r.raw
		r.mu.Unlock()
		if raw != nil {
			realm := raw.Realm()
			if msg, err := reason.AsObject().GetProp(realm, realm.KeyFromGoString("message")); err == nil {
				if s := msg.String(); s != "" {
					return fmt.Errorf("jsrt: 未处理的 Promise 拒绝: %s", s)
				}
			}
		}
	}
	return fmt.Errorf("jsrt: 未处理的 Promise 拒绝: %s", reason.String())
}

// releaseLoopHold 释放常驻循环的持续 hold；可重复调用。
func (r *Runtime) releaseLoopHold() {
	r.mu.Lock()
	hold := r.loopHold
	r.loopHold = nil
	r.mu.Unlock()
	if hold != nil {
		hold(nil)
	}
}

// Close 停止运行时。可重复调用。
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.finish()
	api := r.nodeAPI
	r.nodeAPI = nil
	r.mu.Unlock()

	r.closeListeners()
	r.closeZlibStreams()
	r.closeConns()
	if api != nil {
		_ = api.Close()
	}
	r.closeAllFDs()
	r.closeNodeRoot()
	r.releaseLoopHold()
	// 先收事件循环:它的 Terminate 会中断正在跑的 JS、丢弃未到期的定时器与
	// 未结算的 promise,并等循环收尾。之后再停 worker。
	r.terminateLoop()
	r.interrupt(ErrClosed)
	r.stop()
	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("jsrt: 关闭超时: %w", ctx.Err())
	}
}

// shutdown 是 New 失败路径的清理:停 worker 但不要求优雅(还没有可用的 Runtime)。
func (r *Runtime) shutdown() {
	r.mu.Lock()
	r.closed = true
	r.finish()
	api := r.nodeAPI
	r.nodeAPI = nil
	r.mu.Unlock()
	r.closeListeners()
	r.closeZlibStreams()
	r.closeConns()
	if api != nil {
		_ = api.Close()
	}
	r.closeAllFDs()
	r.closeNodeRoot()
	r.releaseLoopHold()
	r.terminateLoop()
	r.stop()
	<-r.stopped
}

func (r *Runtime) stop() { r.quitOne.Do(func() { close(r.quit) }) }
