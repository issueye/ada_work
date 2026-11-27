package bridge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

func mountBuiltin(t *testing.T, app *kernel.App, id string, cfg map[string]any) {
	t.Helper()
	mk := builtins.Plugins()[id]
	if mk == nil {
		t.Fatalf("内置插件缺失: %s", id)
	}
	f, err := app.Plugin(mk(), kernel.NewConfig(cfg))
	if err != nil {
		t.Fatalf("挂载 %s 失败: %v", id, err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("%s 未就绪: %v", id, err)
	}
}

// TestJSToolText 用唯一的 TS 文件工具(text-helper)+ fs-local 覆盖读写/编辑/搜索/列目录。
func TestJSToolText(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})

	manifest, err := plugin.Load("../../plugins/tool/text-helper")
	if err != nil {
		t.Fatalf("读取 text-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 text-helper 失败: %v", err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)

	call := func(name string, args map[string]any) tools.Result {
		t.Helper()
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: name, Args: data})
		if err != nil {
			t.Fatalf("%s 调用失败: %v", name, err)
		}
		return res
	}

	if res := call("write_file", map[string]any{"path": "a.txt", "content": "hello world"}); res.Status != tools.StatusOK {
		t.Fatalf("write_file 失败: %+v", res)
	}
	if res := call("read_file", map[string]any{"path": "a.txt"}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "hello world") {
		t.Fatalf("read_file 失败: %+v", res)
	}
	if res := call("edit_file", map[string]any{"path": "a.txt", "old": "world", "new": "ada"}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "1 处") {
		t.Fatalf("edit_file 失败: %+v", res)
	}
	if res := call("search_files", map[string]any{"query": "ada"}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "a.txt") {
		t.Fatalf("search_files 失败: %+v", res)
	}
	if res := call("list_dir", map[string]any{}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "a.txt") {
		t.Fatalf("list_dir 失败: %+v", res)
	}

	// 空结果必须是空数组而不是 null:TS 侧直接读 length,遇到 null 会抛 TypeError。
	if res := call("search_files", map[string]any{"query": "没有这段文字-zzz"}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "(无匹配)") {
		t.Fatalf("空搜索结果应返回 (无匹配): %+v", res)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := call("list_dir", map[string]any{"path": "/workspace/empty"}); res.Status != tools.StatusOK || !strings.Contains(res.Content[0].Text, "(空目录)") {
		t.Fatalf("空目录应返回 (空目录): %+v", res)
	}

	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(data) != "hello ada" {
		t.Fatalf("文件内容不正确: %v %q", err, data)
	}
}
