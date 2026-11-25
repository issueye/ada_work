package bundle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bundle"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func pluginManifest(id, config string) string {
	if config == "" {
		config = "{}"
	}
	return `{"id":"` + id + `","version":"0.1.0","kind":"go","config":` + config + `}`
}

func find(t *testing.T, manifests []*plugin.Manifest, id string) *plugin.Manifest {
	t.Helper()
	for _, m := range manifests {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("展开结果缺少 %q", id)
	return nil
}

// 1. bundle 展开:config 整体替换、disabled 生效。
func TestBundleExpand(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", `{"keep":true,"old":1}`))
	writeFile(t, filepath.Join(root, "plugins", "b", "plugin.json"), pluginManifest("b", ""))
	writeFile(t, filepath.Join(root, "base.bundle.json"), `{
	  "name": "base",
	  "entries": [
	    {"id": "a", "plugin": "./plugins/a", "config": {"only": "new"}},
	    {"id": "b", "plugin": "./plugins/b", "disabled": true}
	  ]
	}`)

	manifests, warnings, err := (&bundle.Profile{Name: "p", Bundles: []string{"base.bundle.json"}, Dir: root}).Expand()
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("应展开 2 个插件,实际 %d", len(manifests))
	}
	a := find(t, manifests, "a")
	if a.Config["only"] != "new" {
		t.Fatalf("entry config 应整体替换: %v", a.Config)
	}
	if _, ok := a.Config["keep"]; ok {
		t.Fatalf("整体替换不应保留旧键: %v", a.Config)
	}
	if b := find(t, manifests, "b"); !b.Disabled {
		t.Fatalf("disabled 未生效")
	}
	if len(warnings) != 0 {
		t.Fatalf("不应有警告: %v", warnings)
	}
}

// 2. profile 补丁:config 整体替换、disabled 覆盖、insert 提示只告警。
func TestProfilePatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", `{"x":1}`))
	writeFile(t, filepath.Join(root, "plugins", "b", "plugin.json"), pluginManifest("b", ""))
	writeFile(t, filepath.Join(root, "base.bundle.json"), `{
	  "name": "base",
	  "entries": [
	    {"id": "a", "plugin": "./plugins/a"},
	    {"id": "b", "plugin": "./plugins/b", "disabled": true}
	  ]
	}`)
	writeFile(t, filepath.Join(root, "my.profile.json"), `{
	  "name": "mine",
	  "bundles": ["base.bundle.json"],
	  "patch": [
	    {"id": "a", "config": {"y": 2}},
	    {"id": "b", "disabled": false, "insertBefore": ["a"]}
	  ]
	}`)

	p, err := bundle.LoadProfile(filepath.Join(root, "my.profile.json"))
	if err != nil {
		t.Fatalf("加载 profile 失败: %v", err)
	}
	manifests, warnings, err := p.Expand()
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	a := find(t, manifests, "a")
	if a.Config["y"] != float64(2) || a.Config["x"] != nil {
		t.Fatalf("profile config 未整体替换: %v", a.Config)
	}
	if b := find(t, manifests, "b"); b.Disabled {
		t.Fatalf("profile 应覆盖 bundle 的 disabled=false")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "insert_before/after") {
		t.Fatalf("insert 提示应告警: %v", warnings)
	}
}

// 3. patch 指向不存在的 id → 报错;多个 bundle 重复 id → 报错。
func TestProfileErrors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", ""))
	writeFile(t, filepath.Join(root, "base.bundle.json"), `{"name":"base","entries":[{"id":"a","plugin":"./plugins/a"}]}`)

	if _, _, err := (&bundle.Profile{Name: "p", Bundles: []string{"base.bundle.json"},
		Patch: []bundle.PatchEntry{{ID: "nope"}}, Dir: root}).Expand(); err == nil {
		t.Fatalf("未知 patch id 应报错")
	}

	writeFile(t, filepath.Join(root, "b1.json"), `{"name":"b1","entries":[{"id":"a","plugin":"./plugins/a"}]}`)
	writeFile(t, filepath.Join(root, "b2.json"), `{"name":"b2","entries":[{"id":"a","plugin":"./plugins/a"}]}`)
	if _, _, err := (&bundle.Profile{Name: "p", Bundles: []string{"b1.json", "b2.json"}, Dir: root}).Expand(); err == nil {
		t.Fatalf("跨 bundle 重复 id 应报错")
	}
}

