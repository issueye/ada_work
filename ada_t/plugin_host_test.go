package adat_test

// 插件宿主闭环(公共 API):编译插件目录 → RegisterSlot → LoadPlugin →
// SlotViews 交给宿主绘制(backend/embed)→ 点击回传 → 卸载回收。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/embed"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/plugin"
)

const hostAUI = `
<meta>
{ "name": "Host" }
</meta>
<template>
  <Column gap="4"><Text>host-root</Text></Column>
</template>
`

const helloCardAUI = `
<meta>
{ "name": "HelloCard" }
</meta>
<template>
  <Column gap="4">
    <Text>{{ state.msg }}</Text>
    <PrimaryButton a-on:click="ask">Ask</PrimaryButton>
  </Column>
</template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default {
  name: 'HelloCard',
  setup(props, ctx) {
    const state = reactive({ msg: 'theme=' + (ctx.theme('bg') || 'none') })
    function ask() {
      state.msg = String(ctx.api.call('host', 'greet'))
      ctx.emitHost('asked', { id: ctx.pluginId })
    }
    return { state, ask }
  },
}
</script>
`

const helloSideAUI = `
<meta>
{ "name": "HelloSide" }
</meta>
<template>
  <Row><Text>hello-side</Text></Row>
</template>
`

const helloManifest = `{
  "id": "hello",
  "name": "Hello 插件",
  "version": "0.1.0",
  "entry": "HelloCard",
  "capabilities": ["host.greet"],
  "slots": [
    { "slot": "sidebar.item", "component": "HelloSide", "order": 5 },
    { "slot": "composer.action", "component": "HelloCard", "order": 1 }
  ]
}`

func writePlugin(t *testing.T, dir, manifest string) {
	t.Helper()
	files := map[string]string{
		"plugin.json":   manifest,
		"HelloCard.aui": helloCardAUI,
		"HelloSide.aui": helloSideAUI,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustHostPkg(t *testing.T) *compile.Package {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Host.aui"), []byte(hostAUI), 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := adat.CompileDir(dir, "")
	if err != nil {
		t.Fatalf("编译宿主: %v", err)
	}
	return pkg
}

func newHostApp(t *testing.T, backend *embed.Backend, opts adat.AppOptions) *adat.App {
	t.Helper()
	opts.Backend = backend
	opts.Theme = map[string]any{"bg": "#101010"}
	opts.Logf = func(string, ...any) {}
	app, err := adat.NewApp(mustHostPkg(t), opts)
	if err != nil {
		t.Fatalf("创建应用: %v", err)
	}
	if err := app.ExposeHost("host", "greet", func() string { return "hi-from-host" }); err != nil {
		t.Fatalf("ExposeHost: %v", err)
	}
	return app
}

func TestSlotHostLifecycle(t *testing.T) {
	pdir := t.TempDir()
	writePlugin(t, pdir, helloManifest)
	pkg, man, err := adat.CompilePlugin(pdir)
	if err != nil {
		t.Fatalf("CompilePlugin: %v", err)
	}
	if man.ID != "hello" || pkg.Entry != "HelloCard" {
		t.Fatalf("编译结果 man=%+v pkg.Entry=%s", man, pkg.Entry)
	}

	b := embed.New(embed.Options{Theme: map[string]any{"bg": "#101010"}})
	app := newHostApp(t, b, adat.AppOptions{SlotMode: true})
	defer app.Stop()
	for _, s := range []string{plugin.SlotSidebarItem, plugin.SlotComposerAction} {
		if err := app.RegisterSlot(s); err != nil {
			t.Fatalf("RegisterSlot(%s): %v", s, err)
		}
	}
	var hostEvents []string
	var mu sync.Mutex
	app.OnPluginEvent(func(pluginID, event string, payload any) {
		mu.Lock()
		hostEvents = append(hostEvents, pluginID+":"+event)
		mu.Unlock()
	})

	info, err := app.LoadPlugin(pkg, man)
	if err != nil {
		t.Fatalf("LoadPlugin: %v", err)
	}
	if info.ID != "hello" || len(info.Slots) != 2 {
		t.Fatalf("PluginInfo = %+v", info)
	}
	views := app.SlotViews("")
	if len(views) != 2 {
		t.Fatalf("SlotViews = %+v", views)
	}
	// 按槽位、order 排序;side 在 sidebar,card 在 composer
	side := app.SlotViews(plugin.SlotSidebarItem)
	if len(side) != 1 || side[0].Component != "HelloSide" || side[0].Order != 5 {
		t.Fatalf("sidebar 视图 = %+v", side)
	}
	card := app.SlotViews(plugin.SlotComposerAction)[0]

	waitFor(t, func() bool { return b.Tree(side[0].ID) != nil && b.Tree(card.ID) != nil }, "槽位视图首帧未到达 embed")

	// 宿主把自己的 view 画进 ui.Context(主题令牌 → 插件文案)
	sideTT := ui.NewTester(func(c *ui.Context) { b.Draw(c, side[0].ID) }, 400, 240)
	if !sideTT.HasText("hello-side") {
		t.Fatalf("侧栏槽位内容未绘制: %v", sideTT.Texts())
	}
	cardTT := ui.NewTester(func(c *ui.Context) { b.Draw(c, card.ID) }, 400, 240)
	if !cardTT.HasText("theme=#101010") {
		t.Fatalf("插件未读到主题令牌: %v", cardTT.Texts())
	}

	// 点击槽位里的按钮 → embed dispatch → rt.Fire → 服务调用
	if err := cardTT.Click("Ask"); err != nil {
		t.Fatalf("点击 Ask: %v", err)
	}
	waitFor(t, func() bool {
		tree := b.Tree(card.ID)
		return tree != nil && strings.Contains(tree.Dump(), "hi-from-host")
	}, "槽位点击未回传(能力内服务调用失败)")

	// 插件 → 宿主事件
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range hostEvents {
			if e == "hello:asked" {
				return true
			}
		}
		return false
	}, "插件 emitHost 未到宿主")

	// 重复装载拒绝
	if _, err := app.LoadPlugin(pkg, man); err == nil {
		t.Fatal("重复装载应失败")
	}

	// 卸载:视图回收、embed 树清空
	if err := app.UnloadPlugin("hello"); err != nil {
		t.Fatalf("UnloadPlugin: %v", err)
	}
	if views := app.SlotViews(""); len(views) != 0 {
		t.Fatalf("卸载后仍残留视图: %+v", views)
	}
	waitFor(t, func() bool { return b.Tree(card.ID) == nil }, "卸载后 embed 仍保留树")
}

