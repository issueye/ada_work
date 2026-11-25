package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

// newRuntime 装配一个最小运行时;opts 里的 Artifact 由这里补上。
func newRuntime(t *testing.T, files map[string]string, opts jsrt.Options) *jsrt.Runtime {
	t.Helper()
	opts.Artifact = newArtifact(t, files)
	rt, err := jsrt.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	return rt
}

// logSink 收集 Logf 的输出,用于断言「定时器里的异常只记日志」。
type logSink struct {
	mu   sync.Mutex
	text string
}

func (s *logSink) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text += " " + format
	for _, a := range args {
		s.text += " " + toString(a)
	}
}

func (s *logSink) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}

// 1. 入口是 async 函数:宿主等它结算,并拿到 await 之后的值。
func TestAwaitInsideEntry(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
function twice(v: number): Promise<number> { return Promise.resolve(v * 2) }
export async function double(v: number): Promise<number> {
  const out = await twice(v)
  return out + 1
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "double", 20)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if n := asInt(t, got); n != 41 {
		t.Fatalf("期望 41,实际 %v", got)
	}
	// 引导模块的内部导出不能出现在插件的对外表面上。
	for _, name := range rt.Exports() {
		if strings.HasPrefix(name, "__ada") {
			t.Fatalf("Exports() 泄漏了内部导出 %q: %v", name, rt.Exports())
		}
	}
}

// 2. 调用内的定时器可以被 await,并在本次调用里到期。
func TestSetTimeoutInsideCall(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
export async function wait(v: number): Promise<number> {
  return await new Promise<number>((resolve) => { setTimeout(() => resolve(v + 5), 1) })
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "wait", 7)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if n := asInt(t, got); n != 12 {
		t.Fatalf("期望 12,实际 %v", got)
	}
}

// 3. node:fs/promises 真正异步:await 之后文件已经落盘,读回的是内容。
func TestAwaitFSPromises(t *testing.T) {
	dir := t.TempDir()
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { readFile, writeFile } from "node:fs/promises"
export async function roundtrip(name: string, body: string): Promise<string> {
  await writeFile("/workspace/" + name, body)
  return await readFile("/workspace/" + name, "utf8")
}`,
	}, jsrt.Options{Node: &jsrt.NodeOptions{
		FS: &jsrt.NodeFSOptions{Root: dir, Mount: "/workspace", Cwd: "/workspace", Write: true},
	}})

	got, err := rt.Call(context.Background(), "roundtrip", "a.txt", "hello-loop")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if s, _ := got.(string); s != "hello-loop" {
		t.Fatalf("期望读回 hello-loop,实际 %v", got)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("文件没有落盘: %v", err)
	}
	if string(saved) != "hello-loop" {
		t.Fatalf("落盘内容不符: %q", string(saved))
	}
}

// 4. 没有被 await 的定时器:入口结算时被清掉,既不会拖住调用,也不会活到下一次调用。
func TestUnawaitedTimerIsCleared(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
declare const globalThis: any
export function fireAndForget(): string {
  setTimeout(() => { globalThis.__fired = true }, 5000)
  return "ok"
}
export function fired(): boolean { return globalThis.__fired === true }`,
	}, jsrt.Options{})

	start := time.Now()
	got, err := rt.Call(context.Background(), "fireAndForget")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if s, _ := got.(string); s != "ok" {
		t.Fatalf("期望 ok,实际 %v", got)
	}
	// 若定时器还在排空路径里,这一句要等 5 秒。给 2 秒的余量,只证明「没有被等」。
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("调用被未 await 的定时器拖住了: %s", elapsed)
	}

	got, err = rt.Call(context.Background(), "fired")
	if err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if b, _ := got.(bool); b {
		t.Fatalf("定时器跨调用触发了:未 await 的定时器应当在结算时被清掉")
	}
}

// 5. await 一个永远不会结算的 promise:给出明确错误,而不是挂死。
func TestAwaitNeverSettles(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
export async function stuck(): Promise<number> {
  return await new Promise<number>(() => {})
}`,
	}, jsrt.Options{})

	_, err := rt.Call(context.Background(), "stuck")
	if err == nil || !strings.Contains(err.Error(), "未结算") {
		t.Fatalf("期望「调用未结算」错误,实际 %v", err)
	}
}

// 6. 异步拒绝要带回堆栈,不能被吞成空值。
func TestAsyncRejectionSurfaces(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
export async function boom(): Promise<void> {
  await 0
  throw new Error("async-kaboom")
}`,
	}, jsrt.Options{})

	_, err := rt.Call(context.Background(), "boom")
	if err == nil || !strings.Contains(err.Error(), "async-kaboom") {
		t.Fatalf("错误应包含异步异常信息,实际 %v", err)
	}
}

// 7. 定时器回调里的异常只走 OnError(记日志),不让当前调用失败。
func TestTimerErrorDoesNotFailCall(t *testing.T) {
	sink := &logSink{}
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
export async function noisy(): Promise<string> {
  setTimeout(() => { throw new Error("timer-boom") }, 1)
  await new Promise<void>((resolve) => { setTimeout(() => resolve(), 5) })
  return "ok"
}`,
	}, jsrt.Options{Logf: sink.logf})

	got, err := rt.Call(context.Background(), "noisy")
	if err != nil {
		t.Fatalf("调用不该失败: %v", err)
	}
	if s, _ := got.(string); s != "ok" {
		t.Fatalf("期望 ok,实际 %v", got)
	}
	if !strings.Contains(sink.get(), "timer-boom") {
		t.Fatalf("定时器异常应写入日志,实际日志: %q", sink.get())
	}
}

// 8. 同一个运行时连续调用:每次调用有独立的结算,互不串味。
func TestLoopReuseAcrossCalls(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
let n = 0
export async function next(): Promise<number> {
  await 0
  n++
  return n
}`,
	}, jsrt.Options{})

	for want := int64(1); want <= 3; want++ {
		got, err := rt.Call(context.Background(), "next")
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", want, err)
		}
		if n := asInt(t, got); n != want {
			t.Fatalf("第 %d 次调用期望 %d,实际 %v", want, want, got)
		}
	}
}

// 9. 装配期的 apply 可以是 async:料想在返回前生效,cfg 参数照常传入。
func TestAsyncApplyWithHost(t *testing.T) {
	sink := &logSink{}
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
interface Ctx { log(msg: string): void }
export async function apply(ctx: Ctx, cfg: { name: string }): Promise<string> {
  await 0
  ctx.log("applied " + cfg.name)
  return "done:" + cfg.name
}`,
	}, jsrt.Options{Logf: sink.logf})

	got, err := rt.CallWithHost(context.Background(), "apply", map[string]any{"name": "loopy"})
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if s, _ := got.(string); s != "done:loopy" {
		t.Fatalf("期望 done:loopy,实际 %v", got)
	}
	if !strings.Contains(sink.get(), "applied loopy") {
		t.Fatalf("apply 的副作用应在返回前生效,实际日志: %q", sink.get())
	}
}

// BenchmarkCallSync 记录一次「同步入口 + 事件循环」的调用开销。
//
// 预算口径:单次调用 ≤20 µs(原宿主实测基线 6.86 µs)。
func BenchmarkCallSync(b *testing.B) {
	art, err := compile.Build("test", "plugin.ts", map[string]string{
		"plugin.ts": `export function add(a: number, b: number): number { return a + b }`,
	}, compile.Options{})
	if err != nil {
		b.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		b.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close(context.Background()) }()
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		if _, err := rt.Call(ctx, "add", 1, 2); err != nil {
			b.Fatalf("调用失败: %v", err)
		}
	}
}
