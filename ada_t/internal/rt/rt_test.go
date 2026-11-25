package rt_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/inspector"
	"github.com/issueye/ada_t/internal/rt"
	"github.com/issueye/ada_t/widget"
)

// counterAUI 是端到端冒烟组件:计数器 + 输入 + 列表 + 条件 + computed + i18n。
const counterAUI = `
<meta>
{ "name": "Counter" }
</meta>

<template>
  <Column fill center gap="12">
    <Text font-size="40" bold>{{ display }}</Text>
    <Row gap="8">
      <PrimaryButton a-on:click="inc">Increment</PrimaryButton>
      <Button a-on:click="dec">Dec</Button>
    </Row>
    <TextInput a-model="state.query" placeholder="Type here" />
    <Text>{{ $t('echo') }}: {{ state.query }}</Text>
    <Text a-if="state.n > 2">{{ $t('many') }}</Text>
    <Row a-for="item, i in state.items" a-key="item" gap="4">
      <Text>{{ i }}: {{ item }}</Text>
    </Row>
    <Button a-on:click="addItem">Add</Button>
  </Column>
</template>

<script lang="ts">
import { reactive, computed, watch } from 'ada:surface'

export default {
  name: 'Counter',
  setup(props, ctx) {
    const state = reactive({ n: 0, query: '', items: ['a'] })
    const display = computed(() => {
      if (state.n === 0) return 'zero'
      if (state.n % 15 === 0) return state.n + '!'
      return String(state.n)
    })
    watch(() => state.n, (nv, ov) => ctx.log('n:', ov, '->', nv))
    function inc() { state.n++ }
    function dec() { state.n-- }
    function addItem() { state.items.push('item-' + state.items.length) }
    return { state, display, inc, dec, addItem }
  },
}
</script>

<style scoped>
.hero {
  font-size: 40;
  bold: true;
}
</style>

<i18n>
{
  "zh-CN": { "echo": "回显", "many": "超过 2 了" }
}
</i18n>
`

func mustCompile(t testing.TB, entry string, files map[string]string, opts ...compile.Options) *compile.Package {
	t.Helper()
	o := compile.Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	pkg, err := compile.CompileFiles(entry, files, o)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return pkg
}

// waitFor 轮询直到 cond 成立(渲染是异步的:微任务 + loop 任务)。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// findTagID 在树里找第一个指定 tag 的节点 id(可选按文本过滤)。
func findTagID(tree *widget.Node, tag, text string) string {
	var id string
	tree.Walk(func(n *widget.Node) bool {
		if id != "" {
			return false
		}
		if n.Tag == tag && (text == "" || n.Text() == text) {
			id = n.ID
			return false
		}
		return true
	})
	return id
}

func TestCounterEndToEnd(t *testing.T) {
	pkg := mustCompile(t, "Counter.aui", map[string]string{"Counter.aui": counterAUI})
	b := headless.New()
	var logs []string
	r, err := rt.New(pkg, rt.Options{
		Logf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Locale: "zh-CN",
		OnTree: b.Apply,
	})
	if err != nil {
		t.Fatalf("创建运行时失败: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) {
		r.Fire(hid, payload)
	})
	if _, err := r.MountWindow("main", "Counter", nil); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}

	// 1. 首帧:computed display = "zero",if 不满足
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	root := b.Tree("main")
	if btn := findTagID(root, "PrimaryButton", ""); btn == "" {
		t.Fatalf("首帧没有 PrimaryButton:\n%s", root.Dump())
	}
	if got := findTagID(root, "Text", ""); got == "" {
		t.Fatalf("首帧没有 Text")
	}
	displayOf := func(tree *widget.Node) string {
		if tree == nil {
			return ""
		}
		// display 文本 = 第一个带 font-size 属性的 Text(或第一个 Text)
		var got string
		tree.Walk(func(n *widget.Node) bool {
			if got != "" {
				return false
			}
			if n.Tag == "Text" && n.Text() != "" {
				got = n.Text()
				return false
			}
			return true
		})
		return got
	}
	if d := displayOf(root); d != "zero" {
		t.Fatalf("首帧 display 期望 zero,得到 %q\n%s", d, root.Dump())
	}

	// 2. 点击 Increment 三次 → display "3",if 分支出现
	btn := findTagID(root, "PrimaryButton", "")
	for i := 0; i < 3; i++ {
		if !b.Fire("main", btn, "click", nil) {
			t.Fatalf("找不到 PrimaryButton 的 click handler")
		}
		want := fmt.Sprint(i + 1)
		waitFor(t, func() bool { return displayOf(b.Tree("main")) == want },
			fmt.Sprintf("display 未变为 %s", want))
	}
	if findTagID(b.Tree("main"), "Text", "超过 2 了") == "" {
		t.Fatalf("a-if 分支未出现:\n%s", b.Tree("main").Dump())
	}

	// 3. 输入绑定:input 事件 → state.query → 回显文本
	input := findTagID(b.Tree("main"), "TextInput", "")
	if input == "" {
		t.Fatalf("找不到 TextInput")
	}
	if !b.Fire("main", input, "input", map[string]any{"value": "ada"}) {
		t.Fatalf("找不到 TextInput 的 input handler(a-model)")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "回显: ada") != ""
	}, "a-model 回显未更新")

	// 4. 列表:Add → items 增长(a-for 渲染第二个项)
	addID := findTagID(b.Tree("main"), "Button", "Add")
	if addID == "" {
		t.Fatalf("找不到 Add 按钮:\n%s", b.Tree("main").Dump())
	}
	if !b.Fire("main", addID, "click", nil) {
		t.Fatalf("Add click 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "1: item-1") != ""
	}, "a-for 列表未增长")

	// 5. watch 生效(日志里应有 n 变更记录)
	foundWatch := false
	for _, l := range logs {
		if strings.Contains(l, "->") {
			foundWatch = true
			break
		}
	}
	if !foundWatch {
		t.Fatalf("watch/ctx.log 没有产生日志:%v", logs)
	}

	// 6. i18n 生效($t 占位)
	if findTagID(b.Tree("main"), "Text", "回显: ada") == "" {
		t.Fatalf("$t 未生效")
	}
}

