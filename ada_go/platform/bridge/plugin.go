// Package bridge 把编译产物接成 kernel 插件:M2 的第一段桥。
//
// 核心约束:JS 跑在自己的 goroutine 上,而内核的生命周期操作是**单写者**
// (M1 §1 的并发假设)。因此 JS 侧不能直接调用 Provide/Emit/Plugin——
// 那会同时触碰服务表、事件总线与 fiber 表。这里的做法是:
//
//	JS ctx.provide / ctx.emit ──暂存──▶ stager ──apply 返回后──▶ 内核协程提交
//
// 暂存是按调用顺序的确定性队列;提交发生在 apply 返回之后、仍在调用 Apply 的
// 那个内核协程上,所以不引入跨协程写。
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
	"github.com/issueye/ada_go_v2/plugins/seam/shell"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
	seamworkspace "github.com/issueye/ada_go_v2/plugins/seam/workspace"
	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

// applyTimeout 是装配预算。装配只做编译 + 加载 + 一次 apply,
// 不应该长时间占住内核的控制协程。
const applyTimeout = 10 * time.Second

// Options 描述一个 JS 插件。
type Options struct {
	Name     string
	Artifact *compile.Artifact
	// Logf 接收宿主判定的日志(JS 侧的 ctx.log);nil 表示丢弃。
	Logf func(format string, args ...any)
	// Inject 是清单声明的依赖;TS 插件可注入 tools/shell 以使用脚本工具能力。
	Inject kernel.Inject
}

type provideReq struct {
	name  string
	value *jsrt.Service
}

type emitReq struct {
	event   string
	payload any
}

type toolReq struct {
	def tools.ToolDef
}

type onReq struct {
	event  string
	invoke func(ctx context.Context, payload any) (any, error)
}

type effectReq struct {
	label   string
	dispose func(ctx context.Context) error
}

type wfReq struct {
	event  string
	invoke func(ctx context.Context, input any, next func(any) (any, error)) (any, error)
}

// stager 收集 JS 侧 ctx.provide / ctx.emit 的请求,等回到内核协程再提交。
//
// 为什么必须暂存而不是「加锁直接写」:加锁只能消除数据竞争,消除不了语义竞争——
// 内核的 recompute 依赖「单协程推进」,第二个协程插进来会造成半新半旧的依赖图。
type stager struct {
	mu         sync.Mutex
	closed     bool
	ons        []onReq
	tools      []toolReq
	waterfalls []wfReq
	effects    []effectReq
	provides   []provideReq
	emits      []emitReq
}

func (s *stager) on(event string, invoke func(ctx context.Context, payload any) (any, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.on 只能在 apply 期间调用")
	}
	s.ons = append(s.ons, onReq{event: event, invoke: invoke})
	return nil
}

func (s *stager) waterfall(event string, invoke func(ctx context.Context, input any, next func(any) (any, error)) (any, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.waterfall 只能在 apply 期间调用")
	}
	s.waterfalls = append(s.waterfalls, wfReq{event: event, invoke: invoke})
	return nil
}

func (s *stager) effect(label string, dispose func(ctx context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.effect 只能在 apply 期间调用")
	}
	s.effects = append(s.effects, effectReq{label: label, dispose: dispose})
	return nil
}

func (s *stager) provide(name string, value *jsrt.Service) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.provide 只能在 apply 期间调用")
	}
	s.provides = append(s.provides, provideReq{name: name, value: value})
	return nil
}

func (s *stager) tool(def tools.ToolDef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.tool 只能在 apply 期间调用")
	}
	s.tools = append(s.tools, toolReq{def: def})
	return nil
}

func (s *stager) emit(event string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("ctx.emit 只能在 apply 期间调用")
	}
	s.emits = append(s.emits, emitReq{event: event, payload: payload})
	return nil
}

// drain 关闭入口并取出暂存请求。之后 JS 再调用会被拒绝:
// 没有提交点的调用必须报错,而不是静默丢失。
func (s *stager) drain() ([]wfReq, []onReq, []effectReq, []provideReq, []emitReq, []toolReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.waterfalls, s.ons, s.effects, s.provides, s.emits, s.tools
}

func (s *stager) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// pendingEffects 复制已登记的清理函数,用于装配失败时的兜底回收。
func (s *stager) pendingEffects() []effectReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]effectReq, len(s.effects))
	copy(out, s.effects)
	return out
}

