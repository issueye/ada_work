package flow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/flow"
	"github.com/issueye/ada_go_v2/platform/kernel"
)

// fakeRT 是可脚本化的 Runtime:记录调用,回调决定结果。
type fakeRT struct {
	mu     sync.Mutex
	calls  []string
	events []string
	args   map[string]map[string]any

	callFn  func(ctx context.Context, service, method string, args map[string]any) (any, error)
	eventFn func(ctx context.Context, name, mode string, input any) (any, error)
}

func newFakeRT() *fakeRT { return &fakeRT{args: map[string]map[string]any{}} }

func (f *fakeRT) CallService(ctx context.Context, service, method string, args map[string]any) (any, error) {
	key := service + "." + method
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.args[key] = args
	f.mu.Unlock()
	if f.callFn != nil {
		return f.callFn(ctx, service, method, args)
	}
	return nil, nil
}

func (f *fakeRT) DispatchEvent(ctx context.Context, name, mode string, input any) (any, error) {
	f.mu.Lock()
	f.events = append(f.events, name)
	f.mu.Unlock()
	if f.eventFn != nil {
		return f.eventFn(ctx, name, mode, input)
	}
	return input, nil
}

func (f *fakeRT) callCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func newApp(t *testing.T) *kernel.App {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	return app
}

// 1. 顺序执行:use 与 event,参数用 $.path 引用上一步结果。
func TestRunSequential(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(ctx context.Context, service, method string, args map[string]any) (any, error) {
		switch method {
		case "m1":
			return float64(2), nil
		case "m2":
			if args["v"] != float64(2) {
				t.Errorf("m2 的参数没有解析成上一步结果: %v", args)
			}
			return float64(4), nil
		}
		return nil, nil
	}

	f := flow.New("seq").
		Add(flow.Step{ID: "a", Use: &flow.UseStep{Service: "svc", Method: "m1"}}).
		Add(flow.Step{ID: "b", Use: &flow.UseStep{Service: "svc", Method: "m2", Args: map[string]any{"v": "$.a"}}}).
		Add(flow.Step{ID: "c", Event: &flow.EventStep{Name: "flow/step", Input: "$.b"}})

	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if strings.Join(res.Completed, ",") != "a,b,c" {
		t.Fatalf("完成顺序不正确: %v", res.Completed)
	}
	if res.Values["a"] != float64(2) || res.Values["b"] != float64(4) || res.Values["c"] != float64(4) {
		t.Fatalf("步骤结果不正确: %v", res.Values)
	}
	if len(rt.calls) != 2 || rt.calls[0] != "svc.m1" || rt.calls[1] != "svc.m2" {
		t.Fatalf("调用顺序不正确: %v", rt.calls)
	}
	if len(rt.events) != 1 || rt.events[0] != "flow/step" {
		t.Fatalf("事件不正确: %v", rt.events)
	}
}

// 2. when 为假则跳过;when 为真且声明 abort 则否决。
func TestWhenSkipAndAbort(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(context.Context, string, string, map[string]any) (any, error) { return float64(1), nil }

	f := flow.New("gate").
		Add(flow.Step{ID: "a", Use: &flow.UseStep{Service: "svc", Method: "m"}}).
		Add(flow.Step{ID: "b", When: "$.a == 2", Use: &flow.UseStep{Service: "svc", Method: "m"}}).
		Add(flow.Step{ID: "c", When: "$.a == 1", Abort: "blocked"})

	res, err := f.Run(context.Background(), rt, flow.Options{})
	if !kernel.IsCode(err, kernel.Code(flow.CodeAborted)) {
		t.Fatalf("应返回 EABORTED,实际 %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "b" {
		t.Fatalf("b 应被跳过: %v", res.Skipped)
	}
	if len(rt.calls) != 1 {
		t.Fatalf("被跳过的步骤不应调用服务,实际调用 %d 次", len(rt.calls))
	}
}

// 3. retry:失败两次后成功(总尝试 = max+1)。
func TestRetrySucceeds(t *testing.T) {
	rt := newFakeRT()
	attempts := 0
	rt.callFn = func(context.Context, string, string, map[string]any) (any, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("暂时失败")
		}
		return float64(7), nil
	}
	f := flow.New("retry").
		Add(flow.Step{ID: "a", Retry: &flow.Retry{Max: 2}, Use: &flow.UseStep{Service: "svc", Method: "m"}})

	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("应尝试 3 次,实际 %d", attempts)
	}
	if res.Values["a"] != float64(7) {
		t.Fatalf("结果不正确: %v", res.Values["a"])
	}
}

