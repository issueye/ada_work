package loader_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_jsrt/compile"
)

const tsPlugin = `type Ctx = { log: (m: string) => void }
export function apply(ctx: Ctx): void { ctx.log("loaded") }`

func mountState(t *testing.T, p kernel.Plugin) kernel.State {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	return f.State()
}

// 1. go 插件从宿主注册表取;缺注册必须报明确错误。
func TestGoLookup(t *testing.T) {
	lookup := loader.Lookup(loader.Options{Go: map[string]func() kernel.Plugin{
		"g": func() kernel.Plugin {
			return kernel.Func("g", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) { return nil, nil })
		},
	}})

	p, err := lookup(&plugin.Manifest{ID: "g", Kind: "go", Version: "0.1.0"})
	if err != nil || p == nil {
		t.Fatalf("应取到 go 插件: %v", err)
	}
	if got := mountState(t, p); got != kernel.Active {
		t.Fatalf("go 插件应 active,实际 %s", got)
	}

	if _, err := lookup(&plugin.Manifest{ID: "missing", Kind: "go", Version: "0.1.0"}); err == nil || !strings.Contains(err.Error(), "注册") {
		t.Fatalf("缺注册应报错,实际 %v", err)
	}
}

// 2. ts 源码模式:现场扫描 + 编译 + 挂载。
func TestTSSourceLookup(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.ts"), []byte(tsPlugin), 0o644); err != nil {
		t.Fatalf("写源码失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{})
	p, err := lookup(&plugin.Manifest{ID: "ts-a", Kind: "ts", Version: "0.1.0", Entry: "plugin.ts", Dir: dir})
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if got := mountState(t, p); got != kernel.Active {
		t.Fatalf("ts 插件应 active,实际 %s", got)
	}
}

// 3. ts Artifact 模式:优先读编译产物,不重新编译源码。
func TestTSArtifactLookup(t *testing.T) {
	dir := t.TempDir()
	artifact, err := compile.Build("ts-b", "plugin.ts", map[string]string{"plugin.ts": tsPlugin}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	data, err := artifact.JSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifact.json"), data, 0o644); err != nil {
		t.Fatalf("写 Artifact 失败: %v", err)
	}

	lookup := loader.Lookup(loader.Options{})
	p, err := lookup(&plugin.Manifest{ID: "ts-b", Kind: "ts", Version: "0.1.0", Artifact: "artifact.json", Dir: dir})
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if got := mountState(t, p); got != kernel.Active {
		t.Fatalf("ts 插件应 active,实际 %s", got)
	}
}

// 4. 清单信息不足时给出明确错误。
func TestTSLookupErrors(t *testing.T) {
	lookup := loader.Lookup(loader.Options{})
	if _, err := lookup(&plugin.Manifest{ID: "x", Kind: "ts", Version: "0.1.0", Dir: t.TempDir()}); err == nil {
		t.Fatalf("既无 entry 又无 artifact 应报错")
	}
	if _, err := lookup(&plugin.Manifest{ID: "x", Kind: "ts", Version: "0.1.0", Artifact: "nope.json", Dir: t.TempDir()}); err == nil {
		t.Fatalf("Artifact 不存在应报错")
	}
	if _, err := lookup(&plugin.Manifest{ID: "x", Kind: "wasm", Version: "0.1.0"}); err == nil {
		t.Fatalf("未知 kind 应报错")
	}
}

// 5. 源码扫描:跳过 node_modules 与隐藏目录,只收 TS/JS 源码。
func TestCollectSources(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}
	write("plugin.ts", tsPlugin)
	write("src/util.ts", `export const v = 1`)
	write("node_modules/dep/index.js", `module.exports = 1`)
	write(".git/hook.ts", `export {}`)
	write("README.md", "# doc")

	files, err := loader.CollectSources(dir)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if _, ok := files["plugin.ts"]; !ok {
		t.Fatalf("应收集 plugin.ts: %v", files)
	}
	if _, ok := files["src/util.ts"]; !ok {
		t.Fatalf("应收集 src/util.ts: %v", files)
	}
	for _, bad := range []string{"node_modules/dep/index.js", ".git/hook.ts", "README.md"} {
		if _, ok := files[bad]; ok {
			t.Fatalf("不应收集 %s", bad)
		}
	}
}
