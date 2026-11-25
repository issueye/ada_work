package ui_test

import (
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/ui"
)

// menuFrame 在侧栏会话行上右键并等一帧,让窗口内的菜单画出来。
//
// 菜单与会话行、机器人行共用 overlay.ContextMenu(窗口内绘制),不是系统菜单,
// 所以要按文本点条目,而不是走 ChooseMenuItem 的系统菜单模型。
func menuFrame(t *testing.T, tt *mui.Tester, title string) {
	t.Helper()
	if err := tt.RightClick(title); err != nil {
		t.Fatalf("右键 %q 失败: %v", title, err)
	}
	tt.Advance(50 * time.Millisecond)
	tt.Frame()
}

// choose 点窗口内菜单的一项,并等菜单关闭后的那一帧。
func choose(t *testing.T, tt *mui.Tester, label string) {
	t.Helper()
	if err := tt.Click(label); err != nil {
		t.Fatalf("点菜单项 %q 失败: %v", label, err)
	}
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
}

// TestSessionContextMenuCopyAndArchive 守住会话右键菜单:
// 复制会话 ID 进剪贴板;归档后这一行从「今天」移到「归档」分组。
func TestSessionContextMenuCopyAndArchive(t *testing.T) {
	a := baseApp(t)
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 760)
	tt.Frame()

	menuFrame(t, tt, "你好")
	choose(t, tt, "复制会话 ID")
	if got := tt.Clipboard(); got != "1" {
		t.Fatalf("剪贴板应为会话 ID,实际 %q", got)
	}

	menuFrame(t, tt, "你好")
	choose(t, tt, "归档")
	if _, ok := tt.Find("归档"); !ok {
		t.Fatalf("归档后应出现「归档」分组")
	}
	if !a.Sessions[0].Archived {
		t.Fatalf("会话没有被标记为归档")
	}
	shot(t, tt, "session-archived.png")

	// 取消归档回到主列表。
	menuFrame(t, tt, "你好")
	choose(t, tt, "取消归档")
	if a.Sessions[0].Archived {
		t.Fatalf("取消归档后标记应清除")
	}
}

// TestSessionContextMenuRemove 守住「移除」:先开确认弹窗,确认后对话消失。
func TestSessionContextMenuRemove(t *testing.T) {
	a := baseApp(t)
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 980, 760)
	tt.Frame()

	menuFrame(t, tt, "你好")
	choose(t, tt, "移除")
	if _, ok := tt.Find("删除对话"); !ok {
		t.Fatalf("移除应先打开确认弹窗")
	}
	choose(t, tt, "删除")
	if len(a.Sessions) != 0 {
		t.Fatalf("确认后应删除对话,实际 %d", len(a.Sessions))
	}
	// 删除后没有会话分组;标题仍可能出现在「已删除」提示里,所以按分组断言。
	if _, ok := tt.Find("今天"); ok {
		t.Fatalf("删除后侧栏不应再有会话分组")
	}
}