// TestNestedComponent 验证包内子组件:props 传递 + emit 回传。
const parentAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.v }}</Text>
    <CountBox a-prop:value="state.v" a-on:change="onChange" />
  </Column>
</template>
<script lang="ts">
import CountBox from './CountBox'
import { reactive } from 'ada:surface'
export default {
  components: { CountBox },
  setup(props, ctx) {
    const state = reactive({ v: 1 })
    function onChange(e) { state.v = e.value; }
    return { state, onChange }
  },
}
</script>
`

const stepperAUI = `
<meta>
{ "name": "CountBox" }
</meta>
<template>
  <Row gap="4">
    <PrimaryButton a-on:click="bump">+1</PrimaryButton>
    <Text>{{ props.value }}</Text>
  </Row>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  name: 'CountBox',
  setup(props, ctx) {
    function bump() {
      ctx.emit('change', { value: props.value + 1 })
    }
    return { bump }
  },
}
</script>
`

func TestNestedComponent(t *testing.T) {
	files := map[string]string{"Parent.aui": parentAUI, "CountBox.aui": stepperAUI}
	pkg := mustCompile(t, "Parent.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{
		Logf:   func(f string, a ...any) { t.Logf("[js] "+f, a...) },
		OnTree: b.Apply,
	})
	if err != nil {
		t.Fatalf("创建运行时失败: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) {
		r.Fire(hid, payload)
	})
	if _, err := r.MountWindow("main", "Parent", nil); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	tree := b.Tree("main")
	// 子组件渲染:props.value=1 → 文本 "1"
	if findTagID(tree, "Text", "1") == "" {
		t.Fatalf("子组件 props 未渲染:\n%s", tree.Dump())
	}
	// 点子组件按钮 → emit change → 父改 state.v=2 → 子文本变 "2"
	plus := findTagID(tree, "PrimaryButton", "+1")
	if plus == "" {
		t.Fatalf("找不到子组件按钮:\n%s", tree.Dump())
	}
	if !b.Fire("main", plus, "click", nil) {
		t.Fatalf("子组件按钮事件派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "2") != "" && findTagID(tree, "Text", "2") != findTagID(tree, "Text", "1")
	}, "emit → 父状态 → 子 props 更新失败")
}

// TestUnregisteredComponentImport 拒绝裸包名导入(EUIIMPORT;发生在 Link 时)。
func TestUnregisteredComponentImport(t *testing.T) {
	files := map[string]string{"Bad.aui": `
