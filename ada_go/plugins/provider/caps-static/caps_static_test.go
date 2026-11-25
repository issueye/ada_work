package capsstatic_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	capsstatic "github.com/issueye/ada_go_v2/plugins/provider/caps-static"
)

// caps-static 提供 *caps.Broker 服务,grants 来自配置。
func TestProvidesBroker(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(capsstatic.Plugin(), kernel.NewConfig(map[string]any{
		"grants": map[string][]string{"p": {"fs.read"}},
	})); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	broker, err := kernel.Service[*caps.Broker](app.Root(), "caps")
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	if err := broker.Authorize(context.Background(), "p", "fs.read", "/workspace/a"); err != nil {
		t.Fatalf("已授权能力应通过: %v", err)
	}
	if err := broker.Authorize(context.Background(), "p", "fs.write", "/workspace/a"); err == nil {
		t.Fatalf("未授权能力应拒绝")
	}
}
