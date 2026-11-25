package main

// 无显示器验收示例:用 mygo 的 ui.Tester 渲染宿主的真实 view
// (含两个插件槽位),点插件按钮并等服务回传。

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

func TestHostViewRendersPluginSlots(t *testing.T) {
	app, b, err := newHost(repoRoot())
	if err != nil {
		t.Fatalf("装配宿主: %v", err)
	}
	defer func() { _ = app.Stop() }()

	tt := ui.NewTester(hostView(app, b), 860, 560)
	// 首帧:宿主 chrome + 两个槽位的插件内容
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tt.Frame()
		if tt.HasText("Ada 插件宿主") && tt.HasText("👋 hello 插件") && tt.HasText("插件卡片") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !tt.HasText("👋 hello 插件") {
		t.Fatalf("sidebar.item 槽位内容未绘制: %v", tt.Texts())
	}
	if !tt.HasText("插件卡片") {
		t.Fatalf("composer.action 槽位内容未绘制: %v", tt.Texts())
	}

	// 点击插件按钮 → embed dispatch → rt.Fire → host.greet(能力白名单内)
	if err := tt.Click("向宿主打招呼"); err != nil {
		t.Fatalf("点击插件按钮: %v", err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tt.Frame()
		if tt.HasText("宿主回复") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !tt.HasText("宿主回复") {
		t.Fatalf("插件服务调用未回传: %v", tt.Texts())
	}
}
