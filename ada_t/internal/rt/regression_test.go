package rt_test

// 回归测试:多窗口事件、稳定 handler id、动态 props、附加包后 HMR、
// 子组件 setup 错误边界、服务 panic 回收。
// 这些用例对应 README/分析里已经复现过的缺陷,防止回退。

import (
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/rt"
	"github.com/issueye/ada_t/widget"
)

func countEvents(n *widget.Node) int {
	total := 0
	n.Walk(func(m *widget.Node) bool { total += len(m.Events); return true })
	return total
}

// 两窗口同组件:任意一轮渲染后,两个窗口的 handler 都必须存活。
func TestMultiWindowEvents(t *testing.T) {
	pkg := mustCompile(t, "Counter.aui", map[string]string{"Counter.aui": counterAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	for _, id := range []string{"w1", "w2"} {
		if _, err := r.MountWindow(id, "Counter", nil); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return b.Tree("w1") != nil && b.Tree("w2") != nil }, "两窗口首帧")
	time.Sleep(100 * time.Millisecond)

	want := countEvents(b.Tree("w1")) + countEvents(b.Tree("w2"))
	m, err := r.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if int(m["handlers"]) != want {
		t.Fatalf("两窗 handler 数量不一致: 期望 %d, 实际 %v", want, m["handlers"])
	}

	click := func(win, text string) {
		t.Helper()
		id := findTagID(b.Tree(win), "PrimaryButton", "")
		if id == "" {
			t.Fatalf("%s 没有 PrimaryButton:\n%s", win, b.Tree(win).Dump())
		}
		if !b.Fire(win, id, "click", nil) {
			t.Fatalf("%s 派发失败", win)
		}
		waitFor(t, func() bool { return findTagID(b.Tree(win), "Text", text) != "" }, win+" 未显示 "+text)
	}
	click("w1", "1")
	click("w2", "1")
	click("w1", "2") // w2 渲染之后再点 w1,验证不被清扫
}

// 同一棵不变的树渲染两次应该得到完全相同的 JSON(handler id 跨帧稳定)。
func TestRenderJSONStable(t *testing.T) {
	pkg := mustCompile(t, "Counter.aui", map[string]string{"Counter.aui": counterAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "Counter", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧")
	j1, err := r.RenderOnce("main")
	if err != nil {
		t.Fatal(err)
	}
	j2, err := r.RenderOnce("main")
	if err != nil {
		t.Fatal(err)
	}
	if j1 != j2 {
		t.Fatalf("同一状态两次渲染的 JSON 不同(handler id 不稳定):\n%s\n%s", j1[:minInt(len(j1), 400)], j2[:minInt(len(j2), 400)])
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 对象/数组字面量作为动态 props:不能因为每帧新身份而自激渲染。
func TestLiteralPropDoesNotLoop(t *testing.T) {
	files := map[string]string{
		"P.aui": `
<template><Column><Child a-prop:items="[1, 2, 3]" /></Column></template>
<script lang="ts">
import Child from './Child'
export default { name: 'P', components: { Child }, setup() { return {} } }
</script>`,
		"Child.aui": `
<template><Column><Row a-for="x in props.items"><Text>{{ x }}</Text></Row></Column></template>
<script lang="ts">
export default { name: 'Child' }
</script>`,
	}
	pkg := mustCompile(t, "P.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.MountWindow("w", "P", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Tree("w") != nil }, "首帧")
	if _, err := r.RenderOnce("w"); err != nil { // 强制再渲染一次,进入 props 同步路径
		t.Fatal(err)
	}
	m1, _ := r.Stats()
	time.Sleep(250 * time.Millisecond)
	m2, _ := r.Stats()
	if d := m2["seq"] - m1["seq"]; d > 3 {
		t.Fatalf("字面量 props 自激渲染: 250ms 内 seq 增长 %v", d)
	}
}

// 附加包(Inspector)先装载之后,主包首次热重载仍须成功。
func TestHMRAfterAdditionalPackage(t *testing.T) {
	const parentV1 = `<template><Column><Text>{{ v }}</Text><Child /></Column></template>
<script lang="ts">
import Child from './Child'
export default { name: 'P', components: { Child }, setup() { return { v: 1 } } }
</script>`
	const parentV2 = `<template><Column><Text>{{ v }}</Text><Child /></Column></template>
<script lang="ts">
import Child from './Child'
export default { name: 'P', components: { Child }, setup() { return { v: 2 } } }
</script>`
	const child = `<template><Text>c</Text></template>`
	const dummy = `<template><Column><Text>d</Text></Column></template>`

	pkg1, err := compile.CompileFiles("P.aui", map[string]string{"P.aui": parentV1, "Child.aui": child}, compile.Options{Entry: "P"})
	if err != nil {
		t.Fatal(err)
	}
	b := headless.New()
	r, err := rt.New(pkg1, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "P", nil); err != nil {
		t.Fatal(err)
	}
	dpkg, err := compile.CompileFiles("D.aui", map[string]string{"D.aui": dummy}, compile.Options{Entry: "D"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.LoadAdditional(dpkg); err != nil {
		t.Fatalf("LoadAdditional: %v", err)
	}
	pkg2, err := compile.CompileFiles("P.aui", map[string]string{"P.aui": parentV2, "Child.aui": child}, compile.Options{Entry: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.HotReload(pkg2); err != nil {
		t.Fatalf("附加包之后首次 HotReload 失败: %v", err)
	}
	waitFor(t, func() bool { return findTagID(b.Tree("main"), "Text", "2") != "" }, "热重载后未显示新脚本的值")
}

// 子组件 onMount 在渲染期修改父状态:渲染结束后必须补一轮,界面不能停在旧值。
func TestRenderTimeStateChangeRepaints(t *testing.T) {
	files := map[string]string{
		"Host.aui": `<template><Column><Text>{{ state.ready ? 'ready' : 'wait' }}</Text><Child a-prop:onReady="markReady" /></Column></template>
<script lang="ts">
import Child from './Child'
import { reactive } from 'ada:surface'
export default {
  name: 'Host',
  components: { Child },
  setup() {
    const state = reactive({ ready: false })
    function markReady() { state.ready = true }
    return { state, markReady }
  },
}
</script>`,
		"Child.aui": `<template><Text>child</Text></template>
<script lang="ts">
import { onMount } from 'ada:surface'
export default {
  name: 'Child',
  setup(props) {
    onMount(() => { if (props.onReady) props.onReady() })
    return {}
  },
}
</script>`,
	}
	pkg := mustCompile(t, "Host.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "Host", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "ready") != ""
	}, "渲染期状态变更未触发补渲染")
}

// watch 返回的 stop 必须真正解除依赖;setup 里创建的 watch 随实例清理。
func TestWatchStop(t *testing.T) {
	const aui = `<template>
  <Column>
    <Text>{{ state.n }} / {{ state.hits }}</Text>
    <Button a-on:click="inc">Inc</Button>
    <Button a-on:click="stopWatch">Stop</Button>
  </Column>
</template>
<script lang="ts">
import { reactive, watch } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ n: 0, hits: 0 })
    const stop = watch(() => state.n, () => { state.hits++ })
    function inc() { state.n++ }
    function stopWatch() { stop() }
    return { state, inc, stopWatch }
  },
}
</script>`
	pkg := mustCompile(t, "W.aui", map[string]string{"W.aui": aui})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "W", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧")
	click := func(label string) {
		t.Helper()
		id := findTagID(b.Tree("main"), "Button", label)
		if id == "" {
			t.Fatalf("找不到按钮 %s:\n%s", label, b.Tree("main").Dump())
		}
		if !b.Fire("main", id, "click", nil) {
			t.Fatalf("%s 点击派发失败", label)
		}
	}
	click("Inc")
	waitFor(t, func() bool { return findTagID(b.Tree("main"), "Text", "1 / 1") != "" }, "watch 未触发")
	click("Stop")
	click("Inc")
	waitFor(t, func() bool { return findTagID(b.Tree("main"), "Text", "2 / 1") != "" }, "stop 之后 watch 仍在触发")
}

// 子组件 setup 失败:展示自己的错误卡片,而不是空 Column。
func TestChildSetupErrorCard(t *testing.T) {
	files := map[string]string{
		"Host.aui": `<template><Column><Text>host</Text><Bad /></Column></template>
<script lang="ts">
import Bad from './Bad'
export default { name: 'Host', components: { Bad }, setup() { return {} } }
</script>`,
		"Bad.aui": `<template><Column><Text>{{ state.msg }}</Text></Column></template>
<script lang="ts">
export default { name: 'Bad', setup() { throw new Error('子组件 setup 爆炸') } }
</script>`,
	}
	pkg := mustCompile(t, "Host.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "Host", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧")
	tree := b.Tree("main")
	if findTagID(tree, "Text", "host") == "" {
		t.Fatalf("宿主内容丢失:\n%s", tree.Dump())
	}
	dump := tree.Dump()
	if !strings.Contains(dump, "装配出错") || !strings.Contains(dump, "子组件 setup 爆炸") {
		t.Fatalf("子组件 setup 错误卡片缺失:\n%s", dump)
	}
}

// 服务 panic 必须变成 JS 异常,而不是带崩进程。
func TestServicePanicBecomesError(t *testing.T) {
	const aui = `<template><Column><Text>{{ state.err }}</Text><Button a-on:click="boom">Boom</Button></Column></template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ err: '' })
    function boom() {
      try { ctx.api.call('svc', 'boom') } catch (e) { state.err = 'caught: ' + (e && e.message ? e.message : e) }
    }
    return { state, boom }
  },
}
</script>`
	pkg := mustCompile(t, "Panic.aui", map[string]string{"Panic.aui": aui})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if err := r.Expose("svc", "boom", func() { panic("砰") }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MountWindow("main", "Panic", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧")
	id := findTagID(b.Tree("main"), "Button", "Boom")
	if id == "" {
		t.Fatalf("找不到按钮:\n%s", b.Tree("main").Dump())
	}
	if !b.Fire("main", id, "click", nil) {
		t.Fatal("点击派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && strings.Contains(tree.Dump(), "caught:") && strings.Contains(tree.Dump(), "panic")
	}, "服务 panic 未转成 JS 异常")
}