// runEffects 按逆序执行 JS 登记的清理函数(与内核 effect 栈同语义:
// 后创建的资源可能依赖先创建的,必须先回收)。
func runEffects(plugin string, effects []effectReq, ctx context.Context) []error {
	var errs []error
	for i := len(effects) - 1; i >= 0; i-- {
		if err := effects[i].dispose(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bridge: 插件 %s 的 effect %q 清理失败: %w", plugin, effects[i].label, err))
		}
	}
	return errs
}

// Plugin 是 kernel.Plugin 的 JS 实现。
type Plugin struct {
	opts Options

	mu   sync.Mutex
	logs []string
	rt   *jsrt.Runtime
	kctx *kernel.Context // 运行期 emit 用;卸载时清空
}

// New 构造 JS 插件。它不装配运行时——装配发生在 Apply,失败时内核会把 fiber 置为 failed。
func New(opts Options) (*Plugin, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("bridge: 插件名不能为空")
	}
	if opts.Artifact == nil {
		return nil, fmt.Errorf("bridge: Artifact 不能为 nil")
	}
	return &Plugin{opts: opts}, nil
}

// Name 返回插件名。
func (p *Plugin) Name() string { return p.opts.Name }

// Inject 声明无依赖:M2 第一段桥不做 JS 侧的服务注入。
func (p *Plugin) Inject() kernel.Inject { return p.opts.Inject }

// Provide 声明不提供服务:实际提供什么由 JS 在 apply 期间通过 ctx.provide 决定,
// 装配期的预检(M3)会在加载 Artifact 后根据运行结果补上。
func (p *Plugin) Provide() []string { return nil }

// Config 返回 nil:JS 插件的配置 schema 由插件目录里的 config.schema.json 提供,
// 加载器尚未实现(M3)。
func (p *Plugin) Config() *schema.Schema { return nil }

// nodeOptions 只保留 Node 模块的白名单开关；文件系统根与权限走新的 Permissions.FS。
func nodeOptions() *jsrt.NodeOptions {
	return &jsrt.NodeOptions{Buffer: true}
}

// nodePermissions 把 fs/workspace 的现有策略翻译成 jsrt 的 FS 权限。
//
// 规则与旧的 nodeOptions 保持一致：
//   - fs/workspace 链上有策略时，node:fs 不开放（必须走 ada:fs，避免绕过策略）；
//   - 没有策略时，以 fs 链的根为 /workspace，读开放，写按 workspace 当前根决定；
//   - 路径白名单固定为 /workspace/**，不额外放宽。
func nodePermissions(ctx *kernel.Context, fsChain *seamfs.Chain) jsrt.Permissions {
	if ctx == nil || fsChain == nil || len(fsChain.Applied()) > 0 {
		return jsrt.Permissions{}
	}
	write := true
	if v, ok := ctx.TryService(seamworkspace.ServiceName); ok {
		if ch, ok := v.(*seamworkspace.Chain); ok {
			if len(ch.Applied()) > 0 {
				return jsrt.Permissions{}
			}
			if svc, err := ch.Get(); err == nil {
				if info, ierr := svc.Current(ctx.Context()); ierr != nil || info.ReadOnly {
					write = false
				}
			} else {
				write = false
			}
		}
	}
	root, err := fsChain.Root(ctx.Context())
	if err != nil || root == "" {
		return jsrt.Permissions{}
	}
	access := jsrt.FSReadOnly
	if write {
		access = jsrt.FSReadWrite
	}
	return jsrt.Permissions{FS: jsrt.FSPermission{
		Enabled: true,
		Root:    root,
		Mount:   "/workspace",
		Cwd:     "/workspace",
		Access:  access,
		Allow:   []string{"/workspace/**"},
	}}
}