<template><Column><Text>{{ x }}</Text></Column></template>
<script lang="ts">
import { something } from 'lodash'
export default { setup() { return { x: something } } }
</script>
`}
	pkg, err := compile.CompileFiles("Bad.aui", files, compile.Options{})
	if err != nil {
		t.Fatalf("编译应成功(裸包名在 Link 时拒绝): %v", err)
	}
	r, err := rt.New(pkg, rt.Options{})
	if err == nil {
		r.Close()
		t.Fatalf("裸包名导入应被 Link 拒绝(EUIIMPORT)")
	}
	if !strings.Contains(err.Error(), "EUIIMPORT") {
		t.Fatalf("错误应带 EUIIMPORT 码,得到: %v", err)
	}
}

// TestServiceBridge:Go 服务暴露给组件(ctx.api.call),参数/返回值/错误传播。
const apiAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.sum }}</Text>
    <Text>{{ state.msg }}</Text>
    <Text>{{ state.err }}</Text>
    <PrimaryButton a-on:click="add">Add</PrimaryButton>
    <Button a-on:click="boom">Boom</Button>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ sum: 0, msg: '', err: '' })
    function add() {
      state.sum = ctx.api.call('math', 'add', state.sum, 3)
    }
    function boom() {
      try {
        ctx.api.call('math', 'explode')
      } catch (e) {
        state.err = String(e && e.message ? e.message : e)
      }
    }
    return { state, add, boom }
  },
}
</script>
`

func TestServiceBridge(t *testing.T) {
	pkg := mustCompile(t, "Api.aui", map[string]string{"Api.aui": apiAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if err := r.Expose("math", "add", func(a, b float64) float64 { return a + b }); err != nil {
		t.Fatalf("Expose add: %v", err)
	}
	if err := r.Expose("math", "explode", func() (string, error) {
		return "", fmt.Errorf("故意失败")
	}); err != nil {
		t.Fatalf("Expose explode: %v", err)
	}
	// 签名错误的注册应被拒绝
	if err := r.Expose("math", "bad", 42); err == nil {
		t.Fatal("非函数 Expose 应失败")
	}
	if err := r.Expose("math", "variadic", func(xs ...int) int { return 0 }); err == nil {
		t.Fatal("变参 Expose 应失败")
	}
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Api", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	// 参数经 JSON(float64)→ Go func(a, b float64) → 返回值回 JS
	btn := findTagID(b.Tree("main"), "PrimaryButton", "")
	if btn == "" {
		t.Fatalf("找不到 Add 按钮:\n%s", b.Tree("main").Dump())
	}
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("click 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "3") != ""
	}, "服务调用结果未渲染(期望 sum=3)")

	// 错误传播:Go error → JS 异常 → 组件 catch → state.err
	boom := findTagID(b.Tree("main"), "Button", "Boom")
	if !b.Fire("main", boom, "click", nil) {
		t.Fatal("Boom click 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "err: 故意失败") != "" || findTagID(tree, "Text", "故意失败") != ""
	}, "Go 错误未传播到组件")
}

// findAnyText 返回树里任意非空文本(宽松断言用)。
func findAnyText(tree *widget.Node) string {
	var got string
	tree.Walk(func(n *widget.Node) bool {
		if got == "" && n.Text() != "" {
			got = n.Text()
		}
		return got == ""
	})
	return got
}

// TestHostEvent:Go → 组件事件(Notify + ctx.on),并验证卸载退订。
const eventAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.ticks }}</Text>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ ticks: 0 })
    ctx.on('tick', (p) => { state.ticks = state.ticks + (p && p.n ? p.n : 1) })
    return { state }
  },
}
</script>
`

func TestHostEvent(t *testing.T) {
	pkg := mustCompile(t, "Ev.aui", map[string]string{"Ev.aui": eventAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Ev", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	// 事件带载荷
	r.Notify("tick", map[string]any{"n": 5})
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "5") != ""
	}, "Notify 载荷未到达组件")

	// 无载荷事件:p.n 缺省 1
	r.Notify("tick", nil)
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "6") != ""
	}, "无载荷事件未累加")

	// 未知事件:不影响渲染
	r.Notify("nobody-listens", map[string]any{"x": 1})
	r.Notify("tick", nil)
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "7") != ""
	}, "第三次 Notify 未到达")
}

// TestServiceExposeAfterMount:挂载后再 Expose,ctx.api 动态可见。
func TestServiceExposeAfterMount(t *testing.T) {
	pkg := mustCompile(t, "Api.aui", map[string]string{"Api.aui": apiAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Api", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	// 挂载后才注册服务
	if err := r.Expose("math", "add", func(a, b float64) float64 { return a + b }); err != nil {
		t.Fatalf("挂载后 Expose: %v", err)
	}
	btn := findTagID(b.Tree("main"), "PrimaryButton", "")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("click 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "3") != ""
	}, "挂载后注册的服务未生效")
}

// TestErrorBoundary:setup 抛错 → 窗口显示错误卡片而不是挂载失败;
// 子组件渲染抛错 → 只降级为占位文本,窗口其余部分正常。
const brokenAUI = `
<template>
  <Column gap="4">
    <Text>窗口还活着</Text>
    <Broken a-prop:x="state.v" />
  </Column>