// 4. retry 用尽后失败,并带上步骤名与尝试次数。
func TestRetryExhausted(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errors.New("一直失败")
	}
	f := flow.New("fail").
		Add(flow.Step{ID: "a", Retry: &flow.Retry{Max: 1}, Use: &flow.UseStep{Service: "svc", Method: "m"}})

	_, err := f.Run(context.Background(), rt, flow.Options{})
	if !kernel.IsCode(err, kernel.Code(flow.CodeFlow)) {
		t.Fatalf("应返回 EFLOW,实际 %v", err)
	}
	if !strings.Contains(err.Error(), "步骤 \"a\"") {
		t.Fatalf("错误应点名步骤: %v", err)
	}
	if got := rt.callCount("svc.m"); got != 2 {
		t.Fatalf("应尝试 2 次,实际 %d", got)
	}
}

// 5. timeout:步骤超时后失败,调用收到 ctx 取消。
func TestStepTimeout(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(ctx context.Context, _ string, _ string, _ map[string]any) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f := flow.New("timeout").
		Add(flow.Step{ID: "a", Timeout: flow.Duration(20 * time.Millisecond),
			Use: &flow.UseStep{Service: "svc", Method: "m"}})

	start := time.Now()
	_, err := f.Run(context.Background(), rt, flow.Options{})
	if err == nil {
		t.Fatalf("超时应失败")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时没有及时返回: %s", elapsed)
	}
}

// 6. checkpoint/resume:第二次运行跳过已检查点的步骤,并保留其状态。
func TestCheckpointResume(t *testing.T) {
	store := flow.NewMemoryStore()
	rt := newFakeRT()
	aCalls := 0
	bAttempts := 0
	rt.callFn = func(_ context.Context, _ string, method string, args map[string]any) (any, error) {
		switch method {
		case "a":
			aCalls++
			return float64(10), nil
		case "b":
			bAttempts++
			if bAttempts == 1 {
				return nil, errors.New("第一次失败")
			}
			if args["v"] != float64(10) {
				t.Errorf("恢复后 b 的参数不正确: %v", args)
			}
			return float64(11), nil
		}
		return nil, nil
	}

	f := flow.New("resume").
		Add(flow.Step{ID: "a", Checkpoint: true, Use: &flow.UseStep{Service: "svc", Method: "a"}}).
		Add(flow.Step{ID: "b", Use: &flow.UseStep{Service: "svc", Method: "b", Args: map[string]any{"v": "$.a"}}})

	opts := flow.Options{Store: store, RunID: "run-1"}
	if _, err := f.Run(context.Background(), rt, opts); err == nil {
		t.Fatalf("第一次运行应失败")
	}
	if aCalls != 1 {
		t.Fatalf("a 应执行一次,实际 %d", aCalls)
	}

	res, err := f.Run(context.Background(), rt, opts)
	if err != nil {
		t.Fatalf("恢复后应成功: %v", err)
	}
	if aCalls != 1 {
		t.Fatalf("已有检查点的步骤不应重跑,实际 %d 次", aCalls)
	}
	if res.Values["a"] != float64(10) || res.Values["b"] != float64(11) {
		t.Fatalf("恢复后的结果不正确: %v", res.Values)
	}
}

// 7. OnFailure:失败后执行补偿步骤,并记录失败信息。
func TestOnFailureSteps(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errors.New("坏了")
	}
	f := flow.New("with-failure").
		Add(flow.Step{ID: "a", Use: &flow.UseStep{Service: "svc", Method: "m"}})
	// OnFailure 是独立字段:主链失败后执行,不进入 completed。
	f.OnFailure = []flow.Step{{ID: "notify", Event: &flow.EventStep{Name: "flow/failed"}}}

	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err == nil {
		t.Fatalf("失败步骤应返回错误")
	}
	if len(rt.events) != 1 || rt.events[0] != "flow/failed" {
		t.Fatalf("OnFailure 事件未派发: %v", rt.events)
	}
	if res.Values["__failureErrors"] == nil {
		t.Fatalf("失败信息应记录到 __failureErrors")
	}
}

