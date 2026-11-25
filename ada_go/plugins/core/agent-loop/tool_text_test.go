package agentloop_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// mountToolText 挂载唯一的 TS 文件工具(text-helper);Go 内置注册表里没有它。
func mountToolText(t *testing.T, app *kernel.App) {
	t.Helper()
	m, err := plugin.Load("../../../plugins/tool/text-helper")
	if err != nil {
		t.Fatalf("读取 text-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(m)
	if err != nil {
		t.Fatalf("构造 text-helper 失败: %v", err)
	}
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载 text-helper 失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("text-helper 未就绪: %v", err)
	}
}