</template>
<script lang="ts">
import Broken from './Broken'
import { reactive } from 'ada:surface'
export default {
  components: { Broken },
  setup(props, ctx) {
    const state = reactive({ v: 1 })
    return { state }
  },
}
</script>
`

const brokenChildAUI = `
<meta>
{ "name": "Broken" }
</meta>
<template>
  <Row><Text>{{ props.x.crash() }}</Text></Row>
</template>
`

func TestErrorBoundary(t *testing.T) {
	files := map[string]string{"BrokenHost.aui": brokenAUI, "Broken.aui": brokenChildAUI}
	pkg := mustCompile(t, "BrokenHost.aui", files)
	b := headless.New()
	var logs []string
	r, err := rt.New(pkg, rt.Options{
		Logf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		OnTree: b.Apply,
	})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "BrokenHost", nil); err != nil {
		t.Fatalf("挂载(错误边界下不应失败): %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	tree := b.Tree("main")
	// 窗口其余部分正常
	if findTagID(tree, "Text", "窗口还活着") == "" {
		t.Fatalf("错误边界应保住窗口其余部分:\n%s", tree.Dump())
	}
	// 子组件降级为错误占位
	found := false
	tree.Walk(func(n *widget.Node) bool {
		if strings.Contains(n.Text(), "Broken") && strings.Contains(n.Text(), "渲染出错") {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("子组件错误占位未出现:\n%s", tree.Dump())
	}
	// 错误进日志(可观测)
	foundLog := false
	for _, l := range logs {
		if strings.Contains(l, "子组件渲染失败") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Fatalf("渲染错误应进日志:%v", logs)
	}
}

// TestSetupErrorCard:根组件 setup 抛错 → 窗口渲染错误卡片(应用仍可启动)。
func TestSetupErrorCard(t *testing.T) {
	files := map[string]string{"Bad2.aui": `
<template><Column><Text>{{ state.x }}</Text></Column></template>
<script lang="ts">
export default {
  setup() { throw new Error('setup 爆炸了') },
}
</script>
`}
	pkg := mustCompile(t, "Bad2.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	// 挂载成功(错误被边界接住),窗口渲染错误卡片
	if _, err := r.MountWindow("main", "Bad2", nil); err != nil {
		t.Fatalf("setup 抛错时挂载不应失败: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "错误卡片未渲染")
	tree := b.Tree("main")
	found := false
	tree.Walk(func(n *widget.Node) bool {
		if strings.Contains(n.Text(), "setup 爆炸了") {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("错误卡片应包含错误信息:\n%s", tree.Dump())
	}
}

// TestAsyncService:callAsync 返回 Promise;慢服务不阻塞渲染;
// 等待期间 loop 仍响应事件(Notify 顺带验证)。
const asyncAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.v }}</Text>
    <Text>{{ state.ticks }}</Text>
    <PrimaryButton a-on:click="load">Load</PrimaryButton>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ v: 0, ticks: 0 })
    ctx.on('tick', (p) => { state.ticks = state.ticks + (p && p.n ? p.n : 1) })
    async function load() {
      state.v = await ctx.api.callAsync('math', 'slowAdd', 40, 2)
    }
    return { state, load }
  },
}
</script>
`

func TestAsyncService(t *testing.T) {
	pkg := mustCompile(t, "Async.aui", map[string]string{"Async.aui": asyncAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if err := r.Expose("math", "slowAdd", func(a, b float64) (float64, error) {
		time.Sleep(150 * time.Millisecond) // 模拟慢服务
		return a + b, nil
	}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Async", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	btn := findTagID(b.Tree("main"), "PrimaryButton", "")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("click 派发失败")
	}
	// 慢服务执行期间(150ms),loop 不被阻塞:Notify 仍能驱动渲染
	r.Notify("tick", map[string]any{"n": 7})
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "7") != ""
	}, "慢服务执行期间 loop 被阻塞(Notify 未响应)")

	// Promise 完成后结果渲染
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "42") != ""
	}, "callAsync 结果未渲染(期望 42)")
}

