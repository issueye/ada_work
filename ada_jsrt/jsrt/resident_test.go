package jsrt_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func residentArtifact(t *testing.T, src string) *compile.Artifact {
	t.Helper()
	art, err := compile.Build("resident", "main.ts", map[string]string{"main.ts": src}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	return art
}

func newResident(t *testing.T, src string, opts jsrt.Options) *jsrt.Runtime {
	t.Helper()
	opts.Artifact = residentArtifact(t, src)
	opts.Mode = jsrt.LoopResident
	if opts.Limits.StartupTimeout == 0 {
		opts.Limits.StartupTimeout = 5 * time.Second
	}
	rt, err := jsrt.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("装配常驻运行时失败: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
	})
	return rt
}

func TestResidentTimerSurvivesCalls(t *testing.T) {
	rt := newResident(t, `
let hits = 0;
export function start(): void {
  setTimeout(() => { hits++ }, 20);
}
export function count(): number { return hits; }
`, jsrt.Options{})
	ctx := context.Background()
	if _, err := rt.Call(ctx, "start"); err != nil {
		t.Fatalf("start 失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := rt.Call(ctx, "count")
		if err != nil {
			t.Fatalf("count 失败: %v", err)
		}
		if n, _ := v.(int64); n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("定时器没有在常驻循环里触发，最后 count=%v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResidentWaitAndClose(t *testing.T) {
	rt := newResident(t, `export function ping(): string { return "pong" }`, jsrt.Options{})
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- rt.Wait(ctx) }()
	time.Sleep(20 * time.Millisecond)
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.Close(closeCtx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait 返回错误: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close 后 Wait 未返回")
	}
	if _, err := rt.Call(ctx, "ping"); err == nil {
		t.Fatal("关闭后 Call 应失败")
	}
}

func TestResidentTopLevelAwait(t *testing.T) {
	rt := newResident(t, `
export const value = await new Promise((resolve) => setTimeout(() => resolve("ready"), 20));
export function get(): string { return value; }
`, jsrt.Options{})
	got, err := rt.Call(context.Background(), "get")
	if err != nil || got != "ready" {
		t.Fatalf("顶层 await 结果不符: got=%v err=%v", got, err)
	}
}

func TestScopedTopLevelAwait(t *testing.T) {
	art := residentArtifact(t, `
export const value = await new Promise((resolve) => setTimeout(() => resolve("ready"), 20));
export function get(): string { return value; }
`)
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Limits:   jsrt.Limits{StartupTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("Scoped 顶层 await 装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	got, err := rt.Call(context.Background(), "get")
	if err != nil || got != "ready" {
		t.Fatalf("Scoped 顶层 await 结果不符: got=%v err=%v", got, err)
	}
}

func TestStartupTimeout(t *testing.T) {
	art := residentArtifact(t, `await new Promise(() => {}); export function noop(): void {}`)
	ctx := context.Background()
	start := time.Now()
	_, err := jsrt.New(ctx, jsrt.Options{
		Artifact: art,
		Mode:     jsrt.LoopResident,
		Limits:   jsrt.Limits{StartupTimeout: 100 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("永不结算的顶层 await 应启动超时")
	}
	if !strings.Contains(err.Error(), "启动超时") && !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("期望启动超时错误，实际 %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("启动超时耗时过长: %v", time.Since(start))
	}
}

func TestAbortControllerGlobal(t *testing.T) {
	rt := newResident(t, `
export function hasAbort(): boolean {
  return typeof AbortController === "function" && typeof AbortSignal === "function";
}
export function aborted(): boolean {
  const c = new AbortController();
  let hit = false;
  c.signal.addEventListener("abort", () => { hit = true }, { once: true });
  c.abort();
  return hit && c.signal.aborted;
}
`, jsrt.Options{})
	ctx := context.Background()
	if got, err := rt.Call(ctx, "hasAbort"); err != nil || got != true {
		t.Fatalf("AbortController 未注入: got=%v err=%v", got, err)
	}
	if got, err := rt.Call(ctx, "aborted"); err != nil || got != true {
		t.Fatalf("AbortSignal 行为不符: got=%v err=%v", got, err)
	}
}

func TestResidentUnhandledRejectionEvent(t *testing.T) {
	events := make(chan jsrt.Event, 8)
	rt := newResident(t, `
Promise.reject(new Error("boom"));
export function ping(): string { return "pong" }
`, jsrt.Options{OnEvent: func(ev jsrt.Event) {
		select {
		case events <- ev:
		default:
		}
	}})
	ctx := context.Background()
	if _, err := rt.Call(ctx, "ping"); err != nil {
		t.Fatalf("ping 失败: %v", err)
	}
	select {
	case ev := <-events:
		if ev.Kind != "unhandled_rejection" || ev.Err == nil || !strings.Contains(ev.Err.Error(), "boom") {
			t.Fatalf("事件不符: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 unhandled_rejection 事件")
	}
}