// 4. entry id 必须与清单 id 一致;目录必须恰好一个插件。
func TestEntryIDMismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", ""))
	writeFile(t, filepath.Join(root, "x.json"), `{"name":"x","entries":[{"id":"other","plugin":"./plugins/a"}]}`)
	if _, _, err := (&bundle.Profile{Name: "p", Bundles: []string{"x.json"}, Dir: root}).Expand(); err == nil {
		t.Fatalf("entry id 与清单不一致应报错")
	}
}

// 5. 解析校验:未知字段、空条目、缺 id/plugin、重复 id、路径逃逸。
func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"未知字段":      `{"name":"x","entries":[{"id":"a","plugin":"./p","foo":1}]}`,
		"空 entries": `{"name":"x","entries":[]}`,
		"缺 name":    `{"entries":[{"id":"a","plugin":"./p"}]}`,
		"缺 id":      `{"name":"x","entries":[{"plugin":"./p"}]}`,
		"缺 plugin":  `{"name":"x","entries":[{"id":"a"}]}`,
		"重复 id":     `{"name":"x","entries":[{"id":"a","plugin":"./p"},{"id":"a","plugin":"./q"}]}`,
		"路径逃逸":      `{"name":"x","entries":[{"id":"a","plugin":"../p"}]}`,
	}
	for name, raw := range cases {
		if _, err := bundle.Parse([]byte(raw), t.TempDir()); err == nil {
			t.Fatalf("%s: 应被拒绝", name)
		}
	}
}

// 6. profile 解析校验。
func TestProfileValidation(t *testing.T) {
	cases := map[string]string{
		"空 bundles": `{"name":"p","bundles":[]}`,
		"缺 name":    `{"bundles":["b.json"]}`,
		"重复 patch":  `{"name":"p","bundles":["b.json"],"patch":[{"id":"a"},{"id":"a"}]}`,
		"未知字段":      `{"name":"p","bundles":["b.json"],"patches":[]}`,
	}
	for name, raw := range cases {
		if _, err := bundle.ParseProfile([]byte(raw), t.TempDir()); err == nil {
			t.Fatalf("%s: 应被拒绝", name)
		}
	}
}

// 7. 多个 bundle 合并(不同 id)。
func TestMultipleBundles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p1", "plugin.json"), pluginManifest("one", ""))
	writeFile(t, filepath.Join(root, "p2", "plugin.json"), pluginManifest("two", ""))
	writeFile(t, filepath.Join(root, "b1.json"), `{"name":"b1","entries":[{"id":"one","plugin":"./p1"}]}`)
	writeFile(t, filepath.Join(root, "b2.json"), `{"name":"b2","entries":[{"id":"two","plugin":"./p2"}]}`)

	manifests, _, err := (&bundle.Profile{Name: "p", Bundles: []string{"b1.json", "b2.json"}, Dir: root}).Expand()
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(manifests) != 2 || find(t, manifests, "one") == nil || find(t, manifests, "two") == nil {
		t.Fatalf("多 bundle 合并结果不正确")
	}
}

// 8. entry 的 when 表达式:为假时跳过,为真时装配。
func TestEntryWhen(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", ""))
	writeFile(t, filepath.Join(root, "b.json"), `{"name":"b","entries":[
	  {"id":"a","plugin":"./plugins/a","when":"$.features.enabled == true"}
	]}`)

	p := &bundle.Profile{Name: "p", Bundles: []string{"b.json"}, Dir: root}
	manifests, warnings, err := p.ExpandWithEnv(map[string]any{
		"features": map[string]any{"enabled": true},
	})
	if err != nil {
		t.Fatalf("when 为真展开失败: %v", err)
	}
	if len(manifests) != 1 || find(t, manifests, "a") == nil {
		t.Fatalf("when 为真应装配 entry")
	}
	if len(warnings) != 0 {
		t.Fatalf("不应有警告: %v", warnings)
	}

	manifests, warnings, err = p.ExpandWithEnv(map[string]any{
		"features": map[string]any{"enabled": false},
	})
	if err != nil {
		t.Fatalf("when 为假展开失败: %v", err)
	}
	if len(manifests) != 0 {
		t.Fatalf("when 为假应跳过 entry,实际 %d", len(manifests))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "when") {
		t.Fatalf("跳过应告警: %v", warnings)
	}
}