// Apply 装配 JS 运行时,调用入口模块的 apply(ctx, cfg),再提交暂存的 provide/emit。
func (p *Plugin) Apply(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
	// kernel.Apply 的签名没有 context(M1 冻结),这里用一个有界预算兜底。
	actx, cancel := context.WithTimeout(context.Background(), applyTimeout)
	defer cancel()

	stage := &stager{}
	var toolsReg *tools.Registry
	if v, ok := ctx.TryService("tools"); ok {
		toolsReg, _ = v.(*tools.Registry)
	}
	var shellChain *shell.Chain
	if v, ok := ctx.TryService("shell"); ok {
		shellChain, _ = v.(*shell.Chain)
	}
	var fsChain *seamfs.Chain
	if v, ok := ctx.TryService("fs"); ok {
		fsChain, _ = v.(*seamfs.Chain)
	}
	// secrets 是可选能力:没有装 seam/secrets 时不提供,模块本身仍然可导入,
	// 调用 get 会得到「宿主未安装 secrets」的明确错误(而不是链接期失败)。
	var secretsChain *seamsecrets.Chain
	if v, ok := ctx.TryService("secrets"); ok {
		secretsChain, _ = v.(*seamsecrets.Chain)
	}
	// 插件私有 KV:进程内,随 fiber 卸载丢弃;持久化需要 storage seam。
	store := newPluginStore()
	modules := map[string]*moejs.Module{}
	if shellChain != nil {
		// run 保留旧契约(返回 stdout 字符串,非零退出抛错);exec 返回结构化结果,
		// 让脚本工具可以自己决定非零退出、stderr 与截断如何呈现给模型。
		m, cerr := moejs.Compile("ada:shell", `const host = globalThis.__adaHost;
export function run(program, args, dir) { return host.shell(program, args, dir); }
export function exec(program, args, dir, options) {
  return JSON.parse(host.shellExec(program, args, dir, JSON.stringify(options || {})));
}
export function platform() { return globalThis.__adaOS; }
`)
		if cerr != nil {
			return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
		}
		modules["ada:shell"] = m
	}
	if fsChain != nil {
		m, cerr := moejs.Compile("ada:fs", "const host = globalThis.__adaHost;\nexport function read(path) { return host.fs.read(path); }\nexport function write(path, content, createDirs) { return host.fs.write(path, content, createDirs); }\nexport function edit(path, old, newText, all) { return host.fs.edit(path, old, newText, all); }\nexport function search(query, path, limit) { return JSON.parse(host.fs.search(query, path || \"/workspace\", limit || 0)); }\nexport function list(path, recursive, limit) { return JSON.parse(host.fs.list(path || \"/workspace\", recursive, limit || 0)); }\n")
		if cerr != nil {
			return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
		}
		modules["ada:fs"] = m
	}
	// ada:secrets 始终可导入:get(name) 走 seam/secrets 链(带调用级 ctx,
	// secrets-guard 据此取 principal);has(name) 只回答「能不能读到」,不返回值。
	// 这样脚本可以按「有没有配 key」切换 provider,而不必把 secret 暴露给模型。
	sm, cerr := moejs.Compile("ada:secrets", `const host = globalThis.__adaHost;
export function get(name) { return host.secretsGet(String(name)); }
export function has(name) { try { get(name); return true } catch (_err) { return false } }
`)
	if cerr != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
	}
	modules["ada:secrets"] = sm
	// ada:plan 始终可导入:计划属于会话,宿主函数从调用 ctx 取会话;
	// 没有会话上下文时 get/set/update 返回明确错误,而不是写到某个猜出来的会话。
	pm, cerr := moejs.Compile("ada:plan", `const host = globalThis.__adaHost;
export function get() { return JSON.parse(host.plan.get()); }
export function set(plan) { return JSON.parse(host.plan.set(JSON.stringify(plan))); }
export function update(index, status) { return JSON.parse(host.plan.update(index, status)); }
`)
	if cerr != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
	}
	modules["ada:plan"] = pm
	// ada:session 提供会话身份与「插件自有事件」追加;写事件时由宿主加
	// plugin:<id>/ 前缀并强制 Ignorable,脚本不能伪造模型可见事件。
	sessionMod, cerr := moejs.Compile("ada:session", `const host = globalThis.__adaHost;
export function current() { const v = host.session.current(); return v === null ? null : JSON.parse(v); }
export function append(kind, data) { return host.session.append(String(kind), JSON.stringify(data === undefined ? null : data)); }
export function history(from, limit) { return JSON.parse(host.session.history(Number(from) || 0, Number(limit) || 0)); }
export function recent(limit) { return JSON.parse(host.session.recent(Number(limit) || 0)); }
`)
	if cerr != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
	}
	modules["ada:session"] = sessionMod
	// ada:store 是 ctx.store 的模块形式:插件私有、进程内 KV;重启不保留。
	storeMod, cerr := moejs.Compile("ada:store", `const host = globalThis.__adaHost;
export function get(key) { return host.store.get(String(key)); }
export function set(key, value) { host.store.set(String(key), value); }
export function remove(key) { host.store.delete(String(key)); }
export function list(prefix) { return host.store.list(String(prefix || "")); }
`)
	if cerr != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, cerr, "JS 插件 %s 注入模块编译失败", p.opts.Name)
	}
	modules["ada:store"] = storeMod
	rt, err := jsrt.New(actx, jsrt.Options{
		Artifact:    p.opts.Artifact,
		Logf:        p.record,
		Modules:     modules,
		Node:        nodeOptions(),
		Permissions: nodePermissions(ctx, fsChain),
		// 注入宿主 OS,供 ada:shell 选择默认 shell(cmd.exe / /bin/sh)。
		Globals: map[string]any{"__adaOS": runtime.GOOS},
		Host: jsrt.Host{
			Provide:   stage.provide,
			Emit:      func(event string, payload any) error { return p.emitTo(stage, event, payload) },
			On:        stage.on,
			Effect:    stage.effect,
			Waterfall: stage.waterfall,
			// 网络能力直连标准库(ADR 0011/0012):约束只针对文件与写入,仓库已无 net seam/policy。
			// 仍然是宿主注入的 JSON 入口,脚本没有直接 syscall 的路径。
			HTTPDo:    nodeHTTPDo,
			DNSLookup: nodeDNSLookup,
			NetDial:   nodeDial,
			TLSDial:   nodeTLSDial,
			// secret 按名读,走 seam/secrets 链:provider 决定来源,policy 决定授权,
			// 桥接层不缓存、不写日志,避免 secret 出现在事件或审计里。
			Secrets: func(sctx context.Context, name string) (string, error) {
				if secretsChain == nil {
					return "", fmt.Errorf("secrets 服务不可用:请挂载 seam-secrets 与 secrets-env")
				}
				getter, gerr := secretsChain.Get()
				if gerr != nil {
					return "", gerr
				}
				return getter.Get(sctx, name)
			},
			Shell: func(sctx context.Context, program string, args []string, dir string) (string, error) {
				if shellChain == nil {
					return "", fmt.Errorf("shell 服务不可用")
				}
				sh, err := shellChain.Get()
				if err != nil {
					return "", err
				}
				res, err := sh.Run(sctx, shell.Command{Program: program, Args: args, Dir: shell.Path(dir), Timeout: 30 * time.Second})
				if err != nil {
					return "", err
				}
				text := strings.TrimSpace(string(res.Stdout))
				if stderr := strings.TrimSpace(string(res.Stderr)); stderr != "" {
					if text != "" {
						text += "\\n"
					}
					text += stderr
				}
				if res.ExitCode != 0 {
					return "", fmt.Errorf("命令退出码 %d: %s", res.ExitCode, text)
				}
				return text, nil
			},
			ShellExec: func(sctx context.Context, program string, args []string, dir string, options string) (string, error) {
				if shellChain == nil {
					return "", fmt.Errorf("shell 服务不可用")
				}
				var opts struct {
					TimeoutMs int64 `json:"timeoutMs"`
				}
				if strings.TrimSpace(options) != "" {
					if err := json.Unmarshal([]byte(options), &opts); err != nil {
						return "", fmt.Errorf("shell.exec 的 options 非法: %w", err)
					}
				}
				sh, err := shellChain.Get()
				if err != nil {
					return "", err
				}
				res, err := sh.Run(sctx, shell.Command{
					Program: program, Args: args, Dir: shell.Path(dir),
					Timeout: time.Duration(opts.TimeoutMs) * time.Millisecond,
				})
				if err != nil {
					return "", err
				}
				// 跨 JSON 边界前再收一刀:shell-local 可能返回 4 MiB,
				// 但 AGENTS §5.6 不允许把大对象塞进 JSON。
				stdout, stderr := res.Stdout, res.Stderr
				truncated := res.Truncated
				half := maxShellExecBytes / 2
				if len(stdout) > half {
					stdout, truncated = stdout[:half], true
				}
				if len(stderr) > half {
					stderr, truncated = stderr[:half], true
				}
				payload, err := json.Marshal(shellExecResult{
					Stdout: string(stdout), Stderr: string(stderr),
					ExitCode: res.ExitCode, Truncated: truncated,
					DurationMs: res.Duration.Milliseconds(),
					Enforced:   res.Facts.Enforced, Backend: res.Facts.Backend, Mode: string(res.Facts.Mode),
				})
				if err != nil {
					return "", err
				}
				return string(payload), nil
			},
			FSRead: func(fctx context.Context, path string) (string, error) {
				if fsChain == nil {
					return "", fmt.Errorf("fs 服务不可用")
				}
				impl, err := fsChain.Get()
				if err != nil {
					return "", err
				}
				res, err := impl.Read(fctx, seamfs.ReadRequest{Path: seamfs.Path(path)})
				if err != nil {
					return "", err
				}
				return res.Content, nil
			},
			FSWrite: func(fctx context.Context, path, content string, createDirs bool) error {
				if fsChain == nil {
					return fmt.Errorf("fs 服务不可用")
				}
				impl, err := fsChain.Get()
				if err != nil {
					return err
				}
				_, err = impl.Write(fctx, seamfs.WriteRequest{
					Path: seamfs.Path(path), Content: []byte(content), CreateDirs: createDirs,
				})
				return err
			},
			FSEdit: func(fctx context.Context, path, old, newText string, all bool) (int, error) {
				if fsChain == nil {
					return 0, fmt.Errorf("fs 服务不可用")
				}
				impl, err := fsChain.Get()
				if err != nil {
					return 0, err
				}
				read, err := impl.Read(fctx, seamfs.ReadRequest{Path: seamfs.Path(path)})
				if err != nil {
					return 0, err
				}
				res, err := impl.Edit(fctx, seamfs.EditRequest{
					Path: seamfs.Path(path), IfVersion: read.Version, Old: old, New: newText, All: all,
				})
				if err != nil {
					return 0, err
				}
				return res.Replacements, nil
			},
			FSSearch: func(fctx context.Context, path, query string, limit int) (string, error) {
				if fsChain == nil {
					return "", fmt.Errorf("fs 服务不可用")
				}
				if !strings.HasPrefix(path, "/") {
					path = "/workspace/" + path
				}
				impl, err := fsChain.Get()
				if err != nil {
					return "", err
				}
				res, err := impl.Search(fctx, seamfs.SearchRequest{Path: seamfs.Path(path), Query: query, Limit: limit})
				if err != nil {
					return "", err
				}
				b, err := json.Marshal(res.Matches)
				if err != nil {
					return "", err
				}
				return string(b), nil
			},
			FSList: func(fctx context.Context, path string, recursive bool, limit int) (string, error) {
				if fsChain == nil {
					return "", fmt.Errorf("fs 服务不可用")
				}
				if !strings.HasPrefix(path, "/") {
					path = "/workspace/" + path
				}
				impl, err := fsChain.Get()
				if err != nil {
					return "", err
				}
				entries, err := impl.List(fctx, seamfs.ListRequest{Path: seamfs.Path(path), Recursive: recursive, Limit: limit})
				if err != nil {
					return "", err
				}
				b, err := json.Marshal(entries)
				if err != nil {
					return "", err
				}
				return string(b), nil
			},
			// ada:plan 的宿主实现:从调用 ctx 取会话,写 plugin:plan/updated 事件。
			PlanGet:    planGet,
			PlanSet:    planSet,
			PlanUpdate: planUpdate,
			// ada:session:身份读取 + 插件自有事件追加(kind 由宿主加前缀)。
			SessionCurrent: sessionCurrent,
			SessionAppend: func(sctx context.Context, kind, dataJSON string) (uint64, error) {
				return sessionAppend(p.opts.Name, sctx, kind, dataJSON)
			},
			// ada:session 的只读事件接口:history 分页,recent 取最近 N 条。
			SessionHistory: sessionHistory,
			SessionRecent:  sessionRecent,
			// ctx.store / ada:store:插件私有、进程内 KV。
			StoreGet:    store.get,
			StoreSet:    store.set,
			StoreDelete: store.del,
			StoreList:   store.list,
			Tool: func(name, summary string, input map[string]any, readOnly bool, timeout time.Duration, invoke func(ctx context.Context, args any) (any, error)) error {
				if toolsReg == nil {
					return fmt.Errorf("tools 服务不可用")
				}
				if timeout <= 0 {
					timeout = 30 * time.Second
				}
				def := tools.ToolDef{
					Name: name, Summary: summary, ReadOnly: readOnly,
					Timeout: timeout, Source: p.opts.Name,
					Input: schema.FromMap(input),
					Handler: func(tctx context.Context, call tools.CallInfo) (tools.Result, error) {
						var args any
						if len(call.Args) > 0 {
							if err := json.Unmarshal(call.Args, &args); err != nil {
								return tools.Result{Status: tools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: "参数解析失败: " + err.Error()}}}, nil
							}
						}
						out, err := invoke(tctx, args)
						if err != nil {
							return tools.Result{Status: tools.StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: err.Error()}}}, nil
						}
						// 显式返回 {status, content, details, terminate} 的脚本工具按结构化结果处理;
						// 其余返回值保持旧契约:字符串原样,对象转 JSON。
						if res, ok := toolResultFromJS(out); ok {
							return res, nil
						}
						text, ok := out.(string)
						if !ok {
							b, _ := json.Marshal(out)
							text = string(b)
						}
						return tools.Result{Status: tools.StatusOK, Content: []llm.Block{{Kind: llm.BlockText, Text: text}}}, nil
					},
				}
				return stage.tool(def)
			},
		},
	})
	if err != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "JS 插件 %s 装配失败", p.opts.Name)
	}

	// AGENTS §6.2 的入口约定:apply(ctx, config)。
	res, err := rt.CallWithHost(actx, "apply", cfg.Raw())
	if err != nil {
		stage.close()
		// apply 失败前登记的 effect 也要清理:它们可能已经建了外部资源。
		runEffects(p.opts.Name, stage.pendingEffects(), context.Background())
		_ = rt.Close(context.Background())
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "JS 插件 %s 的 apply 失败", p.opts.Name)
	}

	waterfalls, ons, effects, provides, emits, toolDefs := stage.drain()

	// waterfall 先于普通监听器注册:中间件通常要包住整条链的其余部分。
	for _, req := range waterfalls {
		req := req
		ctx.OnWaterfall(req.event, func(lctx context.Context, _ *kernel.Context, input any, next kernel.Next) (any, error) {
			return req.invoke(lctx, input, next)
		})
	}

	// 提交顺序 = JS 调用顺序语义:先挂监听器,再注册服务,最后派发事件。
	// 反过来会让 JS 在 apply 里 emit 的事件打不到自己刚注册的处理器。
	for _, req := range ons {
		req := req
		ctx.On(req.event, func(lctx context.Context, _ *kernel.Context, args []any) (any, error) {
			var payload any
			switch len(args) {
			case 0:
			case 1:
				payload = args[0]
			default:
				payload = args
			}
			if _, err := req.invoke(lctx, payload); err != nil {
				return nil, err
			}
			return nil, nil
		})
	}
	for _, req := range provides {
		value := any(req.value)
		if req.name == control.ServiceName {
			wrapped, werr := wrapControlService(req.value)
			if werr != nil {
				runEffects(p.opts.Name, effects, context.Background())
				_ = rt.Close(context.Background())
				return nil, kernel.Wrap(kernel.CodeInvalid, werr, "JS 插件 %s 提供 control 服务失败", p.opts.Name)
			}
			value = wrapped
		}
		if _, perr := ctx.Provide(req.name, value); perr != nil {
			runEffects(p.opts.Name, effects, context.Background())
			_ = rt.Close(context.Background())
			return nil, kernel.Wrap(kernel.CodeInvalid, perr, "JS 插件 %s 提供服务 %q 失败", p.opts.Name, req.name)
		}
	}
	var toolDisposers []kernel.Dispose
	for _, req := range toolDefs {
		d, terr := toolsReg.Register(req.def)
		if terr != nil {
			for _, td := range toolDisposers {
				_ = td(context.Background())
			}
			runEffects(p.opts.Name, effects, context.Background())
			_ = rt.Close(context.Background())
			return nil, kernel.Wrap(kernel.CodeInvalid, terr, "JS 插件 %s 注册工具 %q 失败", p.opts.Name, req.def.Name)
		}
		toolDisposers = append(toolDisposers, d)
	}
	for _, req := range emits {
		ctx.EmitContext(actx, req.event, req.payload)
	}

	p.mu.Lock()
	p.rt = rt
	p.kctx = ctx
	p.mu.Unlock()

	ctx.Log().Info("JS 插件已装配",
		"exports", rt.Exports(), "ons", len(ons), "waterfalls", len(waterfalls), "effects", len(effects),
		"provides", len(provides), "emits", len(emits), "tools", len(toolDefs), "result", res)
	return func(dctx context.Context) error {
		p.mu.Lock()
		r := p.rt
		p.rt = nil
		p.kctx = nil
		p.mu.Unlock()
		if r == nil {
			return nil
		}
		// 工具先注销,再跑 JS 清理,最后关闭运行时。
		var errs []error
		for i := len(toolDisposers) - 1; i >= 0; i-- {
			if err := toolDisposers[i](dctx); err != nil {
				errs = append(errs, err)
			}
		}
		errs = append(errs, runEffects(p.opts.Name, effects, dctx)...)
		if cerr := r.Close(dctx); cerr != nil {
			errs = append(errs, cerr)
		}
		return errors.Join(errs...)
	}, nil
}

