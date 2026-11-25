package ui_test

import (
	"path/filepath"
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// baseApp 造一份最小状态:一个会话 + 一个用户名,侧栏底部才有可点的用户区。
// isolateState 把状态文件指到临时路径:UI 测试里的归档/删除会 persist,
// 绝不能写到本机真实的 state.json。
func isolateState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	restore := app.SetStatePath(filepath.Join(dir, "state.json"))
	t.Cleanup(restore)
	restoreCfg := app.SetConfigPath(filepath.Join(dir, "config.json"))
	t.Cleanup(restoreCfg)
}

// baseApp 造最小状态;同时把 state 文件指到临时路径——这里的操作会 persist,
// 不能写到本机真实 state.json(否则会清空用户已有的机器人和会话)。
func baseApp(t *testing.T) *app.App {
	t.Helper()
	isolateState(t)
	a := &app.App{ActiveID: 1}
	a.Prefs.Name = "老王"
	a.Sessions = []*app.Session{{ID: 1, Title: "你好", Preview: "开始聊点什么", Updated: time.Now()}}
	return a
}

// TestUserPopoverHasSettingsEntries 守住左下角用户区的 Popover:
// 点开后能看到 Agent / 供应商 / 常规设置 三个入口。
func TestUserPopoverHasSettingsEntries(t *testing.T) {
	a := baseApp(t)
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 720)
	tt.Frame()

	if err := tt.Click("老王"); err != nil {
		t.Fatalf("点用户区失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	for _, label := range []string{"Agent", "供应商", "常规设置"} {
		if _, ok := tt.Find(label); !ok {
			t.Fatalf("Popover 里缺少入口 %q", label)
		}
	}
	shot(t, tt, "user-popover.png")

	// 点 Agent 入口:打开的是 Agent 弹窗,而且 Popover 已经收起。
	if err := tt.Click("Agent"); err != nil {
		t.Fatalf("点 Agent 入口失败: %v", err)
	}
	tt.Advance(250 * time.Millisecond)
	tt.Frame()
	if _, ok := tt.Find("Agent 路径"); !ok {
		t.Fatalf("Agent 弹窗应显示 Agent 路径字段")
	}
	if _, ok := tt.Find("常规设置"); ok {
		t.Fatalf("打开弹窗后 Popover 应已收起")
	}
	if _, ok := tt.Find("个人资料"); ok {
		t.Fatalf("Agent 弹窗不应混入常规设置的字段")
	}
	shot(t, tt, "agent-dialog.png")
}

// TestProviderDialogIndependent 守住「供应商」是独立弹窗:
// 它显示供应商字段,且不显示 Agent 的路径字段。
func TestProviderDialogIndependent(t *testing.T) {
	a := baseApp(t)
	a.Prefs.Provider = "openai"
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 720)
	tt.Frame()

	if err := tt.Click("老王"); err != nil {
		t.Fatalf("点用户区失败: %v", err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	if err := tt.Click("供应商"); err != nil {
		t.Fatalf("点供应商入口失败: %v", err)
	}
	tt.Advance(250 * time.Millisecond)
	tt.Frame()

	if _, ok := tt.Find("Endpoint"); !ok {
		t.Fatalf("供应商弹窗应显示 Endpoint")
	}
	if _, ok := tt.Find("API Key"); !ok {
		t.Fatalf("供应商弹窗应显示 API Key")
	}
	if _, ok := tt.Find("Agent 路径"); ok {
		t.Fatalf("供应商弹窗不应显示 Agent 字段")
	}
	shot(t, tt, "provider-dialog.png")
}