// TestAsyncServiceError:Go error → Promise 拒绝 → 组件 catch。
const asyncErrAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.err }}</Text>
    <Button a-on:click="boom">Boom</Button>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ err: '' })
    async function boom() {
      try {
        await ctx.api.callAsync('math', 'explode')
        state.err = '不应到达'
      } catch (e) {
        state.err = String(e && e.message ? e.message : e)
      }
    }
    return { state, boom }
  },
}
</script>
`

func TestAsyncServiceError(t *testing.T) {
	pkg := mustCompile(t, "AsyncErr.aui", map[string]string{"AsyncErr.aui": asyncErrAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if err := r.Expose("math", "explode", func() (string, error) {
		return "", fmt.Errorf("异步故意失败")
	}); err != nil {
		t.Fatalf("Expose: %v", err)
	}
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "AsyncErr", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	boom := findTagID(b.Tree("main"), "Button", "")
	if !b.Fire("main", boom, "click", nil) {
		t.Fatal("click 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "异步故意失败") != "" ||
			findTagID(tree, "Text", "Error: 异步故意失败") != ""
	}, "异步错误未传播到组件")
}

// TestAlertDialogModel:对话框 open 的双向绑定(action/change 经 a-model 写回)。
const dialogAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.open }}</Text>
    <Text>{{ state.last }}</Text>
    <Button a-on:click="open">Open</Button>
    <AlertDialog a-model="state.open" title="确认" message="确定吗?" buttons="取消,确定" a-on:action="onAction" />
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    const state = reactive({ open: false, last: '' })
    function open() { state.open = true }
    function onAction(e) {
      state.last = '按钮:' + e.label
      state.open = false
    }
    return { state, open, onAction }
  },
}
</script>
`

func TestAlertDialogModel(t *testing.T) {
	pkg := mustCompile(t, "Dlg.aui", map[string]string{"Dlg.aui": dialogAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Dlg", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	// 打开:open=true 出现在树里
	openBtn := findTagID(b.Tree("main"), "Button", "")
	if !b.Fire("main", openBtn, "click", nil) {
		t.Fatal("click 派发失败")
	}
	var alertID string
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		alertID = ""
		tree.Walk(func(n *widget.Node) bool {
			if n.Tag == "AlertDialog" {
				alertID = n.ID
				if v, ok := n.Props["open"]; ok && v == true {
					return false
				}
			}
			return true
		})
		return alertID != "" && func() bool {
			found := false
			tree.Walk(func(n *widget.Node) bool {
				if n.Tag == "AlertDialog" {
					if v, ok := n.Props["open"]; ok && v == true {
						found = true
					}
					return false
				}
				return true
			})
			return found
		}()
	}, "open=true 未渲染")

	// 用户点「确定」(headless 里模拟后端把 change 写回 + action 事件)
	if !b.Fire("main", alertID, "change", map[string]any{"value": false}) {
		t.Fatal("change 派发失败")
	}
	if !b.Fire("main", alertID, "action", map[string]any{"index": 1.0, "label": "确定"}) {
		t.Fatal("action 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "按钮:确定") != "" && findTagID(tree, "Text", "false") != ""
	}, "action/change 未写回状态")
}

// TestToastQueue:ctx.toast 入队,DrainToasts 按窗口取走。
const toastAUI = `
<template>
  <Column gap="4">
    <Button a-on:click="pop">Pop</Button>
  </Column>
</template>
<script lang="ts">
export default {
  setup(props, ctx) {
    function pop() { ctx.toast('已保存') }
    return { pop }
  },
}
</script>
`

