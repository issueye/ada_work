package rt_test

// 路线图回归:props 删除同步、单次渲染、渲染错误保子实例、HMR 传递依赖、关闭不挂起。

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/rt"
)

// treeHasText 是带 nil 保护的文本断言(首帧到达前 Tree 为 nil)。
func treeHasText(b *headless.Backend, win, text string) bool {
	tree := b.Tree(win)
	return tree != nil && findTagID(tree, "Text", text) != ""
}

const propsClearParentAUI = `
<meta>
{ "name": "PParent" }
</meta>
<template>
  <Column gap="4">
    <Child a-prop:value="state.v" />
    <Button a-on:click="clear">Clear</Button>
  </Column>
</template>
<script lang="ts">
import Child from './Child'
import { reactive } from 'ada:surface'
export default {
  name: 'PParent',
  components: { Child },
  setup() {
    const state = reactive({ v: 'hello' })
    function clear() { state.v = undefined }
    return { state, clear }
  },
}
</script>
`

const propsClearChildAUI = `
<meta>
{ "name": "Child" }
</meta>
<template>
  <Column><Text>{{ props.value ?? 'nil' }}</Text></Column>
</template>
`

// TestChildPropUndefinedSyncs:动态 prop 变成 undefined 时子实例必须收到,
// 而不是保留旧值。
func TestChildPropUndefinedSyncs(t *testing.T) {
	pkg := mustCompile(t, "PParent.aui", map[string]string{
		"PParent.aui": propsClearParentAUI,
		"Child.aui":   propsClearChildAUI,
	})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "PParent", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "hello") }, "首帧未渲染")

	btn := findTagID(b.Tree("main"), "Button", "Clear")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("Clear 点击派发失败")
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "nil") }, "prop 变 undefined 后子组件仍显示旧值")
}

const zeroArgCallAUI = `
<template>
  <Column gap="4">
    <Text>{{ greet() }}</Text>
    <Button a-on:click="bump()">Bump</Button>
    <Text>{{ state.n }}</Text>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ n: 0 })
    function greet() { return 'hi' }
    function bump() { state.n++ }
    return { state, greet, bump }
  },
}
</script>
`

// TestZeroArgCalls:零参表达式的 args 会被 omitempty 省略,
// 渲染/动作求值都不能假设 ast.args 存在。
func TestZeroArgCalls(t *testing.T) {
	pkg := mustCompile(t, "Zero.aui", map[string]string{"Zero.aui": zeroArgCallAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Zero", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "hi") }, "零参表达式调用未渲染")
	btn := findTagID(b.Tree("main"), "Button", "Bump")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("Bump 点击派发失败")
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "1") }, "零参动作调用未执行")
}

const computedWatchAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.hits[0] ?? '-' }}</Text>
    <Text>{{ state.n }}</Text>
    <PrimaryButton a-on:click="inc">Inc</PrimaryButton>
  </Column>
</template>
<script lang="ts">
import { reactive, computed, watch } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ n: 0, hits: [] })
    const double = computed(() => state.n * 2)
    watch(() => double.value, (v) => { state.hits.push(String(v)) })
    function inc() { state.n++ }
    return { state, inc }
  },
}
</script>
`

// TestComputedWatchRunsInSameFlush:computed 的下游 watch 必须在同一轮 flush 里跑完,
// 不能依赖 1ms 兜底定时器补跑(否则一次变更会渲染两次)。
func TestComputedWatchRunsInSameFlush(t *testing.T) {
	pkg := mustCompile(t, "CW.aui", map[string]string{"CW.aui": computedWatchAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "CW", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "-") }, "首帧未渲染")
	s0, _ := r.Stats()

	btn := findTagID(b.Tree("main"), "PrimaryButton", "Inc")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("点击派发失败")
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "2") }, "computed 下游 watch 未生效")
	time.Sleep(120 * time.Millisecond)
	s1, _ := r.Stats()
	if delta := s1["renders"] - s0["renders"]; delta != 1 {
		t.Fatalf("一次变更触发了 %v 次渲染(computed watch 被拆到第二轮),want 1", delta)
	}
}

const renderOnceAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.n }}</Text>
    <PrimaryButton a-on:click="inc">Inc</PrimaryButton>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ n: 0 })
    function inc() { state.n++ }
    return { state, inc }
  },
}
</script>
`

// TestStateChangeRendersOnce:一次状态变更只安排一次渲染 pass
// (微任务 flush 与 1ms 兜底定时器不能各通知一次)。
func TestStateChangeRendersOnce(t *testing.T) {
	pkg := mustCompile(t, "Once.aui", map[string]string{"Once.aui": renderOnceAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Once", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "0") }, "首帧未渲染")

	s0, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	btn := findTagID(b.Tree("main"), "PrimaryButton", "Inc")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("点击派发失败")
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "1") }, "状态未更新")
	time.Sleep(120 * time.Millisecond) // 让兜底定时器有机会再次 notify

	s1, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if delta := s1["renders"] - s0["renders"]; delta != 1 {
		t.Fatalf("一次变更触发了 %v 次渲染 pass(渲染计数 %v → %v),want 1", delta, s0["renders"], s1["renders"])
	}
}

