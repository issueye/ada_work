// Package flow 是 M4 的声明式流水线:L2 基础设施(不是插件)。
//
// 它把「先调谁、再调谁、失败怎么办」变成**数据**,从而可被配置、可被审计、
// 可被替换;真正的调用仍走内核的服务与事件(同一预算/授权/审计)。
//
// 已实现:顺序执行、when 受限表达式、retry、timeout、checkpoint/resume、
// OnFailure、parallel、forEach、Compensate、subflow;fanout 见 docs/m4-flow.md。
package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/expr"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/sched"
)

// 错误码。
const (
	// CodeFlow 是流水线定义/执行失败。
	CodeFlow = "EFLOW"
	// CodeAborted 是步骤主动否决(when + abort)。
	CodeAborted = "EABORTED"
)

// Runtime 是流水线执行时用到的两个能力:调服务、派发事件。
//
// 抽象成接口是为了两件事:测试可以用假实现;宿主可以换成受限实现
// (例如只允许白名单服务),而不改变 Flow 的数据。
type Runtime interface {
	CallService(ctx context.Context, service, method string, args map[string]any) (any, error)
	DispatchEvent(ctx context.Context, name, mode string, input any) (any, error)
}

// Invoker 让一个服务支持「按方法名调用」。
//
// use 步骤需要 (service, method) 这种通用形态;Go 服务的类型是静态的,
// 所以由服务自己实现这个接口来声明「哪些方法可被流水线调用」。
type Invoker interface {
	Invoke(ctx context.Context, method string, args map[string]any) (any, error)
}

// Duration 是 JSON 友好的时长("500ms"、"2s");零值表示不限制。
type Duration time.Duration

// UnmarshalJSON 接受字符串时长(如 "120s")。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("flow: 时长必须是字符串: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("flow: 非法时长 %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON 输出字符串时长。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Step 是流水线的一步。use / event / abort 三者互斥。
type Step struct {
	ID         string        `json:"id"`
	Use        *UseStep      `json:"use,omitempty"`
	Event      *EventStep    `json:"event,omitempty"`
	Parallel   *ParallelStep `json:"parallel,omitempty"`
	Fanout     *ParallelStep `json:"fanout,omitempty"`
	ForEach    *ForEachStep  `json:"forEach,omitempty"`
	Subflow    *SubflowStep  `json:"subflow,omitempty"`
	Abort      string        `json:"abort,omitempty"`
	When       string        `json:"when,omitempty"`
	Retry      *Retry        `json:"retry,omitempty"`
	Timeout    Duration      `json:"timeout,omitempty"`
	Checkpoint bool          `json:"checkpoint,omitempty"`
}

// UseStep 调用某个服务的方法。Args 里的字符串 "$.step.field" 会解析成上一步的结果。
type UseStep struct {
	Service string         `json:"service"`
	Method  string         `json:"method"`
	Args    map[string]any `json:"args,omitempty"`
}

// EventStep 走事件链。
type EventStep struct {
	Name  string `json:"name"`
	Mode  string `json:"mode,omitempty"` // emit(默认) | waterfall
	Input any    `json:"input,omitempty"`
}

// Retry 是步骤级重试:Max 是「重试次数」(总尝试 = Max+1)。
type Retry struct {
	Max     int      `json:"max"`
	Backoff Duration `json:"backoff,omitempty"`
}

// ParallelStep 是并发分支。
//
// Mode:abort(默认,任一分支失败即取消其余并让流线失败)或 collect(等待全部分支,
// 把错误记入结果,流线继续)。
type ParallelStep struct {
	Branches []Step `json:"branches"`
	Mode     string `json:"mode,omitempty"`
}

