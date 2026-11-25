package compile

// 回归测试:构建确定性、组件重名、Verify 摘要/路径安全、多入口报错、诊断行号。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildDeterministic(t *testing.T) {
	files := map[string]string{
		"A.aui": `<template><Column><Text>a</Text></Column></template>`,
		"B.aui": `<template><Column><Text>b</Text></Column></template>`,
		"C.aui": `<template><Column><Text>c</Text></Column></template>`,
	}
	var digest string
	for i := 0; i < 3; i++ {
		pkg, err := CompileFiles("A.aui", files, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg.Components) != 3 {
			t.Fatalf("组件数 = %d", len(pkg.Components))
		}
		// 组件顺序固定按名字排序,保证 manifest/digest 可复现。
		for j := 1; j < len(pkg.Components); j++ {
			if pkg.Components[j-1].Name > pkg.Components[j].Name {
				t.Fatalf("组件未排序: %s > %s", pkg.Components[j-1].Name, pkg.Components[j].Name)
			}
		}
		m, err := pkg.Build(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			digest = m.Digest
		} else if m.Digest != digest {
			t.Fatalf("同输入 digest 不稳定:\n%s\n%s", digest, m.Digest)
		}
	}
}

func TestDuplicateComponentName(t *testing.T) {
	const aui = `<meta>{ "name": "Same" }</meta><template><Column><Text>x</Text></Column></template>`
	_, err := CompileFiles("A.aui", map[string]string{"A.aui": aui, "B.aui": aui}, Options{Entry: "Same"})
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("组件重名应报错: %v", err)
	}
}

func TestVerifyRejectsManifestTamper(t *testing.T) {
	pkg, err := CompileFiles("Counter.aui", files(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := pkg.Build(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ui.manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Digest = "sha256:" + strings.Repeat("0", 64)
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "EUIARTIFACT") {
		t.Fatalf("被篡改的 manifest digest 应报 EUIARTIFACT: %v", err)
	}
}

func TestVerifyRejectsUnsafeArtifactName(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{
		PlanVersion: PlanVersion,
		Entry:       "A",
		Files:       []FileEntry{{Name: "../outside", SHA256: "x", Bytes: 1}},
	}
	digest, err := m.digest()
	if err != nil {
		t.Fatal(err)
	}
	m.Digest = digest
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "ui.manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "非法文件名") {
		t.Fatalf("越界产物路径应拒绝: %v", err)
	}
}

func TestCompileDirRequiresEntryForMultipleAUI(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"A.aui", "B.aui"} {
		src := `<template><Column><Text>` + name + `</Text></Column></template>`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CompileDir(dir, Options{}); err == nil || !strings.Contains(err.Error(), "多个 .aui") {
		t.Fatalf("多组件目录未指定入口应报错: %v", err)
	}
	if _, err := CompileDir(dir, Options{Entry: "A"}); err != nil {
		t.Fatalf("显式入口应通过: %v", err)
	}
}

// TestPackageDigest:产物摘要同输入稳定、内容变化即变(插件完整性校验用)。
func TestPackageDigest(t *testing.T) {
	files := map[string]string{
		"A.aui": `<meta>{ "name": "A" }</meta>
<template><Column><Text>{{ state.n }}</Text></Column></template>
<script lang="ts">
import { reactive } from 'ada:surface'
export default { setup() { const state = reactive({ n: 1 }); return { state } } }
</script>
<style scoped>.x { color: #abc; }</style>
<i18n>{ "zh-CN": { "k": "v" } }</i18n>`,
		"util.ts": "export const K = 1",
	}
	p1, err := CompileFiles("A.aui", files, Options{})
	if err != nil {
		t.Fatalf("编译: %v", err)
	}
	p2, err := CompileFiles("A.aui", files, Options{})
	if err != nil {
		t.Fatalf("编译(第二次): %v", err)
	}
	if p1.Digest() != p2.Digest() {
		t.Fatalf("同输入摘要不稳定: %s != %s", p1.Digest(), p2.Digest())
	}
	if len(p1.Digest()) != len("sha256:")+64 {
		t.Fatalf("摘要格式异常: %q", p1.Digest())
	}
	files["util.ts"] = "export const K = 2"
	p3, err := CompileFiles("A.aui", files, Options{})
	if err != nil {
		t.Fatalf("编译(改依赖后): %v", err)
	}
	if p3.Digest() == p1.Digest() {
		t.Fatal("依赖模块变化后摘要应变化")
	}
}

// TestWarningUsesFileCoordinates:模板告警也要回填 <template> 块偏移。
func TestWarningUsesFileCoordinates(t *testing.T) {
	src := `<template>
  <Column>
    <Row a-for="x in xs"><Text>{{ x }}</Text></Row>
  </Column>
</template>`
	pkg, err := CompileFiles("W.aui", map[string]string{"W.aui": src}, Options{})
	if err != nil {
		t.Fatalf("编译: %v", err)
	}
	if len(pkg.Warnings) == 0 {
		t.Fatal("缺 a-key 应产生告警")
	}
	// a-for 在源文件第 3 行;修复前会报成 template 内的第 2 行。
	if !strings.Contains(pkg.Warnings[0], "W.aui:3:") {
		t.Fatalf("告警应使用源文件行号: %v", pkg.Warnings)
	}
}

// TestScriptErrorUsesFileCoordinates:esbuild 诊断换算到 .aui 源文件坐标。
func TestScriptErrorUsesFileCoordinates(t *testing.T) {
	src := `<template><Column><Text>x</Text></Column></template>

<script lang="ts">
export default {
  setup() {
    const = 1
  },
}
</script>`
	_, err := CompileFiles("E.aui", map[string]string{"E.aui": src}, Options{})
	if err == nil {
		t.Fatal("语法错误应报错")
	}
	if !strings.Contains(err.Error(), "E.aui:6:") || strings.Contains(err.Error(), ".aui.js:") {
		t.Fatalf("脚本诊断应使用源文件坐标(第 6 行): %v", err)
	}
}

// TestStyleErrorUsesFileCoordinates:样式诊断换算到 .aui 源文件坐标。
func TestStyleErrorUsesFileCoordinates(t *testing.T) {
	src := `<template><Column><Text>x</Text></Column></template>

<style scoped>
.x {
  color: #12;
}
</style>`
	_, err := CompileFiles("F.aui", map[string]string{"F.aui": src}, Options{})
	if err == nil {
		t.Fatal("非法颜色应报错")
	}
	if !strings.Contains(err.Error(), "F.aui:5:") || !strings.Contains(err.Error(), "EUISTYLE") {
		t.Fatalf("样式诊断应使用源文件坐标(第 5 行): %v", err)
	}
}

func TestDiagnosticLineUsesFileCoordinates(t *testing.T) {
	src := `<meta>
{ "name": "L" }
</meta>

<template>
  <Column>
    <Column badattr="1"><Text>x</Text></Column>
  </Column>
</template>`
	_, err := CompileFiles("L.aui", map[string]string{"L.aui": src}, Options{})
	if err == nil {
		t.Fatal("非法属性应报错")
	}
	// 真实源文件第 7 行;修复前会报成 template 内的第 2 行。
	if !strings.Contains(err.Error(), "L.aui:") || !strings.Contains(err.Error(), "EUITEMPLATE 7:") {
		t.Fatalf("诊断应使用源文件行号: %v", err)
	}
}