// 8. 声明式 JSON:合法可解析并执行;非法在解析/校验期拒绝。
func TestParseFlowJSON(t *testing.T) {
	valid := []byte(`{
	  "flow": "review",
	  "steps": [
	    {"id": "read", "use": {"service": "svc", "method": "m"}},
	    {"id": "scan", "event": {"name": "flow/scan", "mode": "waterfall"},
	     "when": "$.read == null", "retry": {"max": 1, "backoff": "1ms"},
	     "timeout": "2s", "checkpoint": true}
	  ]
	}`)
	f, err := flow.Parse(valid)
	if err != nil {
		t.Fatalf("合法 JSON 解析失败: %v", err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("校验失败: %v", err)
	}

	bad := map[string]string{
		"未知字段":    `{"flow":"x","steps":[{"id":"a","use":{"service":"s","method":"m"},"foo":1}]}`,
		"空 steps": `{"flow":"x","steps":[]}`,
		"重复 id":   `{"flow":"x","steps":[{"id":"a","use":{"service":"s","method":"m"}},{"id":"a","abort":"b"}]}`,
		"无动作":     `{"flow":"x","steps":[{"id":"a"}]}`,
		"两个动作":    `{"flow":"x","steps":[{"id":"a","use":{"service":"s","method":"m"},"abort":"b"}]}`,
		"坏 when":  `{"flow":"x","steps":[{"id":"a","abort":"b","when":"$.x >"}]}`,
		"坏时长":     `{"flow":"x","steps":[{"id":"a","use":{"service":"s","method":"m"},"timeout":"bad"}]}`,
		"坏事件模式":   `{"flow":"x","steps":[{"id":"a","event":{"name":"e","mode":"parallel"}}]}`,
	}
	for name, raw := range bad {
		if _, err := flow.Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: 应被拒绝", name)
		}
	}
}

// 9. 与内核集成:KernelRuntime 走 inject 门禁与服务/waterfall。
func TestKernelRuntimeIntegration(t *testing.T) {
	app := newApp(t)
	app.Root().OnWaterfall("flow/op", func(_ context.Context, _ *kernel.Context, in any, next kernel.Next) (any, error) {
		n, _ := in.(float64)
		return next(n + 1)
	})

	provider := kernel.Func("provider", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("calc", calcService{})
		return nil, err
	}).WithProvide("calc")
	if _, err := app.Plugin(provider, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载提供者失败: %v", err)
	}

	var result *flow.Result
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		f := flow.New("kernel").
			Add(flow.Step{ID: "dbl", Use: &flow.UseStep{Service: "calc", Method: "double", Args: map[string]any{"n": float64(2)}}}).
			Add(flow.Step{ID: "op", Event: &flow.EventStep{Name: "flow/op", Mode: "waterfall", Input: "$.dbl"}})
		rt := flow.NewKernelRuntime(ctx)
		var err error
		result, err = f.Run(context.Background(), rt, flow.Options{})
		return nil, err
	}).WithInject("calc")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if result == nil {
		t.Fatalf("流水线没有产出结果")
	}
	if result.Values["dbl"] != float64(4) {
		t.Fatalf("服务调用结果不正确: %v", result.Values["dbl"])
	}
	if result.Values["op"] != float64(5) {
		t.Fatalf("waterfall 结果不正确: %v", result.Values["op"])
	}
}

// calcService 是实现了 flow.Invoker 的服务(测试用)。
type calcService struct{}

func (calcService) Invoke(_ context.Context, method string, args map[string]any) (any, error) {
	if method != "double" {
		return nil, kernel.Fail(kernel.CodeInvalid, "未知方法 %q", method)
	}
	n, _ := args["n"].(float64)
	return n * 2, nil
}