// Logs 返回 JS 侧 ctx.log 的输出(测试与诊断用)。
func (p *Plugin) Logs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.logs))
	copy(out, p.logs)
	return out
}

// emitTo 处理 ctx.emit:apply 期间进暂存队列;运行期直接派发。
//
// 为什么运行期可以直接派发:事件是广播,总线自带锁、每个监听器有独立超时;
// 而 provide/on/effect/waterfall 会改服务表与监听器表,仍只允许在 apply 期间提交。
func (p *Plugin) emitTo(stage *stager, event string, payload any) error {
	if err := stage.emit(event, payload); err == nil {
		return nil
	}
	p.mu.Lock()
	ctx := p.kctx
	p.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("ctx.emit: 插件已卸载,事件未派发")
	}
	ctx.EmitContext(context.Background(), event, payload)
	return nil
}

// maxShellExecBytes 是单次 shell.exec 跨 JSON 边界的输出上限(stdout+stderr)。
const maxShellExecBytes = 256 << 10

// shellExecResult 是 ada:shell.exec 的结构化返回值。
//
// 字段名与 shell-helper 的 Details 保持接近;Mode/Backend 为空时表示
// 当前 shell 实现没有从 sandbox 回填 Facts(已知边界)。
type shellExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"durationMs"`
	Enforced   bool   `json:"enforced"`
	Backend    string `json:"backend,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

