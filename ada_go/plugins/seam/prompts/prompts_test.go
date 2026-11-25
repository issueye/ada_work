package prompts_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/plugins/seam/prompts"
)

func TestOrderStable(t *testing.T) {
	e := prompts.NewEngine(100)
	if _, err := e.Section(prompts.Section{Key: "b", Title: "B", Body: "bbbbbb", Order: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Section(prompts.Section{Key: "a", Title: "A", Body: "aaaaaa", Order: 1}); err != nil {
		t.Fatal(err)
	}
	asm, err := e.Assemble(context.Background(), prompts.AssembleInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(asm.Text, "## A") || strings.Index(asm.Text, "## A") > strings.Index(asm.Text, "## B") {
		t.Fatalf("顺序不稳定: %q", asm.Text)
	}
}

func TestBudgetTrim(t *testing.T) {
	e := prompts.NewEngine(6)
	if _, err := e.Section(prompts.Section{Key: "b", Title: "B", Body: "bbbbbb", Order: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Section(prompts.Section{Key: "a", Title: "A", Body: "aaaaaa", Order: 1}); err != nil {
		t.Fatal(err)
	}
	asm, err := e.Assemble(context.Background(), prompts.AssembleInput{})
	if err != nil {
		t.Fatal(err)
	}
	if asm.Tokens > 6 {
		t.Fatalf("应裁剪到 6 token,实际 %d", asm.Tokens)
	}
	if len(asm.Dropped) == 0 || asm.Dropped[0] != "b" {
		t.Fatalf("应从尾部裁 b,实际 %v", asm.Dropped)
	}
}

func TestDuplicateKey(t *testing.T) {
	e := prompts.NewEngine(100)
	s := prompts.Section{Key: "identity", Body: "x"}
	if _, err := e.Section(s); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Section(s); err == nil {
		t.Fatal("重复 key 应失败")
	}
}