// 10. 并行分支:结果合并,父步骤记录 ok/errors。
func TestParallelBranches(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, args map[string]any) (any, error) {
		switch method {
		case "a":
			return float64(1), nil
		case "b":
			return float64(2), nil
		case "sum":
			x, _ := args["x"].(float64)
			y, _ := args["y"].(float64)
			return x + y, nil
		}
		return nil, nil
	}
	f := flow.New("parallel").Add(
		flow.Step{ID: "p", Parallel: &flow.ParallelStep{Branches: []flow.Step{
			{ID: "a", Use: &flow.UseStep{Service: "svc", Method: "a"}},
			{ID: "b", Use: &flow.UseStep{Service: "svc", Method: "b"}},
		}}},
		flow.Step{ID: "after", Use: &flow.UseStep{Service: "svc", Method: "sum", Args: map[string]any{"x": "$.a", "y": "$.b"}}},
	)
	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("并行运行失败: %v", err)
	}
	if res.Values["a"] != float64(1) || res.Values["b"] != float64(2) {
		t.Fatalf("分支结果未合并: %v", res.Values)
	}
	if res.Values["after"] != float64(3) {
		t.Fatalf("后续步骤应能引用分支结果: %v", res.Values["after"])
	}
	parent, ok := res.Values["p"].(map[string]any)
	if !ok || parent["ok"] != true {
		t.Fatalf("父步骤结果不正确: %v", res.Values["p"])
	}
}

// 11. collect 模式:分支失败不终止流线,错误记入父步骤。
func TestParallelCollect(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		switch method {
		case "bad":
			return nil, errors.New("分支失败")
		case "good":
			return float64(7), nil
		case "after":
			return float64(8), nil
		}
		return nil, nil
	}
	f := flow.New("collect").Add(
		flow.Step{ID: "p", Parallel: &flow.ParallelStep{Mode: "collect", Branches: []flow.Step{
			{ID: "bad", Use: &flow.UseStep{Service: "svc", Method: "bad"}},
			{ID: "good", Use: &flow.UseStep{Service: "svc", Method: "good"}},
		}}},
		flow.Step{ID: "after", Use: &flow.UseStep{Service: "svc", Method: "after"}},
	)
	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("collect 模式不应让流线失败: %v", err)
	}
	parent := res.Values["p"].(map[string]any)
	if parent["ok"] != false {
		t.Fatalf("父步骤应记录失败: %v", parent)
	}
	if errs, _ := parent["errors"].([]string); len(errs) != 1 {
		t.Fatalf("应记录 1 个分支错误,实际 %v", parent["errors"])
	}
	if res.Values["good"] != float64(7) || res.Values["after"] != float64(8) {
		t.Fatalf("成功分支结果/后续步骤不正确: %v", res.Values)
	}
}

// 12. abort 模式(默认):任一分支失败 → 流线失败。
func TestParallelAbort(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		if method == "bad" {
			return nil, errors.New("分支失败")
		}
		return float64(1), nil
	}
	f := flow.New("abort").Add(flow.Step{ID: "p", Parallel: &flow.ParallelStep{Branches: []flow.Step{
		{ID: "bad", Use: &flow.UseStep{Service: "svc", Method: "bad"}},
		{ID: "ok", Use: &flow.UseStep{Service: "svc", Method: "ok"}},
	}}})
	if _, err := f.Run(context.Background(), rt, flow.Options{}); err == nil || !strings.Contains(err.Error(), "并行分支") {
		t.Fatalf("abort 模式应失败,实际 %v", err)
	}
}

// 13. 分支真的并发:两个分支都启动后才放行。
func TestParallelConcurrent(t *testing.T) {
	rt := newFakeRT()
	started := make(chan string, 2)
	release := make(chan struct{})
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		started <- method
		<-release
		return float64(1), nil
	}
	f := flow.New("concurrent").Add(flow.Step{ID: "p", Parallel: &flow.ParallelStep{Branches: []flow.Step{
		{ID: "a", Use: &flow.UseStep{Service: "svc", Method: "a"}},
		{ID: "b", Use: &flow.UseStep{Service: "svc", Method: "b"}},
	}}})

	done := make(chan error, 1)
	go func() { _, err := f.Run(context.Background(), rt, flow.Options{}); done <- err }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("分支没有并发启动")
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("并发运行失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("并发运行超时")
	}
}

