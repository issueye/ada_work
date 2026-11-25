// Package tools 是 `seam/tools` 的契约层:工具定义、调用管线与注册表。
//
// 审批/超时策略不在这里:Guard 是同步硬拒绝,审批属于 policy(seam-api §3.4)。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/kernel/schema"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// Status 是工具结果状态。
type Status string

const (
	StatusOK      Status = "ok"
	StatusDenied  Status = "denied"
	StatusError   Status = "error"
	StatusTimeout Status = "timeout"
)

// Handler 是工具实现。
type Handler func(ctx context.Context, call CallInfo) (Result, error)

// ToolDef 是一个工具的定义;Handler 不进模型请求,json 忽略。
type ToolDef struct {
	Name     string         `json:"name"`
	Summary  string         `json:"summary,omitempty"`
	Input    *schema.Schema `json:"-"`
	Output   *schema.Schema `json:"-"`
	ReadOnly bool           `json:"readOnly,omitempty"`
	Timeout  time.Duration  `json:"timeout,omitempty"`
	Source   string         `json:"source,omitempty"`
	Tags     []string       `json:"tags,omitempty"`
	Handler  Handler        `json:"-"`
}

// ToolInfo 是 List 的返回项。
type ToolInfo struct {
	ToolDef
	Visible bool `json:"visible"`
}

// CallInfo 是一次调用。
type CallInfo struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	SessionID string          `json:"sessionId,omitempty"`
	TurnID    string          `json:"turnId,omitempty"`
	StepID    string          `json:"stepId,omitempty"`
	Origin    string          `json:"origin,omitempty"`
}

// Result 是工具结果;Content 会回到模型,Details 只给 UI。
type Result struct {
	Content   []llm.Block     `json:"content"`
	Details   json.RawMessage `json:"details,omitempty"`
	Status    Status          `json:"status"`
	Terminate bool            `json:"terminate,omitempty"`
	Usage     *llm.Usage      `json:"usage,omitempty"`
}

// Guard 是同步拒绝:返回错误即拒绝,与注册顺序无关。
type Guard func(ctx context.Context, call CallInfo) error

// Tools 是工具契约。
type Tools interface {
	Register(def ToolDef) (kernel.Dispose, error)
	List(ctx context.Context) ([]ToolInfo, error)
	Restrict(names ...string) (kernel.Dispose, error)
	Guard(fn Guard) (kernel.Dispose, error)
	Invoke(ctx context.Context, call CallInfo) (Result, error)
}

// 事件:前三个是 waterfall,最后一个是观察点。
const (
	EventPreExecute  = "tools/pre-execute"
	EventExecute     = "tools/execute"
	EventPostExecute = "tools/post-execute"
	EventResult      = "tools/result"
)

// Registry 是 seam 插件提供的服务值。
type Registry struct {
	mu       sync.Mutex
	seq      uint64
	tools    map[string]ToolDef
	restrict map[string]int
	guards   []guardEntry
	bus      *kernel.Context
}

type guardEntry struct {
	id uint64
	fn Guard
}

// NewRegistry 构造注册表;bus 可为 nil(单元测试/无事件场景)。
func NewRegistry(bus *kernel.Context) *Registry {
	return &Registry{tools: map[string]ToolDef{}, restrict: map[string]int{}, bus: bus}
}

// Register 注册工具;同名重复失败,disposer 后消失。
func (r *Registry) Register(def ToolDef) (kernel.Dispose, error) {
	if def.Name == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "tools", "工具名不能为空", false)
	}
	if def.Handler == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "tools", "工具 "+def.Name+" 缺少 Handler", false)
	}
	r.mu.Lock()
	if _, ok := r.tools[def.Name]; ok {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, "tools", "工具已存在: "+def.Name, false)
	}
	r.tools[def.Name] = def
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		delete(r.tools, def.Name)
		r.mu.Unlock()
		return nil
	}, nil
}

