package mygoui

import (
	"testing"

	"github.com/issueye/ada_t/lobe"
	"github.com/issueye/ada_t/widget"
)

func TestNewDefaultsLobeOn(t *testing.T) {
	b := New()
	if !b.lobeOn {
		t.Fatalf("默认应开启 lobe-mygo 原生主题")
	}
	if b.lobeTheme != nil {
		t.Fatalf("默认 lobeTheme 应为 nil(用 lobe.Default())")
	}
}

func TestWithoutLobeTheme(t *testing.T) {
	b := New(WithoutLobeTheme())
	if b.lobeOn {
		t.Fatalf("WithoutLobeTheme 应关闭 lobe 主题")
	}
}

func TestWithLobeTheme(t *testing.T) {
	custom := lobe.Default()
	b := New(WithLobeTheme(custom))
	if !b.lobeOn || b.lobeTheme != custom {
		t.Fatalf("WithLobeTheme 未生效")
	}
	// 已关闭后再开启。
	b2 := New(WithoutLobeTheme(), WithLobeTheme(custom))
	if !b2.lobeOn || b2.lobeTheme != custom {
		t.Fatalf("option 顺序应后者生效")
	}
}

func TestRendererLobeOptIn(t *testing.T) {
	r := NewRenderer()
	if r.lobeOn {
		t.Fatalf("Renderer 默认应关闭 lobe(宿主自管 mygo 主题)")
	}
	custom := lobe.Default()
	r.SetLobeTheme(custom)
	if !r.lobeOn || r.lobeTheme != custom {
		t.Fatalf("SetLobeTheme 未生效")
	}
	r.DisableLobeTheme()
	if r.lobeOn {
		t.Fatalf("DisableLobeTheme 未生效")
	}
}

func TestApplyStillStoresTree(t *testing.T) {
	b := New()
	n := &widget.Node{ID: "root", Tag: "Column"}
	b.Apply("w1", n)
	if b.trees["w1"] != n {
		t.Fatalf("Apply 未存储树")
	}
	b.Apply("w1", nil)
	if _, ok := b.trees["w1"]; ok {
		t.Fatalf("Apply(nil) 应移除窗口树")
	}
}