// 14. 声明式 parallel 的解析与校验。
func TestParseParallel(t *testing.T) {
	valid := []byte(`{"flow":"p","steps":[
	  {"id":"p","parallel":{"mode":"collect","branches":[
	    {"id":"a","use":{"service":"s","method":"m"}},
	    {"id":"b","event":{"name":"e"}}
	  ]}}
	]}`)
	if _, err := flow.Parse(valid); err != nil {
		t.Fatalf("合法 parallel 解析失败: %v", err)
	}
	bad := map[string]string{
		"空分支":  `{"flow":"p","steps":[{"id":"p","parallel":{"branches":[]}}]}`,
		"坏模式":  `{"flow":"p","steps":[{"id":"p","parallel":{"mode":"wait","branches":[{"id":"a","abort":"x"}]}}]}`,
		"分支重名": `{"flow":"p","steps":[{"id":"p","parallel":{"branches":[{"id":"p","abort":"x"}]}}]}`,
		"无动作":  `{"flow":"p","steps":[{"id":"p"}]}`,
	}
	for name, raw := range bad {
		if _, err := flow.Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
}

// 15. forEach:逐项执行并把结果按索引收集。
func TestForEachResults(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, args map[string]any) (any, error) {
		switch method {
		case "seed":
			return []any{float64(1), float64(2), float64(3)}, nil
		case "echo":
			return args["v"], nil
		}
		return nil, nil
	}
	each := &flow.ForEachStep{Over: "$.items", As: "v",
		Step: flow.Step{ID: "one", Use: &flow.UseStep{Service: "svc", Method: "echo", Args: map[string]any{"v": "$.v"}}},
	}
	f := flow.New("each").Add(
		flow.Step{ID: "items", Use: &flow.UseStep{Service: "svc", Method: "seed"}},
		flow.Step{ID: "e", ForEach: each},
	)
	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("forEach 运行失败: %v", err)
	}
	parent := res.Values["e"].(map[string]any)
	results := parent["results"].([]any)
	if len(results) != 3 || results[0] != float64(1) || results[2] != float64(3) {
		t.Fatalf("forEach 结果不正确: %v", results)
	}
	if parent["ok"] != true {
		t.Fatalf("forEach 应成功: %v", parent)
	}
}

// 16. forEach 并发上限:同一时刻不超过 Concurrency。
func TestForEachConcurrency(t *testing.T) {
	rt := newFakeRT()
	var active, max int32
	gate := make(chan struct{})
	started := make(chan int, 8)
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		if method != "work" {
			return []any{float64(1), float64(2), float64(3), float64(4)}, nil
		}
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&max)
			if cur <= old || atomic.CompareAndSwapInt32(&max, old, cur) {
				break
			}
		}
		started <- 1
		<-gate
		atomic.AddInt32(&active, -1)
		return float64(1), nil
	}
	each := &flow.ForEachStep{Over: "$.items", Concurrency: 2,
		Step: flow.Step{ID: "one", Use: &flow.UseStep{Service: "svc", Method: "work"}},
	}
	f := flow.New("conc").Add(
		flow.Step{ID: "items", Use: &flow.UseStep{Service: "svc", Method: "seed"}},
		flow.Step{ID: "e", ForEach: each},
	)
	done := make(chan error, 1)
	go func() { _, err := f.Run(context.Background(), rt, flow.Options{}); done <- err }()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("并发分支没有启动")
		}
	}
	select {
	case <-started:
		t.Fatalf("并发上限为 2,不应有第 3 个任务启动")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forEach 运行失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("forEach 运行超时")
	}
	if got := atomic.LoadInt32(&max); got > 2 {
		t.Fatalf("并发上限被突破: %d", got)
	}
}

