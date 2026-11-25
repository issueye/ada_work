package rt_test

// 插件最小闭环:装载/卸载、模块命名空间、能力白名单、事件前缀、槽位视图。

import (
	"sync"

	"fmt"
	"strings"
	"testing"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/internal/rt"
	"github.com/issueye/ada_t/plugin"
)

const pluginHostAUI = `
<meta>
{ "name": "Host" }
</meta>
<template>
  <Column gap="4"><Text>host-root</Text></Column>
</template>
<script lang="ts">
export default { setup() { return {} } }
</script>
`

func pluginAUI(prefix string) (entry string, files map[string]string) {
	entry = prefix + "Card"
	files = map[string]string{
		prefix + "Card.aui": fmt.Sprintf(`
<meta>
{ "name": "%sCard" }
</meta>
<template>
  <Column gap="4">
    <Text>{{ state.msg }}</Text>
    <PrimaryButton a-on:click="ask">Ask</PrimaryButton>
    <Button a-on:click="secret">Secret</Button>
  </Column>
</template>
<script lang="ts">
import { tag } from './util'
import { reactive } from 'ada:surface'
export default {
  name: '%sCard',
  setup(props, ctx) {
    const state = reactive({ msg: tag() + '|' + ctx.theme('bg') })
    ctx.on('ping', (p) => { state.msg = 'ping:' + p })
    function ask() {
      state.msg = String(ctx.api.call('host', 'greet'))
      ctx.emitHost('asked', { by: ctx.pluginId })
    }
    function secret() {
      ctx.api.call('host', 'secret')
    }
    return { state, ask, secret }
  },
}
</script>
`, prefix, prefix),
		prefix + "Side.aui": fmt.Sprintf(`
<meta>
{ "name": "%sSide" }
</meta>
<template>
  <Row><Text>%s-side</Text></Row>
</template>
<script lang="ts">
export default { name: '%sSide', setup() { return {} } }
</script>
`, prefix, prefix, prefix),
		"util.ts": fmt.Sprintf("export function tag(): string { return '%s-util' }", prefix),
	}
	return entry, files
}

func mustPlugin(t *testing.T, id, prefix string, caps []string, slots []plugin.Contribution) (*plugin.Manifest, *compile.Package) {
	t.Helper()
	entry, files := pluginAUI(prefix)
	pkg, err := compile.CompileFiles(entry+".aui", files, compile.Options{Entry: entry})
	if err != nil {
		t.Fatalf("编译插件 %s 失败: %v", id, err)
	}
	m := &plugin.Manifest{
		ID: id, Name: id, Version: "0.1.0", Entry: entry,
		Capabilities: caps, Slots: slots,
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("清单非法: %v", err)
	}
	return m, pkg
}

