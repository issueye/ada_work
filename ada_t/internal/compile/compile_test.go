package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const counterAUI = `<template>
  <Column gap="4">
    <Text>{{ display }}</Text>
    <PrimaryButton a-on:click="inc">Add</PrimaryButton>
  </Column>
</template>
<script lang="ts">
import { reactive, computed } from 'ada:surface'
import { fmt } from './util'
export default {
  setup() {
    const state = reactive({ n: 0 })
    const display = computed(() => fmt(state.n))
    function inc() { state.n++ }
    return { state, display, inc }
  },
}
</script>
`

const utilTS = `export function fmt(n: number): string { return '#' + n }`

func files() map[string]string {
	return map[string]string{"Counter.aui": counterAUI, "util.ts": utilTS}
}

func TestCompileFiles(t *testing.T) {
	pkg, err := CompileFiles("Counter.aui", files(), Options{})
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if pkg.Entry != "Counter" {
		t.Fatalf("Entry = %q", pkg.Entry)
	}
	if len(pkg.Components) != 1 {
		t.Fatalf("组件数 = %d", len(pkg.Components))
	}
	c := pkg.Components[0]
	if c.Name != "Counter" || !c.HasScript {
		t.Fatalf("组件: %v hasScript=%v", c.Name, c.HasScript)
	}
	// 模块表:组件脚本 + 相对导入
	if _, ok := pkg.Modules["Counter.aui.js"]; !ok {
		t.Fatal("缺组件脚本模块 Counter.aui.js")
	}
	if _, ok := pkg.Modules["util.ts"]; !ok {
		t.Fatal("缺相对导入模块 util.ts")
	}
	// 解析:相对导入 ./util → util.ts
	resolved, ok := pkg.Resolve("Counter.aui.js", "./util")
	if !ok || resolved != "util.ts" {
		t.Fatalf("Resolve ./util = %q ok=%v", resolved, ok)
	}
	// 拒绝裸包名
	if _, ok := pkg.Resolve("Counter.aui.js", "lodash"); ok {
		t.Fatal("裸包名应拒绝")
	}
	// 拒绝越界
	if _, ok := pkg.Resolve("Counter.aui.js", "../../etc/passwd"); ok {
		t.Fatal("越界路径应拒绝")
	}
}

func TestMultiAUIEntry(t *testing.T) {
	files := map[string]string{
		"App.aui": `<template><Column><Sub /></Column></template><script lang="ts">import Sub from './Sub'; export default { components: { Sub }, setup() { return {} } }</script>`,
		"Sub.aui": `<template><Row><Text>sub</Text></Row></template>`,
	}
	pkg, err := CompileFiles("App.aui", files, Options{})
	if err != nil {
		t.Fatalf("多组件编译失败: %v", err)
	}
	if pkg.Entry != "App" || len(pkg.Components) != 2 {
		t.Fatalf("Entry=%q 组件数=%d", pkg.Entry, len(pkg.Components))
	}
}

func TestExplicitEntry(t *testing.T) {
	files := map[string]string{
		"A.aui": `<template><Column /></template>`,
		"B.aui": `<template><Row /></template>`,
	}
	if _, err := CompileFiles("A.aui", files, Options{}); err != nil {
		t.Fatalf("entryFile 决定默认入口: %v", err)
	}
	pkg, err := CompileFiles("A.aui", files, Options{Entry: "B"})
	if err != nil {
		t.Fatalf("显式入口失败: %v", err)
	}
	if pkg.Entry != "B" {
		t.Fatalf("Entry = %q", pkg.Entry)
	}
	_, err = CompileFiles("A.aui", files, Options{Entry: "Z"})
	if err == nil {
		t.Fatal("未知入口组件应失败")
	}
}

func TestBuildAndVerify(t *testing.T) {
	pkg, err := CompileFiles("Counter.aui", files(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m, err := pkg.Build(out)
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}
	if m.PlanVersion != PlanVersion || m.Entry != "Counter" {
		t.Fatalf("manifest: version=%d entry=%q", m.PlanVersion, m.Entry)
	}
	if len(m.Files) < 2 {
		t.Fatalf("文件数 = %d", len(m.Files))
	}
	// Verify:产物与摘要一致
	got, err := Verify(out)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if got.Digest != m.Digest {
		t.Fatal("digest 不一致")
	}
	// 篡改一个产物 → Verify 必须失败(EUIARTIFACT)
	planPath := filepath.Join(out, "Counter.plan.json")
	b, _ := os.ReadFile(planPath)
	_ = os.WriteFile(planPath, []byte(strings.Replace(string(b), "Column", "Roww", 1)), 0o644)
	if _, err := Verify(out); err == nil || !strings.Contains(err.Error(), "EUIARTIFACT") {
		t.Fatalf("篡改产物应报 EUIARTIFACT: %v", err)
	}
}

func TestCompileDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "Counter.aui"), []byte(counterAUI), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "util.ts"), []byte(utilTS), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o644)
	pkg, err := CompileDir(dir, Options{})
	if err != nil {
		t.Fatalf("CompileDir 失败: %v", err)
	}
	if pkg.Entry != "Counter" {
		t.Fatalf("Entry = %q", pkg.Entry)
	}
	if len(pkg.Modules) != 2 {
		t.Fatalf("模块数 = %d(组件脚本+util)", len(pkg.Modules))
	}
}

func TestCompileDirNoAUI(t *testing.T) {
	dir := t.TempDir()
	if _, err := CompileDir(dir, Options{}); err == nil {
		t.Fatal("无 .aui 目录应失败")
	}
}
