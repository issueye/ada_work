package widget

import (
	"strings"
	"testing"
)

func sampleTree() *Node {
	return &Node{
		ID: "n1", Tag: "Column",
		Props: map[string]any{"gap": 12.0},
		Children: []*Node{
			{ID: "n2", Tag: "Text", Props: map[string]any{"text": "hi"}},
			{ID: "n3", Tag: "Button", Props: map[string]any{"label": "go"},
				Events: map[string]string{"click": "h1"}},
			{ID: "n4", Tag: "TextInput", Model: "state.query",
				Props: map[string]any{"value": ""}},
		},
	}
}

func TestWalkFind(t *testing.T) {
	tree := sampleTree()
	if tree.Find("n3") == nil {
		t.Fatal("Find n3 应命中")
	}
	if tree.Find("zz") != nil {
		t.Fatal("Find zz 应为 nil")
	}
	count := 0
	tree.Walk(func(n *Node) bool {
		count++
		return true
	})
	if count != 4 {
		t.Fatalf("Walk 应遍历 4 个节点,得到 %d", count)
	}
}

func TestText(t *testing.T) {
	tree := sampleTree()
	if tree.Children[0].Text() != "hi" {
		t.Fatalf("Text = %q", tree.Children[0].Text())
	}
	if tree.Children[1].Text() != "go" {
		t.Fatalf("label 回退 = %q", tree.Children[1].Text())
	}
}

func TestDump(t *testing.T) {
	out := sampleTree().Dump()
	for _, want := range []string{"Column#n1", "gap=12", "Text#n2", "text=hi", "events=[click]", "model=state.query"} {
		if !strings.Contains(out, want) {
			t.Errorf("Dump 缺少 %q:\n%s", want, out)
		}
	}
}