func TestPluginLifecycle(t *testing.T) {
	hostPkg := mustCompile(t, "Host.aui", map[string]string{"Host.aui": pluginHostAUI})
	b := headless.New()
	var mu sync.Mutex
	var logs []string
	var hostEvents []string
	r, err := rt.New(hostPkg, rt.Options{
		Logf: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(f, a...))
			mu.Unlock()
		},
		OnTree: b.Apply,
		Theme:  map[string]any{"bg": "#123456"},
	})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	b.SetDispatch(func(_ string, hid string, payload any) { r.Fire(hid, payload) })
	r.OnHostEvent(func(name string, payload any) {
		mu.Lock()
		hostEvents = append(hostEvents, name)
		mu.Unlock()
	})
	if err := r.Expose("host", "greet", func() string { return "hi-alpha" }); err != nil {
		t.Fatalf("Expose greet: %v", err)
	}
	if err := r.Expose("host", "secret", func() string { return "should-not-call" }); err != nil {
		t.Fatalf("Expose secret: %v", err)
	}

	alphaM, alphaPkg := mustPlugin(t, "alpha", "Alpha", []string{"host.greet"},
		[]plugin.Contribution{{Slot: plugin.SlotSidebarItem, Component: "AlphaSide"}})
	if err := r.LoadPlugin(alphaM, alphaPkg); err != nil {
		t.Fatalf("装载 alpha: %v", err)
	}
	if _, err := r.MountWindowOwned("v-alpha", "AlphaCard", nil, "alpha"); err != nil {
		t.Fatalf("挂载 alpha 视图: %v", err)
	}
	waitFor(t, func() bool {
		tree := b.Tree("v-alpha")
		return tree != nil && strings.Contains(tree.Dump(), "Alpha-util|#123456")
	}, "插件首帧未渲染(主题或模块导入失败)")

	// 能力白名单内:调用成功
	ask := findTagID(b.Tree("v-alpha"), "PrimaryButton", "Ask")
	if !b.Fire("v-alpha", ask, "click", nil) {
		t.Fatal("Ask 点击派发失败")
	}
	waitFor(t, func() bool {
		tree := b.Tree("v-alpha")
		return tree != nil && strings.Contains(tree.Dump(), "hi-alpha")
	}, "能力内服务调用未生效")

	// 插件 → 宿主事件(带 plugin.<id>. 前缀)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range hostEvents {
			if e == "plugin.alpha.asked" {
				return true
			}
		}
		return false
	}, "ctx.emitHost 未到达宿主")

	// 宿主 → 插件事件(命名空间)
	r.Notify("plugin.alpha.ping", "x")
	waitFor(t, func() bool { return strings.Contains(b.Tree("v-alpha").Dump(), "ping:x") }, "EmitTo 前缀事件未到达插件")
	// 没有前缀的事件不应串到插件
	r.Notify("ping", "y")
	waitFor(t, func() bool { return !strings.Contains(b.Tree("v-alpha").Dump(), "ping:y") }, "无前缀事件串到了插件")

	// 能力白名单外:调用被拒绝(事件异常进日志)
	secret := findTagID(b.Tree("v-alpha"), "Button", "Secret")
	if !b.Fire("v-alpha", secret, "click", nil) {
		t.Fatal("Secret 点击派发失败")
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range logs {
			if strings.Contains(l, "无能力调用 host.secret") {
				return true
			}
		}
		return false
	}, "越权调用未被拒绝")

	// 第二个插件:同名 util.ts 不同内容,靠模块前缀互不覆盖
	betaM, betaPkg := mustPlugin(t, "beta", "Beta", []string{"host.greet"},
		[]plugin.Contribution{{Slot: plugin.SlotSidebarItem, Component: "BetaSide"}})
	if err := r.LoadPlugin(betaM, betaPkg); err != nil {
		t.Fatalf("装载 beta: %v", err)
	}
	if _, err := r.MountWindowOwned("v-beta", "BetaCard", nil, "beta"); err != nil {
		t.Fatalf("挂载 beta 视图: %v", err)
	}
	waitFor(t, func() bool {
		tree := b.Tree("v-beta")
		return tree != nil && strings.Contains(tree.Dump(), "Beta-util")
	}, "beta 未渲染")
	if strings.Contains(b.Tree("v-alpha").Dump(), "Beta-util") {
		t.Fatal("alpha 的模块被 beta 覆盖(前缀失效)")
	}

	// 重名组件:拒绝装载(组件名全局唯一)
	gammaM, gammaPkg := mustPlugin(t, "gamma", "Beta", nil, nil)
	if err := r.LoadPlugin(gammaM, gammaPkg); err == nil {
		t.Fatal("组件重名应拒绝装载")
	}

	// 插件登记与槽位贡献
	if infos := r.Plugins(); len(infos) != 2 {
		t.Fatalf("已装载插件 = %v, want 2", infos)
	}
	info, ok := r.PluginInfo("alpha")
	if !ok || info.Entry != "AlphaCard" || len(info.Slots) != 1 {
		t.Fatalf("PluginInfo(alpha) = %+v ok=%v", info, ok)
	}
	if views := r.PluginViews("alpha"); len(views) != 1 || views[0] != "v-alpha" {
		t.Fatalf("PluginViews(alpha) = %v", views)
	}

	// 卸载 alpha:视图回收、组件注销、beta 不受影响
	if err := r.UnloadPlugin("alpha"); err != nil {
		t.Fatalf("卸载 alpha: %v", err)
	}
	if views := r.PluginViews("alpha"); len(views) != 0 {
		t.Fatalf("卸载后仍有视图: %v", views)
	}
	if _, err := r.MountWindowOwned("v-alpha2", "AlphaCard", nil, "alpha"); err == nil {
		t.Fatal("卸载后仍能挂载插件组件")
	}
	if _, ok := r.PluginInfo("alpha"); ok {
		t.Fatal("卸载后插件仍在登记表")
	}
	waitFor(t, func() bool { return strings.Contains(b.Tree("v-beta").Dump(), "Beta-util") }, "beta 被 alpha 卸载波及")

	// 统计:实例/handler 回落到宿主规模
	st, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st["plugins"] < 1 {
		t.Fatalf("stats 未记录插件数: %v", st)
	}
	if st["instances"] > 2 {
		t.Fatalf("卸载后实例未回落: %v", st["instances"])
	}
}

// TestPluginCapabilitiesListing:宿主可用 HostCapabilities 生成插件能力文档。
func TestPluginCapabilitiesListing(t *testing.T) {
	hostPkg := mustCompile(t, "Host.aui", map[string]string{"Host.aui": pluginHostAUI})
	r, err := rt.New(hostPkg, rt.Options{Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("运行时: %v", err)
	}
	defer r.Close()
	if err := r.Expose("host", "bots.list", func() []string { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := r.Expose("host", "chat.append", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	caps := r.Capabilities()
	if len(caps) != 2 || caps[0] != "host.bots.list" || caps[1] != "host.chat.append" {
		t.Fatalf("Capabilities = %v", caps)
	}
}
