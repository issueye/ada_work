package headless

import (
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/widget"
)

func TestApplyFrameAndTree(t *testing.T) {
	b := New()
	tree := &widget.Node{ID: "n1", Tag: "Text", Props: map[string]any{"text": "hi"}}
	b.Apply("w", tree)
	if got := b.Tree("w"); got == nil || got.Text() != "hi" {
		t.Fatalf("Tree = %v", got)
	}
	if n := b.FrameCount("w"); n != 1 {
		t.Fatalf("FrameCount = %d", n)
	}
	if logs := b.Log(); len(logs) != 1 || !strings.Contains(logs[0], "apply w frame=1") {
		t.Fatalf("Log = %v", logs)
	}
	// Apply(nil) 表示窗口移除:树清空,但仍记一帧。
	b.Apply("w", nil)
	if b.Tree("w") != nil {
		t.Fatal("Apply(nil) 后 Tree 应为 nil")
	}
	if n := b.FrameCount("w"); n != 2 {
		t.Fatalf("FrameCount = %d, want 2", n)
	}
}

func TestFireUsesHandlerInTree(t *testing.T) {
	b := New()
	var got []string
	b.SetDispatch(func(winID, hid string, payload any) {
		got = append(got, winID+":"+hid)
	})
	b.Apply("w", &widget.Node{ID: "btn", Tag: "Button", Events: map[string]string{"click": "h1"}})
	if !b.Fire("w", "btn", "click", nil) {
		t.Fatal("Fire 应找到 handler")
	}
	if len(got) != 1 || got[0] != "w:h1" {
		t.Fatalf("dispatch = %v", got)
	}
	if b.Fire("w", "btn", "submit", nil) {
		t.Fatal("不存在的事件不应派发")
	}
	if b.Fire("missing", "btn", "click", nil) {
		t.Fatal("不存在的窗口不应派发")
	}
}

// TestStopBeforeRun:先 Stop 再 Run 必须立刻返回,不能永久阻塞。
func TestStopBeforeRun(t *testing.T) {
	b := New()
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Run([]backend.WindowSpec{{ID: "w"}}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 之后 Run 仍阻塞")
	}
	// 重复 Stop 幂等
	if err := b.Stop(); err != nil {
		t.Fatalf("重复 Stop: %v", err)
	}
}

// TestRunStops:正常 Run → Stop → Run 返回。
func TestRunStops(t *testing.T) {
	b := New()
	done := make(chan error, 1)
	go func() { done <- b.Run(nil) }()
	time.Sleep(20 * time.Millisecond)
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 后 Run 未返回")
	}
}