// 17. forEach 失败:错误点名第几项。
func TestForEachError(t *testing.T) {
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, args map[string]any) (any, error) {
		if method == "seed" {
			return []any{float64(1), float64(2), float64(3)}, nil
		}
		if args["v"] == float64(2) {
			return nil, errors.New("第 2 项坏了")
		}
		return args["v"], nil
	}
	each := &flow.ForEachStep{Over: "$.items", As: "v",
		Step: flow.Step{ID: "one", Use: &flow.UseStep{Service: "svc", Method: "echo", Args: map[string]any{"v": "$.v"}}},
	}
	f := flow.New("fail").Add(
		flow.Step{ID: "items", Use: &flow.UseStep{Service: "svc", Method: "seed"}},
		flow.Step{ID: "e", ForEach: each},
	)
	_, err := f.Run(context.Background(), rt, flow.Options{})
	if err == nil || !strings.Contains(err.Error(), "第 1 项") {
		t.Fatalf("forEach 失败应点名索引,实际 %v", err)
	}
}

// 18. 声明式 forEach 解析与校验。
func TestParseForEach(t *testing.T) {
	valid := []byte(`{"flow":"e","steps":[
	  {"id":"items","use":{"service":"s","method":"m"}},
	  {"id":"e","forEach":{"over":"$.items","as":"v","concurrency":2,
	    "step":{"id":"one","use":{"service":"s","method":"m","args":{"v":"$.v"}}}}}
	]}`)
	if _, err := flow.Parse(valid); err != nil {
		t.Fatalf("合法 forEach 解析失败: %v", err)
	}
	bad := map[string]string{
		"over 非路径": `{"flow":"e","steps":[{"id":"e","forEach":{"over":"items","step":{"id":"one","abort":"x"}}}]}`,
		"并发超限":     `{"flow":"e","steps":[{"id":"e","forEach":{"over":"$.items","concurrency":33,"step":{"id":"one","abort":"x"}}}]}`,
		"模板缺 id":   `{"flow":"e","steps":[{"id":"e","forEach":{"over":"$.items","step":{"abort":"x"}}}]}`,
		"嵌套":       `{"flow":"e","steps":[{"id":"e","forEach":{"over":"$.items","step":{"id":"one","forEach":{"over":"$.x","step":{"id":"z","abort":"x"}}}}}]}`,
	}
	for name, raw := range bad {
		if _, err := flow.Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
}

// 19. Compensate:失败后按逆序执行补偿步骤。
func TestCompensateReverse(t *testing.T) {
	rt := newFakeRT()
	var calls []string
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		calls = append(calls, method)
		if method == "bad" {
			return nil, errors.New("失败")
		}
		return float64(1), nil
	}
	f := flow.New("compensate").Add(
		flow.Step{ID: "bad", Use: &flow.UseStep{Service: "svc", Method: "bad"}},
	)
	f.Compensate = []flow.Step{
		{ID: "c1", Use: &flow.UseStep{Service: "svc", Method: "comp1"}},
		{ID: "c2", Use: &flow.UseStep{Service: "svc", Method: "comp2"}},
	}
	_, err := f.Run(context.Background(), rt, flow.Options{})
	if err == nil {
		t.Fatalf("主链应失败")
	}
	want := "bad,comp2,comp1"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("补偿顺序不正确: 期望 %s,实际 %s", want, got)
	}
}

// 20. subflow:加载子文件、输入模板解析、结果回填父值表。
func TestSubflowRunsChild(t *testing.T) {
	dir := t.TempDir()
	child := `{"flow":"child","steps":[
	  {"id":"double","use":{"service":"svc","method":"double","args":{"n":"$.n"}}}
	]}`
	if err := os.WriteFile(filepath.Join(dir, "child.flow.json"), []byte(child), 0o644); err != nil {
		t.Fatalf("写子流线失败: %v", err)
	}

	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, args map[string]any) (any, error) {
		switch method {
		case "seed":
			return float64(2), nil
		case "double":
			n, _ := args["n"].(float64)
			return n * 2, nil
		case "echo":
			return args["v"], nil
		}
		return nil, nil
	}
	f := &flow.Flow{Name: "parent", Dir: dir}
	f.Add(
		flow.Step{ID: "seed", Use: &flow.UseStep{Service: "svc", Method: "seed"}},
		flow.Step{ID: "sub", Subflow: &flow.SubflowStep{Path: "child.flow.json", Input: map[string]any{"n": "$.seed"}}},
		flow.Step{ID: "after", Use: &flow.UseStep{Service: "svc", Method: "echo", Args: map[string]any{"v": "$.sub.double"}}},
	)
	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("subflow 运行失败: %v", err)
	}
	sub, ok := res.Values["sub"].(map[string]any)
	if !ok || sub["double"] != float64(4) {
		t.Fatalf("子流线结果未回填: %v", res.Values["sub"])
	}
	if res.Values["after"] != float64(4) {
		t.Fatalf("父流线引用子结果失败: %v", res.Values["after"])
	}
}

