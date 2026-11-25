package embed

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/widget"
)

func TestApplyDrawAndFire(t *testing.T) {
	b := New(Options{})
	tree := &widget.Node{ID: "root", Tag: "Column", Children: []*widget.Node{
		{ID: "t", Tag: "Text", Props: map[string]any{"text": "slot-hello"}},
		{ID: "btn", Tag: "PrimaryButton", Props: map[string]any{"label": "Go"}, Events: map[string]string{"click": "h1"}},
	}}
	b.Apply("slot-1", tree)
	if b.Tree("slot-1") == nil {
		t.Fatal("Apply 后应能取到树")
	}
	if slots := b.Slots(); len(slots) != 1 || slots[0] != "slot-1" {
		t.Fatalf("Slots = %v", slots)
	}

	tt := ui.NewTester(func(c *ui.Context) { b.Draw(c, "slot-1") }, 320, 200)
	if !tt.HasText("slot-hello") {
		t.Fatalf("Draw 未画出槽位内容: %v", tt.Texts())
	}

	var fired []string
	b.SetDispatch(func(slotID, hid string, payload any) { fired = append(fired, slotID+":"+hid) })
	if !b.Fire("slot-1", "btn", "click", nil) {
		t.Fatal("Fire 应找到 handler")
	}
	if len(fired) != 1 || fired[0] != "slot-1:h1" {
		t.Fatalf("dispatch = %v", fired)
	}
	if b.Fire("slot-1", "btn", "submit", nil) {
		t.Fatal("不存在的事件不应派发")
	}

	// Draw 内的点击检测同样经 Dispatch 回传
	if err := tt.Click("Go"); err != nil {
		t.Fatalf("点击: %v", err)
	}
	if len(fired) != 2 {
		t.Fatalf("Draw 内点击未回传: %v", fired)
	}

	// 卸载:树清空,Draw 画占位文本
	b.Apply("slot-1", nil)
	if b.Tree("slot-1") != nil || len(b.Slots()) != 0 {
		t.Fatal("Apply(nil) 后仍保留槽位树")
	}
	empty := ui.NewTester(func(c *ui.Context) { b.Draw(c, "slot-1") }, 200, 100)
	if !empty.HasText("…") {
		t.Fatalf("空槽位应画占位: %v", empty.Texts())
	}
}

// TestToastSource:插件 ctx.toast 的队列能被宿主拉取展示。
func TestToastSource(t *testing.T) {
	b := New(Options{})
	var src []string
	b.SetToastSource(func(slotID string) []string {
		if slotID == "slot-1" {
			q := src
			src = nil
			return q
		}
		return nil
	})
	src = []string{"已保存"}
	if got := b.DrainToasts("slot-1"); len(got) != 1 || got[0] != "已保存" {
		t.Fatalf("DrainToasts = %v", got)
	}
	if got := b.DrainToasts("other"); got != nil {
		t.Fatalf("其它槽位应无提示: %v", got)
	}
}

func TestRunRejectsWindowsAndStops(t *testing.T) {
	b := New(Options{})
	errs := make(chan error, 1)
	go func() { errs <- b.Run([]backend.WindowSpec{{ID: "w"}}) }()
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("embed 不接受 WindowSpec(窗口由宿主负责)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 应立即返回错误")
	}

	done := make(chan error, 1)
	go func() { done <- b.Run(nil) }()
	time.Sleep(20 * time.Millisecond)
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run(nil): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 后 Run 未返回")
	}
	if err := b.Stop(); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
}
