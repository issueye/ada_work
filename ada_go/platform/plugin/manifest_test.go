package plugin_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/plugin"
)

func TestParseValidManifest(t *testing.T) {
	src := `{
	  "id": "approval-gate",
	  "name": "审批闸门",
	  "version": "0.2.1",
	  "kind": "ts",
	  "entry": "src/index.ts",
	  "inject": {"required": ["session"], "optional": ["telemetry"]},
	  "provide": ["approval"],
	  "config": {"timeout": 3000}
	}`
	m, err := plugin.Parse([]byte(src), "/tmp/approval-gate")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.ID != "approval-gate" || m.Kind != "ts" || m.Dir != "/tmp/approval-gate" {
		t.Fatalf("字段不正确: %+v", m)
	}
	if len(m.Inject.Required) != 1 || m.Inject.Required[0] != "session" {
		t.Fatalf("inject.required 不正确: %+v", m.Inject)
	}
}

// 1. 未知字段必须报错:清单是契约,拼错的字段不能静默忽略。
func TestRejectsUnknownField(t *testing.T) {
	if _, err := plugin.Parse([]byte(`{"id":"a","version":"0.1.0","kind":"go","provide":["x"],"injects":{"required":["y"]}}`), ""); err == nil {
		t.Fatalf("拼错的 injects 应被拒绝")
	}
}

// 2. 清单自身的合法性。
func TestValidateRules(t *testing.T) {
	cases := map[string]string{
		"坏 id":                   `{"id":"Bad_ID","version":"0.1.0","kind":"go"}`,
		"坏版本":                    `{"id":"a","version":"1.0","kind":"go"}`,
		"坏 kind":                 `{"id":"a","version":"0.1.0","kind":"wasm"}`,
		"ts 缺入口":                 `{"id":"a","version":"0.1.0","kind":"ts"}`,
		"provide 重复":             `{"id":"a","version":"0.1.0","kind":"go","provide":["x","x"]}`,
		"自依赖":                    `{"id":"a","version":"0.1.0","kind":"go","provide":["x"],"inject":{"required":["x"]}}`,
		"required 与 optional 重叠": `{"id":"a","version":"0.1.0","kind":"go","inject":{"required":["x"],"optional":["x"]}}`,
	}
	for name, src := range cases {
		if _, err := plugin.Parse([]byte(src), ""); err == nil {
			t.Fatalf("%s: 应报错,实际通过: %s", name, src)
		}
	}
}

// 3. 目录扫描:每个子目录一份清单,按 id 排序;单插件目录也支持。
func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	write := func(dir, id string) {
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		src := `{"id":"` + id + `","version":"0.1.0","kind":"go"}`
		if err := os.WriteFile(filepath.Join(full, "plugin.json"), []byte(src), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
	}
	write("zeta", "zeta")
	write("alpha", "alpha")
	if err := os.MkdirAll(filepath.Join(root, "not-a-plugin"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}

	ms, err := plugin.LoadDir(root)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != "alpha" || ms[1].ID != "zeta" {
		t.Fatalf("应按 id 排序返回两个插件,实际 %+v", ms)
	}

	// 单插件目录
	single, err := plugin.LoadDir(filepath.Join(root, "alpha"))
	if err != nil || len(single) != 1 || single[0].ID != "alpha" {
		t.Fatalf("单插件目录加载失败: %v %+v", err, single)
	}

	// 空目录
	if _, err := plugin.LoadDir(filepath.Join(root, "not-a-plugin")); err == nil {
		t.Fatalf("没有 plugin.json 的目录应报错")
	}
}

// 能力与信任等级:非法能力名/信任等级在清单校验期拒绝。
func TestCapsAndTrust(t *testing.T) {
	valid := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go",
		Caps: []string{"fs.read", "exec"}, Trust: "official"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法能力/信任等级应通过: %v", err)
	}

	badCaps := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go", Caps: []string{"fs.exec"}}
	if err := badCaps.Validate(); err == nil {
		t.Fatalf("非法能力名应被拒绝")
	}

	badTrust := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go", Trust: "root"}
	if err := badTrust.Validate(); err == nil {
		t.Fatalf("非法信任等级应被拒绝")
	}
}

// engines:只认 ada,且约束语法必须合法。
func TestEngines(t *testing.T) {
	valid := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go",
		Engines: map[string]string{"ada": ">=2.0.0"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 engines 应通过: %v", err)
	}
	badConstraint := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go",
		Engines: map[string]string{"ada": "^2.0.0"}}
	if err := badConstraint.Validate(); err == nil {
		t.Fatalf("非法约束应被拒绝")
	}
	unknownKey := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go",
		Engines: map[string]string{"node": ">=1.0.0"}}
	if err := unknownKey.Validate(); err == nil {
		t.Fatalf("未知 engine key 应被拒绝")
	}
}

// 磁盘上的 TS 插件自述 builtin 必须降为 third-party。
func TestLoadDirDowngradesDiskTrust(t *testing.T) {
	dir := t.TempDir()
	body := `{"id":"evil","version":"0.1.0","kind":"ts","entry":"plugin.ts","trust":"builtin","caps":["exec"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ms, err := plugin.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Trust != "third-party" {
		t.Fatalf("磁盘 TS 插件 trust 应降为 third-party,实际 %+v", ms)
	}
}

// activation 只允许 startup / on-demand。
func TestActivation(t *testing.T) {
	valid := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go", Activation: "on-demand"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法 activation 应通过: %v", err)
	}
	for _, bad := range []string{"lazy", "OnDemand", "cold"} {
		m := &plugin.Manifest{ID: "p", Version: "0.1.0", Kind: "go", Activation: bad}
		if err := m.Validate(); err == nil {
			t.Fatalf("非法 activation %q 应被拒绝", bad)
		}
	}
}
