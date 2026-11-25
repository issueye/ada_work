package ui_test

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	mui "github.com/egoist/mygo/ui"

	"ada_chat/internal/app"
	"ada_chat/internal/ui"
)

// wildcardApp 造一份最小状态:一条正在生成的会话 + 一条已结束的会话。
// 只填导出字段,不碰配置/状态文件,测试因此与机器无关。
func wildcardApp() *app.App {
	now := time.Now()
	a := &app.App{ActiveID: 1}
	a.Sessions = []*app.Session{
		{
			ID: 1, Title: "你是谁", Preview: "我先去搜一下最新动态", Updated: now,
			Messages: []*app.Message{{ID: 1, Role: app.RoleAssistant, At: now, Streaming: true}},
		},
		{ID: 2, Title: "今天关于 AI 的新闻", Preview: "采集和对比已经完成", Updated: now.Add(-90 * time.Minute)},
	}
	return a
}

// shot 把当前帧写成 PNG(只在 ADA_CHAT_SHOT_DIR 设置时落盘),供人工核对动画位置。
func shot(t *testing.T, tt *mui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("ADA_CHAT_SHOT_DIR")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

// TestRunningSessionSpins 守住「运行的会话在侧栏有 loading 动画」:
// 生成中的行持续请求帧(spinner 在转),结束后视图必须能停下来。
func TestRunningSessionSpins(t *testing.T) {
	a := wildcardApp()
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 620, 720)
	tt.Frame()
	if !tt.FramePending() {
		t.Fatalf("生成中的会话应在侧栏持续请求帧(loading 动画)")
	}
	first := copyRGBA(tt.Image())
	shot(t, tt, "sidebar-running.png")

	// 动画必须真的在动:推进一帧后像素应发生变化。
	tt.Advance(200 * time.Millisecond)
	tt.Frame()
	second := copyRGBA(tt.Image())
	if equalRGBA(first, second) {
		t.Fatalf("loading 动画没有动:两帧像素完全相同")
	}

	// 停掉这一轮:spinner 消失,视图应当能静下来(过渡/settle 需要几帧)。
	a.Sessions[0].Messages[0].Streaming = false
	for i := 0; i < 8 && tt.FramePending(); i++ {
		tt.Advance(200 * time.Millisecond)
		tt.Frame()
	}
	if tt.FramePending() {
		t.Fatalf("会话结束后不应再持续请求帧")
	}
	shot(t, tt, "sidebar-idle.png")
}

func copyRGBA(img *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(img.Bounds())
	copy(dst.Pix, img.Pix)
	return dst
}

func equalRGBA(a, b *image.RGBA) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return false
		}
	}
	return true
}

// TestRunningSessionRowKeepsTitle 确认自绘行没有丢掉可访问名字(按钮的 Label 仍是会话标题)。
func TestRunningSessionRowKeepsTitle(t *testing.T) {
	a := wildcardApp()
	u := ui.New(a)
	tt := mui.NewTester(u.Root, 620, 720)
	tt.Frame()
	if _, ok := tt.Access("你是谁"); !ok {
		t.Fatalf("侧栏行应以会话标题作为可访问名字")
	}
}