// toolResultFromJS 识别脚本工具返回的结构化结果:
// {status, content, details, terminate}。没有 status 的对象仍按旧契约转 JSON 文本。
func toolResultFromJS(out any) (tools.Result, bool) {
	m, ok := out.(map[string]any)
	if !ok {
		return tools.Result{}, false
	}
	status, ok := m["status"].(string)
	if !ok || status == "" {
		return tools.Result{}, false
	}
	res := tools.Result{Status: tools.Status(status), Content: blocksFromJS(m["content"])}
	if d, ok := m["details"]; ok && d != nil {
		if b, err := json.Marshal(d); err == nil {
			res.Details = b
		}
	}
	if t, ok := m["terminate"].(bool); ok {
		res.Terminate = t
	}
	return res, true
}

// blocksFromJS 把脚本工具返回的 content 转成模型消息块。
func blocksFromJS(v any) []llm.Block {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []llm.Block{{Kind: llm.BlockText, Text: x}}
	case []any:
		out := make([]llm.Block, 0, len(x))
		for _, item := range x {
			switch it := item.(type) {
			case string:
				if it != "" {
					out = append(out, llm.Block{Kind: llm.BlockText, Text: it})
				}
			case map[string]any:
				kind := llm.BlockText
				if k, ok := it["kind"].(string); ok && k != "" {
					kind = llm.BlockKind(k)
				}
				text, _ := it["text"].(string)
				out = append(out, llm.Block{Kind: kind, Text: text})
			}
		}
		return out
	}
	return nil
}

func (p *Plugin) record(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p.mu.Lock()
	p.logs = append(p.logs, msg)
	p.mu.Unlock()
	if p.opts.Logf != nil {
		p.opts.Logf("%s", msg)
	}
}