// ForEachStep 对数组逐项执行模板步骤,带并发上限。
//
// Over 必须是 "$.path" 引用;As 是每项注入值表的变量名(默认 item);
// 结果按索引收集到父步骤的 {ok, results}。
type ForEachStep struct {
	Over        string `json:"over"`
	As          string `json:"as,omitempty"`
	Step        Step   `json:"step"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// SubflowStep 引用另一个 flow 文件(相对当前 flow 文件所在目录)。
//
// Input 里的 "$.path" 会先按父值表解析,作为子 flow 的初始值表。
type SubflowStep struct {
	Path  string         `json:"path"`
	Input map[string]any `json:"input,omitempty"`
}

// MaxSubflowDepth 是子流线嵌套上限,防止数据里写出无限递归。
const MaxSubflowDepth = 10

// Flow 是一条流水线。
type Flow struct {
	Name       string `json:"flow"`
	Steps      []Step `json:"steps"`
	OnFailure  []Step `json:"onFailure,omitempty"`
	Compensate []Step `json:"compensate,omitempty"`

	// Dir 是 flow 文件所在目录(Load 填写),subflow 的相对路径据此解析。
	Dir string `json:"-"`
}

// New 创建一条空流水线。
func New(name string) *Flow { return &Flow{Name: name} }

// Add 追加步骤,便于链式构造。
func (f *Flow) Add(steps ...Step) *Flow {
	f.Steps = append(f.Steps, steps...)
	return f
}

// Parse 解析声明式 JSON。未知字段一律报错:流线是数据契约,拼错必须暴露。
func Parse(data []byte) (*Flow, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f Flow
	if err := dec.Decode(&f); err != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "flow: 解析失败")
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Load 从文件读取 flow,并记录目录用于 subflow 的相对路径解析。
func Load(path string) (*Flow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "flow: 读取 %s 失败", path)
	}
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	f.Dir = filepath.Dir(path)
	return f, nil
}

// Validate 校验流线定义。
func (f *Flow) Validate() error {
	if f.Name == "" {
		return kernel.Fail(kernel.CodeInvalid, "flow: 缺少 flow 名称")
	}
	if len(f.Steps) == 0 {
		return kernel.Fail(kernel.CodeInvalid, "flow %q: steps 不能为空", f.Name)
	}
	ids := map[string]bool{}
	if err := validateSteps(f.Steps, ids, f.Name); err != nil {
		return err
	}
	if err := validateSteps(f.OnFailure, ids, f.Name); err != nil {
		return err
	}
	return validateSteps(f.Compensate, ids, f.Name)
}

func validateSteps(steps []Step, ids map[string]bool, flowName string) error {
	for _, s := range steps {
		if ids[s.ID] {
			return kernel.Fail(kernel.CodeInvalid, "flow %q: 步骤 id %q 重复", flowName, s.ID)
		}
		if err := validateStep(s); err != nil {
			return err
		}
		ids[s.ID] = true
		if s.Parallel != nil {
			if err := validateSteps(s.Parallel.Branches, ids, flowName); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateParallel(p *ParallelStep, id string) error {
	if p == nil {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 parallel/fanout 不能为空", id)
	}
	if len(p.Branches) == 0 {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 parallel/fanout 至少需要一个分支", id)
	}
	if p.Mode != "" && p.Mode != "abort" && p.Mode != "collect" {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 parallel/fanout.mode 必须是 abort 或 collect", id)
	}
	return nil
}

func validateStep(s Step) error {
	if s.ID == "" {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤缺少 id")
	}
	n := 0
	if s.Use != nil {
		n++
	}
	if s.Event != nil {
		n++
	}
	if s.Abort != "" {
		n++
	}
	if s.Parallel != nil {
		n++
		if err := validateParallel(s.Parallel, s.ID); err != nil {
			return err
		}
	}
	if s.Fanout != nil {
		n++
		if err := validateParallel(s.Fanout, s.ID); err != nil {
			return err
		}
	}
	if s.ForEach != nil {
		n++
		if s.ForEach.Step.ID == "" {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 forEach.step 缺少 id", s.ID)
		}
		if s.ForEach.Over == "" || !strings.HasPrefix(s.ForEach.Over, "$.") {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 forEach.over 必须是 $.path", s.ID)
		}
		if s.ForEach.Concurrency < 0 || s.ForEach.Concurrency > 32 {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 forEach.concurrency 必须在 0..32", s.ID)
		}
		if s.ForEach.Step.ForEach != nil {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 forEach 模板不能再是 forEach", s.ID)
		}
		if s.ForEach.Step.Parallel != nil || s.ForEach.Step.Use != nil || s.ForEach.Step.Event != nil || s.ForEach.Step.Abort != "" {
			if err := validateStep(s.ForEach.Step); err != nil {
				return err
			}
		}
	}
	if s.Subflow != nil {
		n++
		if s.Subflow.Path == "" {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 subflow 缺少 path", s.ID)
		}
		// 跨平台拒绝绝对路径:POSIX 的 "/x"、Windows 盘符/UNC 与反斜杠混用都不接受。
		if strings.HasPrefix(s.Subflow.Path, "/") || strings.Contains(s.Subflow.Path, `\`) || filepath.IsAbs(s.Subflow.Path) {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 subflow 只允许相对路径: %s", s.ID, s.Subflow.Path)
		}
		clean := filepath.Clean(s.Subflow.Path)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 subflow 路径不能逃逸 flow 目录: %s", s.ID, s.Subflow.Path)
		}
	}
	if n != 1 {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 必须且只能有 use/event/parallel/ fanout/forEach/subflow/abort 之一", s.ID)
	}
	if s.Use != nil {
		if s.Use.Service == "" || s.Use.Method == "" {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 use 需要 service 与 method", s.ID)
		}
	}
	if s.Event != nil {
		if s.Event.Name == "" {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 event 需要 name", s.ID)
		}
		if s.Event.Mode != "" && s.Event.Mode != "emit" && s.Event.Mode != "waterfall" {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的事件模式必须是 emit 或 waterfall", s.ID)
		}
	}
	if s.Retry != nil && s.Retry.Max < 0 {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 retry.max 不能为负", s.ID)
	}
	if s.When != "" {
		if err := expr.Validate(s.When); err != nil {
			return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 when 非法: %v", s.ID, err)
		}
	}
	return nil
}

// CheckpointStore 是检查点存储(设计 §12.2:落 ctx.store;这里抽象成接口,便于测试)。
type CheckpointStore interface {
	Save(ctx context.Context, runID string, data []byte) error
	Load(ctx context.Context, runID string) ([]byte, error)
	Delete(ctx context.Context, runID string) error
}

// MemoryStore 是内存检查点存储(测试与最小宿主使用)。
type MemoryStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemoryStore 创建内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string][]byte)}
}

// Save 写入检查点。
func (m *MemoryStore) Save(_ context.Context, runID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[runID] = append([]byte(nil), data...)
	return nil
}

// Load 读取检查点;不存在时返回 nil,nil。
func (m *MemoryStore) Load(_ context.Context, runID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[runID], nil
}

// Delete 删除检查点。
func (m *MemoryStore) Delete(_ context.Context, runID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, runID)
	return nil
}

// Options 控制一次运行。
type Options struct {
	// Store + RunID 一起给出时启用检查点/恢复;两者缺一即不启用。
	Store CheckpointStore
	RunID string
	// Clock 用于 retry 的 backoff;nil 表示真实时钟。
	Clock sched.Clock
}

// Result 是一次运行的结果。
type Result struct {
	Name      string
	Values    map[string]any
	Completed []string
	Skipped   []string
}

type checkpoint struct {
	Flow      string         `json:"flow"`
	Completed []string       `json:"completed"`
	Values    map[string]any `json:"values"`
}

type runState struct {
	flow      *Flow
	rt        Runtime
	clock     sched.Clock
	values    map[string]any
	completed map[string]bool
	saved     map[string]bool
	skipped   []string
	stepIDs   map[string]bool
	stack     []string
}

// Run 顺序执行流水线。
func (f *Flow) Run(ctx context.Context, rt Runtime, opts Options) (*Result, error) {
	return f.runWithStack(ctx, rt, opts, nil, nil)
}

// runWithStack 是 Run 的内部实现:stack 记录 subflow 的绝对路径(环检测),
// seed 是子流线的初始值表。
func (f *Flow) runWithStack(ctx context.Context, rt Runtime, opts Options, stack []string, seed map[string]any) (*Result, error) {
	if rt == nil {
		return nil, kernel.Fail(kernel.CodeInvalid, "flow %q: Runtime 不能为 nil", f.Name)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	clock := opts.Clock
	if clock == nil {
		clock = sched.RealClock{}
	}
	st := &runState{
		flow: f, rt: rt, clock: clock,
		values:    make(map[string]any),
		completed: make(map[string]bool),
		saved:     make(map[string]bool),
		stepIDs:   make(map[string]bool),
		stack:     stack,
	}
	for k, v := range seed {
		st.values[k] = v
	}
	for _, s := range f.Steps {
		st.stepIDs[s.ID] = true
	}

	resume, err := st.restore(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, id := range resume {
		st.completed[id] = true
	}

	for _, step := range f.Steps {
		if st.completed[step.ID] {
			continue
		}
		if err := st.runStep(ctx, step, true); err != nil {
			st.runFailureSteps(ctx)
			st.runCompensateSteps(ctx)
			return &Result{Name: f.Name, Values: st.values, Completed: st.completedList(), Skipped: st.skipped}, err
		}
		// 检查点只在标记了 checkpoint 的步骤后写;写失败按运行失败处理(不能假装成功)。
		if err := st.save(ctx, opts, step); err != nil {
			st.runFailureSteps(ctx)
			return &Result{Name: f.Name, Values: st.values, Completed: st.completedList(), Skipped: st.skipped}, err
		}
	}

	return &Result{Name: f.Name, Values: st.values, Completed: st.completedList(), Skipped: st.skipped}, nil
}

func (st *runState) restore(ctx context.Context, opts Options) ([]string, error) {
	if opts.Store == nil || opts.RunID == "" {
		return nil, nil
	}
	data, err := opts.Store.Load(ctx, opts.RunID)
	if err != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "flow: 读取检查点失败")
	}
	if len(data) == 0 {
		return nil, nil
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, kernel.Wrap(kernel.CodeInvalid, err, "flow: 检查点损坏")
	}
	if cp.Flow != st.flow.Name {
		return nil, kernel.Fail(kernel.CodeInvalid,
			"flow: 检查点属于流线 %q,当前是 %q", cp.Flow, st.flow.Name)
	}
	for k, v := range cp.Values {
		st.values[k] = v
	}
	for _, id := range cp.Completed {
		if !st.stepIDs[id] {
			return nil, kernel.Fail(kernel.CodeInvalid, "flow: 检查点里的步骤 %q 不在当前流线中", id)
		}
	}
	return cp.Completed, nil
}

func (st *runState) save(ctx context.Context, opts Options, step Step) error {
	if opts.Store == nil || opts.RunID == "" || !step.Checkpoint {
		return nil
	}
	cp := checkpoint{Flow: st.flow.Name, Completed: st.completedList(), Values: st.values}
	data, err := json.Marshal(cp)
	if err != nil {
		return kernel.Wrap(CodeFlow, err, "flow: 序列化检查点失败")
	}
	if err := opts.Store.Save(ctx, opts.RunID, data); err != nil {
		return kernel.Wrap(CodeFlow, err, "flow: 写入检查点失败")
	}
	return nil
}

func (st *runState) completedList() []string {
	out := make([]string, 0, len(st.completed))
	for id := range st.completed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (st *runState) runStep(ctx context.Context, step Step, record bool) error {
	if step.When != "" {
		ok, err := Eval(step.When, st.values)
		if err != nil {
			return kernel.Wrap(kernel.CodeInvalid, err, "flow: 步骤 %q 的 when 求值失败", step.ID)
		}
		if !ok {
			if record {
				st.skipped = append(st.skipped, step.ID)
				st.completed[step.ID] = true
			}
			return nil
		}
	}

	if step.Abort != "" {
		return kernel.Fail(CodeAborted, "flow %q: 步骤 %q 主动否决: %s", st.flow.Name, step.ID, step.Abort)
	}

	var err error
	switch {
	case step.Use != nil:
		err = st.execWithRetry(ctx, step, func(ctx context.Context) error {
			args, aerr := resolveArgs(step.Use.Args, st.values)
			if aerr != nil {
				return aerr
			}
			value, callErr := st.rt.CallService(ctx, step.Use.Service, step.Use.Method, args)
			if callErr != nil {
				return callErr
			}
			st.values[step.ID] = value
			return nil
		})
	case step.Event != nil:
		err = st.execWithRetry(ctx, step, func(ctx context.Context) error {
			input, aerr := resolveValue(step.Event.Input, st.values)
			if aerr != nil {
				return aerr
			}
			value, dispatchErr := st.rt.DispatchEvent(ctx, step.Event.Name, step.Event.Mode, input)
			if dispatchErr != nil {
				return dispatchErr
			}
			st.values[step.ID] = value
			return nil
		})
	case step.Parallel != nil:
		err = st.runParallel(ctx, step)
	case step.Fanout != nil:
		err = st.runFanout(ctx, step)
	case step.ForEach != nil:
		err = st.runForEach(ctx, step)
	case step.Subflow != nil:
		err = st.execWithRetry(ctx, step, func(ctx context.Context) error {
			value, serr := st.runSubflow(ctx, step)
			if serr != nil {
				return serr
			}
			st.values[step.ID] = value
			return nil
		})
	}
	if err != nil {
		return err
	}
	if record {
		st.completed[step.ID] = true
	}
	return nil
}

type branchOutcome struct {
	values    map[string]any
	completed map[string]bool
	err       error
}

// child 为并发分支复制一份独立的值表与完成集合。
//
// 值表是浅拷贝:分支之间通过「父值快照 + 各自新键」隔离,合并时父键优先不被覆盖。
func (st *runState) child() *runState {
	values := make(map[string]any, len(st.values))
	for k, v := range st.values {
		values[k] = v
	}
	return &runState{
		flow: st.flow, rt: st.rt, clock: st.clock,
		values: values, completed: map[string]bool{}, saved: map[string]bool{},
		stepIDs: st.stepIDs,
	}
}

// runFanout 是 parallel 的别名:fanout 的语义就是「并发分支 + join」。
func (st *runState) runFanout(ctx context.Context, step Step) error {
	step.Parallel = step.Fanout
	return st.runParallel(ctx, step)
}

// runParallel 并发执行分支;abort 模式首个失败即取消其余,collect 模式等待全部。
func (st *runState) runParallel(ctx context.Context, step Step) error {
	p := step.Parallel
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	outs := make([]branchOutcome, len(p.Branches))
	var wg sync.WaitGroup
	for i, br := range p.Branches {
		wg.Add(1)
		go func(i int, br Step) {
			defer wg.Done()
			sub := st.child()
			err := sub.runStep(ctx2, br, true)
			outs[i] = branchOutcome{values: sub.values, completed: sub.completed, err: err}
			if err != nil && p.Mode != "collect" {
				cancel()
			}
		}(i, br)
	}
	wg.Wait()

	var errs []string
	for i, br := range p.Branches {
		o := outs[i]
		if o.err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", br.ID, o.err))
			continue
		}
		for k, v := range o.values {
			if _, exists := st.values[k]; !exists {
				st.values[k] = v
			}
		}
		for id := range o.completed {
			st.completed[id] = true
		}
	}
	ok := len(errs) == 0
	st.values[step.ID] = map[string]any{"ok": ok, "errors": errs}
	if !ok && p.Mode != "collect" {
		return kernel.Wrap(CodeFlow, fmt.Errorf("%s", strings.Join(errs, "; ")),
			"flow %q: %d 个并行分支失败", st.flow.Name, len(errs))
	}
	return nil
}

func (st *runState) execWithRetry(ctx context.Context, step Step, fn func(context.Context) error) error {
	attempts := 1
	var backoff time.Duration
	if step.Retry != nil {
		if step.Retry.Max > 0 {
			attempts = step.Retry.Max + 1
		}
		backoff = time.Duration(step.Retry.Backoff)
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		runCtx := ctx
		var cancel context.CancelFunc
		if step.Timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(step.Timeout))
		}
		err := fn(runCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return nil
		}
		last = err
		if attempt == attempts-1 {
			break
		}
		if backoff > 0 {
			select {
			case <-st.clock.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return kernel.Wrap(CodeFlow, last, "flow %q: 步骤 %q 失败(尝试 %d 次)", st.flow.Name, step.ID, attempts)
}

// runCompensateSteps 逆序执行补偿步骤(尽力而为,错误记入 __compensateErrors)。
func (st *runState) runCompensateSteps(ctx context.Context) {
	var errs []string
	for i := len(st.flow.Compensate) - 1; i >= 0; i-- {
		step := st.flow.Compensate[i]
		if err := st.runStep(ctx, step, false); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", step.ID, err))
		}
	}
	if len(errs) > 0 {
		st.values["__compensateErrors"] = strings.Join(errs, "; ")
	}
}

// runSubflow 加载并执行子流线:环检测 + 深度上限 + 输入模板解析。
func (st *runState) runSubflow(ctx context.Context, step Step) (any, error) {
	p := step.Subflow
	path := p.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(st.flow.Dir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, kernel.Wrap(CodeFlow, err, "flow: subflow 路径非法: %s", p.Path)
	}
	for _, seen := range st.stack {
		if seen == abs {
			return nil, kernel.Fail(CodeFlow, "flow: subflow 出现环: %s", abs)
		}
	}
	if len(st.stack) >= MaxSubflowDepth {
		return nil, kernel.Fail(CodeFlow, "flow: subflow 嵌套超过上限 %d", MaxSubflowDepth)
	}
	child, err := Load(abs)
	if err != nil {
		return nil, err
	}
	input, err := resolveArgs(p.Input, st.values)
	if err != nil {
		return nil, err
	}
	childStack := append(append([]string(nil), st.stack...), abs)
	// 子流线不继承 checkpoint(避免 RunID 冲突);Clock 继承以保证重试语义一致。
	res, err := child.runWithStack(ctx, st.rt, Options{Clock: st.clock}, childStack, input)
	if err != nil {
		return nil, err
	}
	return res.Values, nil
}

// runForEach 逐项执行模板步骤,按并发上限节流;任一失败即取消全部。
func (st *runState) runForEach(ctx context.Context, step Step) error {
	p := step.ForEach
	itemsAny, err := resolveValue(p.Over, st.values)
	if err != nil {
		return err
	}
	items, ok := itemsAny.([]any)
	if !ok {
		return kernel.Fail(kernel.CodeInvalid, "flow: 步骤 %q 的 forEach.over 不是数组", step.ID)
	}
	as := p.As
	if as == "" {
		as = "item"
	}
	concurrency := p.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, concurrency)
	results := make([]any, len(items))
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i, item := range items {
		wg.Add(1)
		go func(i int, item any) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx2.Done():
				return
			}
			sub := st.child()
			sub.values[as] = item
			if err := sub.runStep(ctx2, p.Step, true); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = kernel.Wrap(CodeFlow, err, "flow %q: forEach 第 %d 项失败", st.flow.Name, i)
				}
				mu.Unlock()
				cancel()
				return
			}
			value := sub.values[p.Step.ID]
			mu.Lock()
			results[i] = value
			mu.Unlock()
		}(i, item)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	st.values[step.ID] = map[string]any{"ok": true, "results": results}
	return nil
}

func (st *runState) runFailureSteps(ctx context.Context) {
	var errs []string
	for _, step := range st.flow.OnFailure {
		if err := st.runStep(ctx, step, false); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", step.ID, err))
		}
	}
	st.values["__failureErrors"] = strings.Join(errs, "; ")
}

// resolveArgs 递归解析参数里的 "$.path" 引用。
func resolveArgs(args map[string]any, values map[string]any) (map[string]any, error) {
	if args == nil {
		return nil, nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		rv, err := resolveValue(v, values)
		if err != nil {
			return nil, err
		}
		out[k] = rv
	}
	return out, nil
}

func resolveValue(v any, values map[string]any) (any, error) {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "$.") {
			return lookupPath(x, values)
		}
		return x, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			rv, err := resolveValue(vv, values)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			rv, err := resolveValue(vv, values)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return v, nil
	}
}