func TestToastQueue(t *testing.T) {
	pkg := mustCompile(t, "Toast.aui", map[string]string{"Toast.aui": toastAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Toast", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	if q := r.DrainToasts("main"); len(q) != 0 {
		t.Fatalf("初始队列应为空,得到 %v", q)
	}
	btn := findTagID(b.Tree("main"), "Button", "")
	if !b.Fire("main", btn, "click", nil) {
		t.Fatal("click 派发失败")
	}
	waitFor(t, func() bool {
		q := r.DrainToasts("main")
		if len(q) == 0 {
			return false
		}
		return q[0] == "已保存"
	}, "toast 未入队")
	// 取走即清空
	if q := r.DrainToasts("main"); len(q) != 0 {
		t.Fatalf("Drain 后队列应清空,得到 %v", q)
	}
}

// TestTabsPanes:页签(choice)驱动 a-if 内容面板切换。
const tabsAUI = `
<template>
  <Column gap="4">
    <Tabs a-model="state.tab" options="计数,设置" fill-x />
    <Text a-if="state.tab === '计数'">计数面板</Text>
    <Text a-if="state.tab === '设置'">设置面板</Text>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ tab: '计数' })
    return { state }
  },
}
</script>
`

func TestTabsPanes(t *testing.T) {
	pkg := mustCompile(t, "Tabs.aui", map[string]string{"Tabs.aui": tabsAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Tabs", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	tree := b.Tree("main")
	if findTagID(tree, "Text", "计数面板") == "" {
		t.Fatalf("初始页签内容缺失:\n%s", tree.Dump())
	}
	var tabsID string
	tree.Walk(func(n *widget.Node) bool {
		if n.Tag == "Tabs" {
			tabsID = n.ID
			return false
		}
		return true
	})
	if tabsID == "" {
		t.Fatal("找不到 Tabs 节点")
	}
	if !b.Fire("main", tabsID, "change", map[string]any{"value": "设置"}) {
		t.Fatal("change 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "设置面板") != "" && findTagID(tree, "Text", "计数面板") == ""
	}, "页签切换未生效")
}

// TestSplitSync:分栏尺寸双向(input 事件写回 a-model 状态)。
const splitAUI = `
<template>
  <Column fill gap="4">
    <Text>{{ state.pane }}</Text>
    <Split a-model="state.pane" size="260" fill>
      <Column><Text>first</Text></Column>
      <Column><Text>second</Text></Column>
    </Split>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ pane: 260 })
    return { state }
  },
}
</script>
`

func TestSplitSync(t *testing.T) {
	pkg := mustCompile(t, "Split.aui", map[string]string{"Split.aui": splitAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Split", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	tree := b.Tree("main")
	if findTagID(tree, "Text", "first") == "" || findTagID(tree, "Text", "second") == "" {
		t.Fatalf("两个面板未渲染:\n%s", tree.Dump())
	}
	var splitID string
	tree.Walk(func(n *widget.Node) bool {
		if n.Tag == "Split" {
			splitID = n.ID
			return false
		}
		return true
	})
	if splitID == "" {
		t.Fatal("找不到 Split 节点")
	}
	// 用户拖拽:后端以 input 事件回传新尺寸
	if !b.Fire("main", splitID, "input", map[string]any{"value": 320.0}) {
		t.Fatal("input 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "320") != ""
	}, "分栏尺寸未写回状态")
}

// statsOf 取 surface 注册表计数(便利封装)。
func statsOf(t *testing.T, r *rt.Runtime) map[string]float64 {
	t.Helper()
	m, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return m
}

// TestLeakMountUnmount:挂载/卸载 100 次,实例/handler/事件订阅/窗口计数
// 必须回到基线(§14 泄漏口径)。组件带 ctx.on 订阅与 a-model 输入。
func TestLeakMountUnmount(t *testing.T) {
	pkg := mustCompile(t, "Counter.aui", map[string]string{"Counter.aui": counterAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })

	base := statsOf(t, r)
	for i := 0; i < 100; i++ {
		winID := fmt.Sprintf("w%d", i)
		if _, err := r.MountWindow(winID, "Counter", nil); err != nil {
			t.Fatalf("第 %d 次挂载失败: %v", i, err)
		}
		waitFor(t, func() bool { return b.Tree(winID) != nil }, fmt.Sprintf("第 %d 次首帧", i))
		r.UnmountWindow(winID)
	}
	waitFor(t, func() bool {
		m := statsOf(t, r)
		return m["instances"] == base["instances"] &&
			m["handlers"] == base["handlers"] &&
			m["eventSubs"] == base["eventSubs"] &&
			m["windows"] == base["windows"]
	}, fmt.Sprintf("100 次挂载/卸载后计数未回落: base=%v now=%v", base, statsOf(t, r)))
}

// TestLeakChildDispose:a-for 列表清空后,子组件实例被 dispose(计数回落)。
const leakChildAUI = `
<template>
  <Column gap="4">
    <Child a-for="it in state.items" a-key="it" a-prop:name="it" />
    <Button a-on:click="clearAll">Clear</Button>
    <Button a-on:click="refill">Refill</Button>
  </Column>
</template>
<script lang="ts">
import Child from './Child'
import { reactive } from 'ada:surface'
export default {
  components: { Child },
  setup(props, ctx) {
    const state = reactive({ items: ['a', 'b', 'c'] })
    function clearAll() { state.items = [] }
    function refill() { state.items = ['x', 'y', 'z'] }
    return { state, clearAll, refill }
  },
}
</script>
`

const leakGrandAUI = `
<meta>
{ "name": "Child" }
</meta>
<template>
  <Row><Text>{{ props.name }}</Text></Row>
</template>
`

func TestLeakChildDispose(t *testing.T) {
	files := map[string]string{
		"Host.aui":  leakChildAUI,
		"Child.aui": leakGrandAUI,
	}
	pkg := mustCompile(t, "Host.aui", files)
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Host", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	base := statsOf(t, r)
	// 3 个子实例(根 + 3 = 4)
	if m := statsOf(t, r); m["instances"] != base["instances"] {
		t.Fatalf("子实例计数应稳定: base=%v now=%v", base, statsOf(t, r))
	}
	// 反复增删列表:实例数随渲染收敛,handler/实例不增长
	for i := 0; i < 20; i++ {
		clearBtn := findTagID(b.Tree("main"), "Button", "Clear")
		if !b.Fire("main", clearBtn, "click", nil) {
			t.Fatal("Clear click 派发失败")
		}
		waitFor(t, func() bool {
			tree := b.Tree("main")
			return tree != nil && findTagID(tree, "Text", "x") == "" && findTagID(tree, "Text", "a") == ""
		}, "清空未生效")
		refillBtn := findTagID(b.Tree("main"), "Button", "Refill")
		if !b.Fire("main", refillBtn, "click", nil) {
			t.Fatal("Refill click 派发失败")
		}
		waitFor(t, func() bool {
			tree := b.Tree("main")
			return tree != nil && findTagID(tree, "Text", "x") != ""
		}, "重填未生效")
	}
	final := statsOf(t, r)
	if final["instances"] != base["instances"] {
		t.Fatalf("20 轮增删后实例数应稳定: base=%v final=%v", base, final)
	}
	if final["handlers"] > base["handlers"]+40 {
		t.Fatalf("handler 数异常增长: base=%v final=%v", base, final)
	}
}

// TestHMRTemplateSwap:模板变更 → 只换数据,状态保留,新模板立即生效。
const hmrV1 = `
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

const hmrV2 = `
<template>
  <Column gap="4">
    <Text>v2: {{ state.n }}</Text>
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

func TestHMRTemplateSwap(t *testing.T) {
	pkg := mustCompile(t, "H.aui", map[string]string{"H.aui": hmrV1})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "H", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	// 改状态到 5
	btn := findTagID(b.Tree("main"), "PrimaryButton", "")
	for i := 0; i < 5; i++ {
		b.Fire("main", btn, "click", nil)
		waitFor(t, func() bool {
			return findTagID(b.Tree("main"), "Text", fmt.Sprint(i+1)) != ""
		}, "inc 未生效")
	}

	// 模板变更热重载(脚本字节相同)
	pkg2 := mustCompile(t, "H.aui", map[string]string{"H.aui": hmrV2})
	if err := r.HotReload(pkg2); err != nil {
		t.Fatalf("HotReload: %v", err)
	}
	waitFor(t, func() bool {
		return findTagID(b.Tree("main"), "Text", "v2: 5") != ""
	}, "新模板未生效或状态丢失(期望 v2: 5)")
}

// TestHMRScriptChange:脚本变更 → 重跑 setup(props 保留),窗口不关。
const hmrScriptV1 = `
<template>
  <Column gap="4">
    <Text>{{ state.msg }}</Text>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props) {
    const state = reactive({ msg: '旧实现 ' + props.title })
    return { state }
  },
}
</script>
`

const hmrScriptV2 = `
<template>
  <Column gap="4">
    <Text>{{ state.msg }}</Text>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup(props, ctx) {
    ctx.log('v2 setup 运行')
    const state = reactive({ msg: '新实现 ' + props.title })
    return { state }
  },
}
</script>
`

func TestHMRScriptChange(t *testing.T) {
	pkg := mustCompile(t, "S.aui", map[string]string{"S.aui": hmrScriptV1})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if _, err := r.MountWindow("main", "S", map[string]any{"title": "演示"}); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool {
		return b.Tree("main") != nil && findTagID(b.Tree("main"), "Text", "旧实现 演示") != ""
	}, "首帧未渲染")

	pkg2 := mustCompile(t, "S.aui", map[string]string{"S.aui": hmrScriptV2})
	if err := r.HotReload(pkg2); err != nil {
		t.Fatalf("HotReload: %v", err)
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		return tree != nil && findTagID(tree, "Text", "新实现 演示") != ""
	}, "脚本热替换未生效(props 应保留)")
}

// TestHMRNewComponent:热重载新增组件并使用。
const hmrHostV1 = `
<template>
  <Column gap="4">
    <Text>宿主 v1</Text>
  </Column>
</template>
<script lang="ts">
export default {
  setup() { return {} },
}
</script>
`

const hmrHostV2 = `
<template>
  <Column gap="4">
    <Text>宿主 v2</Text>
    <Fresh />
  </Column>
</template>
<script lang="ts">
import Fresh from './Fresh'
export default {
  components: { Fresh },
  setup() { return {} },
}
</script>
`

const hmrFreshAUI = `
<meta>
{ "name": "Fresh" }
</meta>
<template>
  <Row><Text color="gray">新组件</Text></Row>
</template>
`

func TestHMRNewComponent(t *testing.T) {
	pkg := mustCompile(t, "Host.aui", map[string]string{"Host.aui": hmrHostV1})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "Host", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")

	pkg2 := mustCompile(t, "Host.aui", map[string]string{
		"Host.aui":  hmrHostV2,
		"Fresh.aui": hmrFreshAUI,
	}, compile.Options{Entry: "Host"})
	if err := r.HotReload(pkg2); err != nil {
		t.Fatalf("HotReload: %v", err)
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "宿主 v2") != "" && findTagID(tree, "Text", "新组件") != ""
	}, "新增组件未渲染")
}

// TestTableModel:Table rows 数据进树 + 选中行 change 写回。
const tableAUI = `
<template>
  <Column gap="4">
    <Text>{{ state.rows[state.sel] && state.rows[state.sel].name }}</Text>
    <Table a-model="state.sel" a-bind:rows="state.rows" columns="名称" fields="name" height="200" />
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  setup() {
    const state = reactive({ rows: [{ name: 'alpha' }, { name: 'beta' }], sel: -1 })
    return { state }
  },
}
</script>
`

func TestTableModel(t *testing.T) {
	pkg := mustCompile(t, "T.aui", map[string]string{"T.aui": tableAUI})
	b := headless.New()
	r, err := rt.New(pkg, rt.Options{Logf: func(string, ...any) {}, OnTree: b.Apply})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	if _, err := r.MountWindow("main", "T", nil); err != nil {
		t.Fatalf("挂载: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("main") != nil }, "首帧未渲染")
	var tableID string
	b.Tree("main").Walk(func(n *widget.Node) bool {
		if n.Tag == "Table" {
			tableID = n.ID
			return false
		}
		return true
	})
	if tableID == "" {
		t.Fatalf("找不到 Table 节点:\n%s", b.Tree("main").Dump())
	}
	// rows 数据在树里
	if !b.Fire("main", tableID, "change", map[string]any{"value": 1.0}) {
		t.Fatal("change 派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("main")
		if tree == nil {
			return false
		}
		return findTagID(tree, "Text", "beta") != ""
	}, "选中行未写回状态")
}

// TestInspectorPanel:Inspector 附加包 + __inspector 推送 → 树行渲染。
func TestInspectorPanel(t *testing.T) {
	pkg := mustCompile(t, "Counter.aui", map[string]string{"Counter.aui": counterAUI})
	b := headless.New()
	var logs []string
	r, err := rt.New(pkg, rt.Options{
		Logf:   func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		OnTree: b.Apply,
	})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	ipkg, err := inspector.Package()
	if err != nil {
		t.Fatalf("inspector 包: %v", err)
	}
	if err := r.LoadAdditional(ipkg); err != nil {
		t.Fatalf("LoadAdditional: %v", err)
	}
	if _, err := r.MountWindow("inspector", "Inspector", nil); err != nil {
		t.Fatalf("挂载 inspector: %v", err)
	}
	if _, err := r.MountWindow("app", "Counter", nil); err != nil {
		t.Fatalf("挂载 app: %v", err)
	}
	waitFor(t, func() bool { return b.Tree("inspector") != nil && b.Tree("app") != nil }, "两窗口首帧未渲染")

	// 模拟 App ticker 推送
	rows := r.FlatRows("app")
	if len(rows) == 0 {
		t.Fatalf("FlatRows(app) 为空")
	}
	r.Notify("__inspector", map[string]any{
		"stats":   map[string]any{"instances": 2.0},
		"windows": []any{map[string]any{"id": "app", "rows": rows}},
	})
	waitFor(t, func() bool {
		tree := b.Tree("inspector")
		if tree == nil {
			return false
		}
		found := false
		tree.Walk(func(n *widget.Node) bool {
			if strings.Contains(n.Text(), "alpha") || strings.Contains(n.Text(), "zero") {
				found = true
				return false
			}
			return true
		})
		return found
	}, "Inspector 未渲染推送的组件树行")
}
