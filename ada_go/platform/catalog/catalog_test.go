package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/plugin"
)

// TestCatalogManifestsMatchRegistry 守住「仓库里的插件清单 = 编译进宿主的注册表」。
//
// 仓库的 plugins/<分类>/<目录>/plugin.json 是内置插件的唯一清单来源(发布打包也从注册表
// 重新导出);手改清单、加插件忘记重生成、或把 config 写回清单,都必须在这里失败。
func TestCatalogManifestsMatchRegistry(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatalf("解析插件根目录失败: %v", err)
	}
	ids, err := BuiltinIDs()
	if err != nil {
		t.Fatalf("读取内置注册表失败: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("内置注册表为空")
	}
	dirs := map[string]string{}
	for _, id := range ids {
		dir, ok := PluginSourceDir(root, id)
		if !ok {
			t.Fatalf("内置插件 %q 没有源码目录(在 plugins/<分类>/<目录> 下找不到);新增插件必须同时放进 plugins/", id)
		}
		if other, dup := dirs[dir]; dup {
			t.Fatalf("插件 %q 与 %q 指向同一个源码目录 %s", id, other, dir)
		}
		dirs[dir] = id

		raw, rerr := os.ReadFile(filepath.Join(dir, "plugin.json"))
		if rerr != nil {
			t.Fatalf("插件 %q 缺少 plugin.json(%s);用 go run ./scripts/tools/builtins-dump -source plugins 生成", id, dir)
		}
		m, perr := plugin.Parse(raw, dir)
		if perr != nil {
			t.Fatalf("插件 %q 的 plugin.json 无效: %v", id, perr)
		}
		if m.Config != nil {
			t.Fatalf("插件 %q 的清单写了 config;参数属于 preset,清单只陈述 id/依赖/提供", id)
		}
		want, merr := BuiltinManifest(id)
		if merr != nil {
			t.Fatalf("生成插件 %q 的清单失败: %v", id, merr)
		}
		wantRaw, jerr := json.MarshalIndent(want, "", "  ")
		if jerr != nil {
			t.Fatalf("序列化插件 %q 的清单失败: %v", id, jerr)
		}
		wantRaw = append(wantRaw, '\n')
		if string(raw) != string(wantRaw) {
			t.Fatalf("插件 %q 的清单与注册表不一致(%s)\n仓库:\n%s\n注册表:\n%s\n重生成: go run ./scripts/tools/builtins-dump -source plugins",
				id, dir, raw, wantRaw)
		}
	}
}

// TestSeamDirsHoldContractsOnly 守住「plugins/seam 只放契约」。
//
// seam 目录下的包是接口/类型/错误码;插件实现放在 provider/policy/core/tool。
// 这条断言防的是「清单写错目录」——曾经 plan 的清单就被写进了 plugins/seam/plan,
// 而真正的实现是 plugins/core/plan。
func TestSeamDirsHoldContractsOnly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "plugins"))
	if err != nil {
		t.Fatalf("解析插件根目录失败: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "seam"))
	if err != nil {
		t.Fatalf("读取 seam 目录失败: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, "seam", e.Name(), "plugin.json")
		if _, err := os.Stat(path); err != nil {
			continue // 契约包本来就不该有清单
		}
		m, err := plugin.Parse(mustRead(t, path), path)
		if err != nil {
			t.Fatalf("seam 目录下的清单无效(%s): %v", path, err)
		}
		if !strings.HasPrefix(m.ID, "seam-") {
			t.Fatalf("plugins/seam/%s 的清单 id=%q 不是 seam 插件;插件实现应放在 provider/policy/core/tool", e.Name(), m.ID)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return data
}

// TestFindBaseAnchors 覆盖装配基目录的两种锚点:发布包的 .ada-base 与源码根的 go.mod。
func TestFindBaseAnchors(t *testing.T) {
	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, BaseMarker), []byte("root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	presets := filepath.Join(pkg, "presets")
	if err := os.MkdirAll(presets, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindBase(presets); got != pkg {
		t.Fatalf("FindBase(%q)=%q, want %q", presets, got, pkg)
	}

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(src, "examples", "agent")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindBase(nested); got != src {
		t.Fatalf("FindBase(%q)=%q, want %q", nested, got, src)
	}

	if got := FindBase(t.TempDir()); got != "" {
		t.Fatalf("没有锚点的目录应返回空串,得到 %q", got)
	}
}

func TestSameIndir(t *testing.T) {
	dir := t.TempDir()
	if !SameIndir(dir, filepath.Join(dir, ".")) {
		t.Fatal("同一目录应判定为同一路径")
	}
	if SameIndir(dir, filepath.Join(dir, "missing")) {
		t.Fatal("不同路径不应判定为同一目录")
	}
}