const preserveParentAUI = `
<template>
  <Column gap="4">
    <Text>{{ boom() }}</Text>
    <Child a-prop:n="state.n" />
  </Column>
</template>
<script lang="ts">
import Child from './Child'
import { reactive } from 'ada:surface'
export default {
  name: 'PreserveParent',
  components: { Child },
  setup(props, ctx) {
    const state = reactive({ explode: false, n: 1 })
    function boom() {
      if (state.explode) throw new Error('瞬时渲染错误')
      return 'ok'
    }
    ctx.on('explode', () => { state.explode = true })
    ctx.on('recover', () => { state.explode = false })
    return { state, boom }
  },
}
</script>
`

const preserveChildAUI = `
<meta>
{ "name": "Child" }
</meta>
<template>
  <Column><Text>{{ state.mark }}</Text></Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  name: 'Child',
  setup(props, ctx) {
    ctx.api.call('probe', 'setup')
    const state = reactive({ mark: 'child-alive' })
    return { state }
  },
}
</script>
`

// TestRenderErrorKeepsChildInstances:根树渲染抛错时不能清扫子实例,
// 恢复后子组件内部状态与 setup 次数都应保持。
func TestRenderErrorKeepsChildInstances(t *testing.T) {
	pkg := mustCompile(t, "PreserveParent.aui", map[string]string{
		"PreserveParent.aui": preserveParentAUI,
		"Child.aui":          preserveChildAUI,
	})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	var setups int32
	if err := r.Expose("probe", "setup", func() { atomic.AddInt32(&setups, 1) }); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "PreserveParent", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "child-alive") }, "子组件未渲染")
	if got := atomic.LoadInt32(&setups); got != 1 {
		t.Fatalf("子组件 setup 次数 = %d, want 1", got)
	}

	r.Notify("explode", nil)
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && strings.Contains(tree.Dump(), "渲染出错")
	}, "瞬时渲染错误未发生")

	stats, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats["instances"] < 2 {
		t.Fatalf("渲染错误后子实例被销毁: instances=%v, want >=2", stats["instances"])
	}

	r.Notify("recover", nil)
	waitFor(t, func() bool { return treeHasText(b, "main", "child-alive") }, "恢复后子组件未重新渲染")
	if got := atomic.LoadInt32(&setups); got != 1 {
		t.Fatalf("恢复后子组件被重建(setup 次数 = %d)", got)
	}
}

const hmrDepHostAUI = `
<meta>
{ "name": "DepHost" }
</meta>
<template>
  <Column><Text>{{ state.v }}</Text></Column>
</template>
<script lang="ts">
import { value } from './dep'
import { reactive } from 'ada:surface'
export default {
  name: 'DepHost',
  setup() {
    const state = reactive({ v: value() })
    return { state }
  },
}
</script>
`

const hmrDepV1 = `export function value(): string { return 'dep-v1' }`
const hmrDepV2 = `export function value(): string { return 'dep-v2' }`

// TestHMRPropagatesDependencyChange:只改被导入的 .ts、组件脚本不变时,
// 组件也必须重新 adopt(否则 require 仍拿到旧 exports)。
func TestHMRPropagatesDependencyChange(t *testing.T) {
	pkg1 := mustCompile(t, "DepHost.aui", map[string]string{
		"DepHost.aui": hmrDepHostAUI,
		"dep.ts":      hmrDepV1,
	})
	b := headless.New()
	r, err := rt.New(pkg1, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "DepHost", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "dep-v1") }, "首帧未渲染")

	pkg2 := mustCompile(t, "DepHost.aui", map[string]string{
		"DepHost.aui": hmrDepHostAUI, // 脚本完全不变
		"dep.ts":      hmrDepV2,
	})
	if err := r.HotReload(pkg2); err != nil {
		t.Fatalf("HotReload: %v", err)
	}
	waitFor(t, func() bool { return treeHasText(b, "main", "dep-v2") }, "依赖模块变更未传播到组件(仍是旧 exports)")
}

// TestCloseDoesNotHangInFlightCalls:Close 与在途 Stats 并发时不能永久阻塞。
func TestCloseDoesNotHangInFlightCalls(t *testing.T) {
	pkg := mustCompile(t, "Once.aui", map[string]string{"Once.aui": renderOnceAUI})
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	if _, err := r.MountWindow("main", "Once", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			_, _ = r.Stats()
		}
		close(done)
	}()
	time.Sleep(2 * time.Millisecond)
	_ = r.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close 期间 Stats 永久阻塞")
	}
}