// 21. subflow 缺失文件报错。
func TestSubflowMissingFile(t *testing.T) {
	f := &flow.Flow{Name: "parent", Dir: t.TempDir()}
	f.Add(flow.Step{ID: "sub", Subflow: &flow.SubflowStep{Path: "nope.flow.json"}})
	if _, err := f.Run(context.Background(), newFakeRT(), flow.Options{}); err == nil {
		t.Fatalf("缺失子流线应报错")
	}
}

// 22. subflow 环检测:a → b → a。
func TestSubflowCycle(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}
	write("a.flow.json", `{"flow":"a","steps":[{"id":"toB","subflow":{"path":"b.flow.json"}}]}`)
	write("b.flow.json", `{"flow":"b","steps":[{"id":"toA","subflow":{"path":"a.flow.json"}}]}`)

	parent, err := flow.Load(filepath.Join(dir, "a.flow.json"))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := parent.Run(context.Background(), newFakeRT(), flow.Options{}); err == nil || !strings.Contains(err.Error(), "环") {
		t.Fatalf("环检测失败: %v", err)
	}
}

// 23. 声明式 subflow 解析与校验。
func TestParseSubflow(t *testing.T) {
	valid := []byte(`{"flow":"p","steps":[
	  {"id":"sub","subflow":{"path":"child.flow.json","input":{"n":"$.seed"}}}
	]}`)
	if _, err := flow.Parse(valid); err != nil {
		t.Fatalf("合法 subflow 解析失败: %v", err)
	}
	bad := map[string]string{
		"缺 path": `{"flow":"p","steps":[{"id":"sub","subflow":{}}]}`,
		"绝对路径":   `{"flow":"p","steps":[{"id":"sub","subflow":{"path":"/tmp/x.json"}}]}`,
		"逃逸":     `{"flow":"p","steps":[{"id":"sub","subflow":{"path":"../x.json"}}]}`,
		"两个动作":   `{"flow":"p","steps":[{"id":"sub","subflow":{"path":"a.json"},"use":{"service":"s","method":"m"}}]}`,
	}
	for name, raw := range bad {
		if _, err := flow.Parse([]byte(raw)); err == nil {
			t.Fatalf("%s: 应拒绝", name)
		}
	}
}

// 26. fanout 是 parallel 的别名:解析/校验/执行。
func TestFanout(t *testing.T) {
	valid := []byte(`{"flow":"f","steps":[
	  {"id":"fan","fanout":{"branches":[
	    {"id":"a","use":{"service":"svc","method":"a"}},
	    {"id":"b","use":{"service":"svc","method":"b"}}
	  ]}}
	]}`)
	f, err := flow.Parse(valid)
	if err != nil {
		t.Fatalf("合法 fanout 解析失败: %v", err)
	}
	rt := newFakeRT()
	rt.callFn = func(_ context.Context, _ string, method string, _ map[string]any) (any, error) {
		return method, nil
	}
	res, err := f.Run(context.Background(), rt, flow.Options{})
	if err != nil {
		t.Fatalf("fanout 运行失败: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		if _, ok := res.Values[id]; !ok {
			t.Fatalf("分支 %q 未执行: %v", id, res.Values)
		}
	}

	bad := map[string]string{
		"空分支":  `{"flow":"f","steps":[{"id":"fan","fanout":{"branches":[]}}]}`,
		"两个动作": `{"flow":"f","steps":[{"id":"fan","fanout":{"branches":[{"id":"a","abort":"x"}]},"use":{"service":"s","method":"m"}}]}`,
	}
	for name, raw := range bad {
		if _, err := flow.Parse([]byte(raw)); err == nil {
			t.Fatalf("%s 应拒绝", name)
		}
	}
}
