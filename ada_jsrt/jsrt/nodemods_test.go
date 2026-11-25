package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// 1. node:timers 必须是全局定时器的同一组函数,并且模块路径创建的定时器同样能被 await。
func TestNodeTimersModule(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import * as timers from "node:timers"

export function same(): boolean {
  return timers.setTimeout === setTimeout && timers.clearTimeout === clearTimeout
}

export async function wait(v: number): Promise<number> {
  return await new Promise<number>((resolve) => { timers.setTimeout(() => resolve(v + 1), 1) })
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "same")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if same, _ := got.(bool); !same {
		t.Fatalf("模块导出的定时器必须与全局是同一组函数")
	}

	got, err = rt.Call(context.Background(), "wait", 41)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if n := asInt(t, got); n != 42 {
		t.Fatalf("期望 42,实际 %v", got)
	}
}

// 2. node:timers/promises:setTimeout/setImmediate/可迭代的 setInterval。
func TestNodeTimersPromises(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { setInterval as every, setImmediate as soon, setTimeout as delay } from "node:timers/promises"

export async function run(): Promise<any> {
  const a = await delay(1, "delay")
  const b = await soon("immediate")
  const ticks: string[] = []
  for await (const v of every(1, "tick")) {
    ticks.push(v)
    if (ticks.length >= 3) break
  }
  return { a, b, ticks: ticks.join(",") }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if s, _ := obj["a"].(string); s != "delay" {
		t.Fatalf("setTimeout 应返回 value,实际 %v", obj["a"])
	}
	if s, _ := obj["b"].(string); s != "immediate" {
		t.Fatalf("setImmediate 应返回 value,实际 %v", obj["b"])
	}
	if s, _ := obj["ticks"].(string); s != "tick,tick,tick" {
		t.Fatalf("setInterval 迭代不对: %q", s)
	}
}

// 3. node:stream:Readable.from → PassThrough → pipeline,以及 for await 消费。
func TestNodeStreamPipelineAndIteration(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { PassThrough, Readable, pipeline } from "node:stream"

function collect(stream: any): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const parts: string[] = []
    stream.on("data", (c: any) => parts.push(String(c)))
    stream.on("end", () => resolve(parts.join("")))
    stream.on("error", (e: any) => reject(e))
  })
}

export async function piped(): Promise<string> {
  const src = Readable.from(["a", "b", "c"])
  const pass = new PassThrough()
  const done = collect(pass)
  pipeline(src, pass)
  return await done
}

export async function iterated(): Promise<string> {
  const src = Readable.from(["x", "y", "z"])
  const out: string[] = []
  for await (const chunk of src) out.push(String(chunk))
  return out.join("-")
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "piped")
	if err != nil {
		t.Fatalf("pipeline 失败: %v", err)
	}
	if s, _ := got.(string); s != "abc" {
		t.Fatalf("pipeline 期望 abc,实际 %q", s)
	}

	got, err = rt.Call(context.Background(), "iterated")
	if err != nil {
		t.Fatalf("异步迭代失败: %v", err)
	}
	if s, _ := got.(string); s != "x-y-z" {
		t.Fatalf("异步迭代期望 x-y-z,实际 %q", s)
	}
}

