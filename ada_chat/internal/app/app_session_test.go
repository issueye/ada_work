package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sessionFixture() *App {
	now := time.Now()
	a := &App{ActiveID: 1}
	a.Sessions = []*Session{
		{ID: 1, Title: "第一条", Updated: now},
		{ID: 2, Title: "第二条", Updated: now.Add(-time.Hour)},
	}
	a.nextID = 3
	return a
}

// TestArchiveSessionSwitchesActive 归档当前对话时切到一条未归档的,并把标记落盘。
func TestArchiveSessionSwitchesActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)

	a := sessionFixture()
	if !a.ArchiveSession(1, true) {
		t.Fatalf("归档应成功")
	}
	if !a.Sessions[0].Archived {
		t.Fatalf("归档标记没有写进会话")
	}
	if a.ActiveID != 2 {
		t.Fatalf("归档当前对话后应切到未归档的,实际 ActiveID=%d", a.ActiveID)
	}

	// 直接读状态文件:New() 会丢掉 BotID 不存在(夹具里没有机器人)的会话。
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取状态文件失败: %v", err)
	}
	var saved savedState
	if err := json.Unmarshal(disk, &saved); err != nil {
		t.Fatalf("状态文件不是合法 JSON: %v", err)
	}
	if len(saved.Sessions) != 2 || !saved.Sessions[0].Archived {
		t.Fatalf("落盘后归档标记丢失: %+v", saved.Sessions)
	}

	if !a.ArchiveSession(1, false) {
		t.Fatalf("取消归档应成功")
	}
	if a.Sessions[0].Archived {
		t.Fatalf("取消归档后标记应为 false")
	}
}

// TestDeleteSessionSwitchesActive 删除当前对话时切到下一条;删光后 ActiveID 归零。
func TestDeleteSessionSwitchesActive(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	a := sessionFixture()
	if _, ok := a.DeleteSession(1); !ok {
		t.Fatalf("删除应成功")
	}
	if a.ActiveID != 2 {
		t.Fatalf("删除当前对话后应切到剩下的,实际 %d", a.ActiveID)
	}
	if _, ok := a.DeleteSession(2); !ok {
		t.Fatalf("删除第二条应成功")
	}
	if a.ActiveID != 0 || len(a.Sessions) != 0 {
		t.Fatalf("删光后应回到空工作台: active=%d sessions=%d", a.ActiveID, len(a.Sessions))
	}
}

// TestOpenBotChatSkipsArchived 归档一条对话后,再打开该机器人应新建一条,
// 而不是把用户丢回归档里的旧对话。
func TestOpenBotChatSkipsArchived(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	withStatePath(t, path)

	a := New()
	bot, err := a.AddBot(Bot{Name: "小助手", Workspace: dir})
	if err != nil {
		t.Fatalf("添加机器人失败: %v", err)
	}
	a.OpenBotChat(bot.ID)
	if len(a.Sessions) != 1 {
		t.Fatalf("应有一条对话,实际 %d", len(a.Sessions))
	}
	first := a.Sessions[0]
	a.ArchiveSession(first.ID, true)

	if !a.OpenBotChat(bot.ID) {
		t.Fatalf("再次打开机器人失败")
	}
	if len(a.Sessions) != 2 {
		t.Fatalf("归档后应新建一条对话,实际 %d", len(a.Sessions))
	}
	fresh := a.Current()
	if fresh == nil || fresh.ID == first.ID {
		t.Fatalf("当前对话应指向新建的那条: %+v", fresh)
	}
	if fresh.Archived {
		t.Fatalf("新对话不应是归档状态")
	}
}

// TestSaveStateKeepsBackup 覆盖前留一份 .bak:万一状态被写空,机器人还能捞回来。
func TestSaveStateKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	withStatePath(t, path)
	const old = `{"version":1,"nextId":9,"activeId":0}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{nextID: 3}
	if err := a.SaveState(); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("没有生成备份: %v", err)
	}
	if string(bak) != old {
		t.Fatalf("备份内容不对: %s", bak)
	}
}
