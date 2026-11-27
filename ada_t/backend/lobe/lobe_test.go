package lobeui

import (
	"testing"

	"github.com/issueye/ada_t/backend"
	"github.com/issueye/ada_t/lobe"
	"github.com/issueye/ada_t/widget"
)

func TestNewDefaults(t *testing.T) {
	b := New()
	if !b.themeOn {
		t.Fatalf("默认应开启 lobe-mygo 原生主题")
	}
	if b.theme != nil {
		t.Fatalf("默认 theme 应为 nil(用 ada_t/lobe.Default())")
	}
}

func TestLobeComponentsDefaultOn(t *testing.T) {
	b := New()
	if !b.lobeComponents {
		t.Fatalf("默认应开启 widget→lobe-mygo 组件映射")
	}
	if New(WithoutLobeComponents()).lobeComponents {
		t.Fatalf("WithoutLobeComponents 未生效")
	}
}

func TestWithThemeAndWithout(t *testing.T) {
	custom := lobe.Default()
	b := New(WithTheme(custom))
	if b.theme != custom {
		t.Fatalf("WithTheme 未生效")
	}
	b2 := New(WithoutTheme())
	if b2.themeOn {
		t.Fatalf("WithoutTheme 未生效")
	}
}

func TestApplyStoresAndRemoves(t *testing.T) {
	b := New()
	n := &widget.Node{ID: "root", Tag: "Column"}
	b.Apply("w", n)
	b.mu.Lock()
	got := b.trees["w"]
	b.mu.Unlock()
	if got != n {
		t.Fatalf("Apply 未存储树")
	}
	b.Apply("w", nil)
	b.mu.Lock()
	_, ok := b.trees["w"]
	b.mu.Unlock()
	if ok {
		t.Fatalf("Apply(nil) 应移除树")
	}
}

func TestDispatchSet(t *testing.T) {
	b := New()
	var called bool
	b.SetDispatch(func(winID, hid string, payload any) { called = true })
	b.mu.Lock()
	fn := b.dispatch
	b.mu.Unlock()
	if fn == nil {
		t.Fatalf("SetDispatch 未生效")
	}
	fn("w", "h", nil)
	if !called {
		t.Fatalf("dispatch 未被调用")
	}
}

func TestContracts(t *testing.T) {
	var _ backend.Backend = New()
	var _ backend.Dispatcher = New()
	var _ backend.ToastSource = New()
}