// 3b. node:stream 的背压:异步 _write 让后续写入排队,write() 会说真话,drain 在排空后发。
func TestNodeStreamBackpressure(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { Transform, Writable } from "node:stream"

export async function writable(): Promise<any> {
  const seen: string[] = []
  const w: any = new Writable({ highWaterMark: 4 })
  w._write = function (chunk: any, enc: any, cb: any) {
    seen.push(String(chunk))
    setTimeout(cb, 1)
  }
  const accepted: boolean[] = []
  accepted.push(w.write("aaaa"))
  accepted.push(w.write("bbbb"))
  accepted.push(w.write("cccc"))
  let drained = false
  w.on("drain", () => {
    drained = true
  })
  await new Promise<void>((resolve) => { w.end(() => resolve()) })
  return { accepted: accepted.join(","), drained, order: seen.join("") }
}

export async function transform(): Promise<any> {
  const seen: string[] = []
  const t: any = new Transform({ highWaterMark: 1 })
  t._transform = function (chunk: any, enc: any, cb: any) {
    seen.push(String(chunk))
    setTimeout(() => cb(null, "[" + String(chunk) + "]"), 1)
  }
  const out: string[] = []
  t.on("data", (chunk: any) => out.push(String(chunk)))
  const accepted: boolean[] = []
  accepted.push(t.write("x"))
  accepted.push(t.write("y"))
  await new Promise<void>((resolve) => { t.end(() => resolve()) })
  return { accepted: accepted.join(","), order: seen.join(""), out: out.join("") }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "writable")
	if err != nil {
		t.Fatalf("writable 调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if s, _ := obj["accepted"].(string); s != "true,false,false" {
		t.Fatalf("背压下的 write() 返回值不对: %q", s)
	}
	if b, _ := obj["drained"].(bool); !b {
		t.Fatalf("排空后应发 drain")
	}
	if s, _ := obj["order"].(string); s != "aaaabbbbcccc" {
		t.Fatalf("写入顺序被破坏: %q", s)
	}

	got, err = rt.Call(context.Background(), "transform")
	if err != nil {
		t.Fatalf("transform 调用失败: %v", err)
	}
	obj, _ = got.(map[string]any)
	if s, _ := obj["accepted"].(string); s != "true,false" {
		t.Fatalf("Transform 的 write() 返回值不对: %q", s)
	}
	if s, _ := obj["order"].(string); s != "xy" {
		t.Fatalf("Transform 的输入顺序不对: %q", s)
	}
	if s, _ := obj["out"].(string); s != "[x][y]" {
		t.Fatalf("Transform 的输出不对: %q", s)
	}
}

// 4. node:readline(+ promises):从可读流按行切分,question 返回下一行。
func TestNodeReadline(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { createInterface } from "node:readline"
import { createInterface as createPromiseInterface } from "node:readline/promises"
import { Readable } from "node:stream"

export async function lines(): Promise<string> {
  const rl = createInterface({ input: Readable.from(["a\nb", "\nc\n"]) })
  const out: string[] = []
  for await (const line of rl) out.push(String(line))
  return out.join("|")
}

export async function ask(): Promise<string> {
  const rl = createPromiseInterface({ input: Readable.from(["answer\n"]) })
  const a = await rl.question("q? ")
  rl.close()
  return a
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "lines")
	if err != nil {
		t.Fatalf("按行读取失败: %v", err)
	}
	if s, _ := got.(string); s != "a|b|c" {
		t.Fatalf("期望 a|b|c,实际 %q", s)
	}

	got, err = rt.Call(context.Background(), "ask")
	if err != nil {
		t.Fatalf("question 失败: %v", err)
	}
	if s, _ := got.(string); s != "answer" {
		t.Fatalf("question 期望 answer,实际 %q", s)
	}
}

// 5. node:perf_hooks:单调 now、mark/measure、观察者异步收到条目。
func TestNodePerfHooks(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { PerformanceObserver, performance } from "node:perf_hooks"

export async function run(): Promise<any> {
  const t0 = performance.now()
  performance.mark("a")
  performance.mark("b")
  const measure = performance.measure("m", "a", "b")

  const seen: string[] = []
  const observer = new PerformanceObserver((list: any) => {
    for (const entry of list.getEntries()) seen.push(entry.name)
  })
  observer.observe({ entryTypes: ["mark"] })
  performance.mark("c")
  await new Promise<void>((resolve) => { setTimeout(() => resolve(), 1) })
  observer.disconnect()

  return {
    t0,
    measure: measure.duration,
    seen: seen.join(","),
    marks: performance.getEntriesByType("mark").length,
    origin: performance.timeOrigin,
  }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if n := asInt(t, obj["t0"]); n < 0 {
		t.Fatalf("now() 应是非负数,实际 %v", obj["t0"])
	}
	if d, _ := obj["measure"].(float64); d < 0 {
		t.Fatalf("measure 的时长不能为负: %v", obj["measure"])
	}
	if s, _ := obj["seen"].(string); !strings.Contains(s, "c") {
		t.Fatalf("观察者应收到 mark c,实际 %q", s)
	}
	if n := asInt(t, obj["marks"]); n < 3 {
		t.Fatalf("应有 3 个 mark 条目,实际 %v", n)
	}
}

// 6. node:tty:没有终端 → isatty 恒 false;流类构造即抛 ERR_NOT_SUPPORTED。
func TestNodeTty(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import { isatty, WriteStream } from "node:tty"

export function probe(): any {
  let code = "no-error"
  try {
    new (WriteStream as any)(1)
  } catch (e: any) {
    code = e.code
  }
  return { tty: isatty(1), code }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if b, _ := obj["tty"].(bool); b {
		t.Fatalf("运行时没有终端,isatty 必须为 false")
	}
	if s, _ := obj["code"].(string); s != "ERR_NOT_SUPPORTED" {
		t.Fatalf("WriteStream 构造应抛 ERR_NOT_SUPPORTED,实际 %q", s)
	}
}