// List 返回未被 restrict 的工具。
func (r *Registry) List(ctx context.Context) ([]ToolInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.Wrap(seamerr.ErrCanceled, "tools", "列工具前已取消", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolInfo, 0, len(r.tools))
	for name, def := range r.tools {
		if r.restrict[name] > 0 {
			continue
		}
		out = append(out, ToolInfo{ToolDef: def, Visible: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Restrict 隐藏一组工具;disposer 只撤销自己这一次的隐藏。
func (r *Registry) Restrict(names ...string) (kernel.Dispose, error) {
	r.mu.Lock()
	for _, n := range names {
		r.restrict[n]++
	}
	r.mu.Unlock()
	var once sync.Once
	return func(context.Context) error {
		once.Do(func() {
			r.mu.Lock()
			for _, n := range names {
				if r.restrict[n] > 1 {
					r.restrict[n]--
				} else {
					delete(r.restrict, n)
				}
			}
			r.mu.Unlock()
		})
		return nil
	}, nil
}

// Guard 注册同步拒绝;返回 disposer。
func (r *Registry) Guard(fn Guard) (kernel.Dispose, error) {
	if fn == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "tools", "guard 不能为 nil", false)
	}
	r.mu.Lock()
	r.seq++
	entry := guardEntry{id: r.seq, fn: fn}
	r.guards = append(r.guards, entry)
	r.mu.Unlock()
	var once sync.Once
	return func(context.Context) error {
		once.Do(func() {
			r.mu.Lock()
			for i := range r.guards {
				if r.guards[i].id == entry.id {
					r.guards = append(r.guards[:i], r.guards[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
		})
		return nil
	}, nil
}

// Invoke 执行工具:可见性 → guard → pre → schema → handler → post → 观察。
func (r *Registry) Invoke(ctx context.Context, call CallInfo) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, seamerr.Wrap(seamerr.ErrCanceled, "tools", "调用前已取消", err)
	}
	r.mu.Lock()
	def, ok := r.tools[call.Name]
	restricted := r.restrict[call.Name] > 0
	guards := append([]guardEntry(nil), r.guards...)
	bus := r.bus
	r.mu.Unlock()

	if !ok {
		return Result{}, seamerr.New(seamerr.ErrInvalid, "tools", "未知工具: "+call.Name, false)
	}
	if restricted {
		return r.finish(ctx, bus, call, denied("工具已被 restrict: "+call.Name))
	}
	for _, g := range guards {
		if err := g.fn(ctx, call); err != nil {
			return r.finish(ctx, bus, call, denied(err.Error()))
		}
	}

	if bus != nil {
		v, err := bus.Waterfall(ctx, EventPreExecute, &call)
		if err != nil {
			return r.finish(ctx, bus, call, failed(StatusDenied, err))
		}
		if c, ok := v.(*CallInfo); ok && c != nil {
			call = *c
		}
	}

	if def.Input != nil {
		if err := validateArgs(def.Input, call.Args); err != nil {
			return r.finish(ctx, bus, call, failed(StatusError, err))
		}
	}

	run := Execution{Call: &call}
	if bus != nil {
		v, werr := bus.Waterfall(ctx, EventExecute, &run)
		if werr != nil {
			return r.finish(ctx, bus, call, failed(StatusError, werr))
		}
		if e, ok := v.(*Execution); ok && e != nil && e.Done {
			run = *e
		}
	}
	if !run.Done {
		res, err := run.execute(ctx, def)
		if err != nil {
			return r.finish(ctx, bus, call, failed(StatusError, err))
		}
		run.Result = res
	}

	res := run.Result
	if bus != nil {
		v, err := bus.Waterfall(ctx, EventPostExecute, &res)
		if err != nil {
			return r.finish(ctx, bus, call, failed(StatusError, err))
		}
		if p, ok := v.(*Result); ok && p != nil {
			res = *p
		}
	}
	return r.finish(ctx, bus, call, res)
}

// Execution 是 tools/execute waterfall 的输入:监听者可改写 Call,
// 或设置 Done+Result 直接替换执行(例如缓存/权限桩)。
type Execution struct {
	Call    *CallInfo
	Result  Result
	Done    bool
	Timeout time.Duration // >0 时覆盖 ToolDef.Timeout(policy 可收紧/放宽)
}

// execute 执行实际工具,并强制 Timeout(默认 30s)与 panic 隔离。
func (e *Execution) execute(ctx context.Context, def ToolDef) (Result, error) {
	if e == nil || e.Call == nil {
		return Result{}, fmt.Errorf("tools: 执行请求为空")
	}
	timeout := def.Timeout
	if e.Timeout > 0 {
		timeout = e.Timeout
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan struct {
		res Result
		err error
	}, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- struct {
					res Result
					err error
				}{Result{Status: StatusError, Content: []llm.Block{{Kind: llm.BlockText, Text: fmt.Sprintf("工具 panic: %v", rec)}}}, nil}
			}
		}()
		res, err := def.Handler(actx, *e.Call)
		done <- struct {
			res Result
			err error
		}{res, err}
	}()
	select {
	case r := <-done:
		return r.res, r.err
	case <-actx.Done():
		return Result{Status: StatusTimeout, Content: []llm.Block{{Kind: llm.BlockText, Text: "工具超时: " + def.Name}}}, nil
	}
}

func (r *Registry) finish(ctx context.Context, bus *kernel.Context, call CallInfo, res Result) (Result, error) {
	if res.Status == "" {
		res.Status = StatusOK
	}
	if bus != nil {
		payload := map[string]any{"call": call, "result": res}
		bus.EmitContext(ctx, EventResult, payload)
	}
	return res, nil
}

func denied(msg string) Result {
	return Result{Status: StatusDenied, Content: []llm.Block{{Kind: llm.BlockText, Text: msg}}}
}

func failed(status Status, err error) Result {
	return Result{Status: status, Content: []llm.Block{{Kind: llm.BlockText, Text: err.Error()}}}
}

func validateArgs(s *schema.Schema, raw json.RawMessage) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return seamerr.New(seamerr.ErrSchema, "tools", "参数不是合法 JSON: "+err.Error(), false)
	}
	if err := s.Validate(v); err != nil {
		return seamerr.Wrap(seamerr.ErrSchema, "tools", "参数不符合 schema", err)
	}
	return nil
}

const pluginID = "seam-tools"

// Plugin 返回 seam/tools 插件:提供 tools 服务(Registry)。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("tools", NewRegistry(ctx), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("tools")
}