// TestAppRunLifecycleWithEmbed:App.Run 在 embed 后端上阻塞到 Stop,
// 然后关闭运行时(示例与真实宿主都依赖这个时序)。
func TestAppRunLifecycleWithEmbed(t *testing.T) {
	app := newHostApp(t, embed.New(embed.Options{}), adat.AppOptions{SlotMode: true})
	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	time.Sleep(30 * time.Millisecond)
	if err := app.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 后 App.Run 未返回")
	}
}

// TestExamplePluginCompiles 守住示例插件与清单不漂移。
func TestExamplePluginCompiles(t *testing.T) {
	pkg, man, err := adat.CompilePlugin("examples/plugins/hello")
	if err != nil {
		t.Fatalf("示例插件编译失败: %v", err)
	}
	if man.ID != "hello" || man.Entry != "HelloCard" {
		t.Fatalf("示例清单 = %+v", man)
	}
	if len(man.Slots) != 2 {
		t.Fatalf("示例槽位 = %+v", man.Slots)
	}
	if digest := adat.PluginDigest(pkg); digest == "" || len(digest) != len("sha256:")+64 {
		t.Fatalf("摘要格式异常: %q", digest)
	}
}

func TestSlotMustBeRegistered(t *testing.T) {
	pdir := t.TempDir()
	writePlugin(t, pdir, `{
  "id": "hello",
  "version": "0.1.0",
  "entry": "HelloCard",
  "slots": [{ "slot": "unknown.slot", "component": "HelloSide" }]
}`)
	pkg, man, err := adat.CompilePlugin(pdir)
	if err != nil {
		t.Fatalf("CompilePlugin: %v", err)
	}
	app := newHostApp(t, embed.New(embed.Options{}), adat.AppOptions{SlotMode: true})
	defer app.Stop()
	if _, err := app.LoadPlugin(pkg, man); err == nil {
		t.Fatal("未注册的插槽应拒绝装载")
	}
	if err := app.RegisterSlot("unknown.slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.LoadPlugin(pkg, man); err != nil {
		t.Fatalf("注册后应可装载: %v", err)
	}
}

func TestPluginTrust(t *testing.T) {
	pdir := t.TempDir()
	writePlugin(t, pdir, `{
  "id": "hello",
  "version": "0.1.0",
  "entry": "HelloCard",
  "sha256": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
}`)
	pkg, man, err := adat.CompilePlugin(pdir)
	if err != nil {
		t.Fatalf("CompilePlugin: %v", err)
	}
	app := newHostApp(t, embed.New(embed.Options{}), adat.AppOptions{SlotMode: true})
	defer app.Stop()
	if _, err := app.LoadPlugin(pkg, man); err == nil {
		t.Fatal("摘要不符应拒绝装载")
	}
	man.SHA256 = adat.PluginDigest(pkg)
	if _, err := app.LoadPlugin(pkg, man); err != nil {
		t.Fatalf("摘要正确后应可装载: %v", err)
	}
	// 要求摘要但清单没有
	app2 := newHostApp(t, embed.New(embed.Options{}), adat.AppOptions{
		SlotMode:    true,
		PluginTrust: adat.PluginTrust{RequireDigest: true},
	})
	defer app2.Stop()
	man.SHA256 = ""
	if _, err := app2.LoadPlugin(pkg, man); err == nil {
		t.Fatal("RequireDigest 下缺摘要应拒绝")
	}
}

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
