package preset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/preset"
)

func writePlugin(t *testing.T, root, id, extra string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	body := `{"id":"` + id + `","version":"0.1.0","kind":"go"` + extra + `}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
}

func writePreset(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写 preset 失败: %v", err)
	}
}

// 1. preset 展开:多目录、disabled、config 深合并。
func TestLoadExpandAppliesDisableAndPatch(t *testing.T) {
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	writePlugin(t, plugins, "alpha", `,"config":{"a":{"x":1,"y":2},"keep":true}`)
	writePlugin(t, plugins, "beta", "")

	presetPath := filepath.Join(root, "base.preset.json")
	writePreset(t, presetPath, `{
	  "name": "base",
	  "plugins": ["./plugins"],
	  "disable": ["beta"],
	  "patch": {"alpha": {"a": {"y": 3}, "extra": "p"}}
	}`)

	p, err := preset.Load(presetPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	ms, err := p.Expand()
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("应展开 2 个插件,实际 %d", len(ms))
	}

	byID := map[string]*plugin.Manifest{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	alpha := byID["alpha"]
	if alpha == nil {
		t.Fatalf("展开结果缺少 alpha: %v", byID)
	}
	a, _ := alpha.Config["a"].(map[string]any)
	if a == nil || a["x"] != float64(1) || a["y"] != float64(3) {
		t.Fatalf("config 深合并不正确: %v", alpha.Config)
	}
	if alpha.Config["keep"] != true || alpha.Config["extra"] != "p" {
		t.Fatalf("config 其它键不应丢失: %v", alpha.Config)
	}
	beta := byID["beta"]
	if beta == nil || !beta.Disabled {
		t.Fatalf("beta 应存在且被 disabled: %+v", beta)
	}
}

// 2. 相对路径按 preset 文件所在目录解析(而不是当前工作目录)。
func TestRelativeToPresetDir(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, filepath.Join(root, "plugins"), "alpha", "")
	presetPath := filepath.Join(root, "presets", "p.json")
	writePreset(t, presetPath, `{"name":"rel","plugins":["../plugins"]}`)

	p, err := preset.Load(presetPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	ms, err := p.Expand()
	if err != nil || len(ms) != 1 || ms[0].ID != "alpha" {
		t.Fatalf("相对路径解析失败: %v %+v", err, ms)
	}
}

// 3. 未知的 disable/patch 目标必须报错,不能静默忽略。
func TestUnknownTargetsFail(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, filepath.Join(root, "plugins"), "alpha", "")

	base := `{"name":"p","plugins":["./plugins"],`
	cases := map[string]string{
		"disable": base + `"disable":["nope"]}`,
		"patch":   base + `"patch":{"nope":{"x":1}}}`,
	}
	for name, body := range cases {
		presetPath := filepath.Join(root, name+".preset.json")
		writePreset(t, presetPath, body)
		p, err := preset.Load(presetPath)
		if err != nil {
			t.Fatalf("%s: 加载失败: %v", name, err)
		}
		if _, err := p.Expand(); err == nil || !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("%s: 未知目标应报错,实际 %v", name, err)
		}
	}
}

// 4. preset 自身的校验。
func TestPresetValidation(t *testing.T) {
	cases := map[string]string{
		"坏 name":     `{"name":"Base","plugins":["."]}`,
		"空 plugins":  `{"name":"x","plugins":[]}`,
		"disable 重复": `{"name":"x","plugins":["."],"disable":["a","a"]}`,
		"未知字段":       `{"name":"x","plugins":["."],"disabled":["a"]}`,
	}
	for name, body := range cases {
		if _, err := preset.Parse([]byte(body), ""); err == nil {
			t.Fatalf("%s: 应报错,实际通过: %s", name, body)
		}
	}
}

// 5. 同一 id 出现在多个目录 → 展开失败(装配层不允许重复身份)。
func TestDuplicateIDAcrossDirs(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, filepath.Join(root, "one"), "alpha", "")
	writePlugin(t, filepath.Join(root, "two"), "alpha", "")
	presetPath := filepath.Join(root, "dup.preset.json")
	writePreset(t, presetPath, `{"name":"dup","plugins":["./one","./two"]}`)

	p, err := preset.Load(presetPath)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, err := p.Expand(); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 id 应报错,实际 %v", err)
	}
}