// 9. patch 的 when:为假时整个补丁不生效。
func TestPatchWhen(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"), pluginManifest("a", `{"x":1}`))
	writeFile(t, filepath.Join(root, "base.bundle.json"), `{"name":"base","entries":[{"id":"a","plugin":"./plugins/a"}]}`)
	p := &bundle.Profile{Name: "p", Bundles: []string{"base.bundle.json"},
		Patch: []bundle.PatchEntry{{ID: "a", Config: map[string]any{"y": 2}, When: "$.features.patch == true"}}, Dir: root}

	manifests, _, err := p.ExpandWithEnv(map[string]any{"features": map[string]any{"patch": false}})
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	a := find(t, manifests, "a")
	if a.Config["x"] != float64(1) {
		t.Fatalf("when 为假时 patch 不应生效: %v", a.Config)
	}

	manifests, _, err = p.ExpandWithEnv(map[string]any{"features": map[string]any{"patch": true}})
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	a = find(t, manifests, "a")
	if a.Config["y"] != 2 || a.Config["x"] != nil {
		t.Fatalf("when 为真时 patch 应生效: %v", a.Config)
	}
}

// 10. when 表达式非法在解析期拒绝。
func TestWhenInvalid(t *testing.T) {
	if _, err := bundle.Parse([]byte(`{"name":"b","entries":[{"id":"a","plugin":"./p","when":"$.a >"}]}`), t.TempDir()); err == nil {
		t.Fatalf("非法 entry when 应拒绝")
	}
	if _, err := bundle.ParseProfile([]byte(`{"name":"p","bundles":["b.json"],"patch":[{"id":"a","when":"bad ident"}]}`), t.TempDir()); err == nil {
		t.Fatalf("非法 patch when 应拒绝")
	}
}

// 11. engine(宿主版本)与插件 engines 校验;HostVersion 为空时跳过。
func TestEngineChecks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plugins", "a", "plugin.json"),
		`{"id":"a","version":"0.1.0","kind":"go","engines":{"ada":">=2.0.0"}}`)
	writeFile(t, filepath.Join(root, "b.json"), `{"name":"b","engine":">=2.0.0","entries":[{"id":"a","plugin":"./plugins/a"}]}`)

	p := &bundle.Profile{Name: "p", Bundles: []string{"b.json"}, Dir: root}
	if _, _, err := p.ExpandWithOptions(bundle.ExpandOptions{HostVersion: "2.1.0"}); err != nil {
		t.Fatalf("2.1.0 应满足: %v", err)
	}
	if _, _, err := p.ExpandWithOptions(bundle.ExpandOptions{HostVersion: "1.0.0"}); err == nil {
		t.Fatalf("1.0.0 不满足 bundle engine,应报错")
	}

	// bundle 没有 engine,但插件清单有 engines。
	writeFile(t, filepath.Join(root, "b2.json"), `{"name":"b2","entries":[{"id":"a","plugin":"./plugins/a"}]}`)
	p2 := &bundle.Profile{Name: "p2", Bundles: []string{"b2.json"}, Dir: root}
	if _, _, err := p2.ExpandWithOptions(bundle.ExpandOptions{HostVersion: "1.0.0"}); err == nil {
		t.Fatalf("插件 engines 不满足应报错")
	}
	if _, _, err := p2.Expand(); err != nil {
		t.Fatalf("未提供宿主版本时应跳过校验: %v", err)
	}
}

// 12. bundle 的 engine 语法在解析期校验。
func TestEngineInvalid(t *testing.T) {
	raw := []byte(`{"name":"b","engine":"^2.0.0","entries":[{"id":"a","plugin":"./p"}]}`)
	if _, err := bundle.Parse(raw, t.TempDir()); err == nil {
		t.Fatalf("非法 engine 应拒绝")
	}
}
