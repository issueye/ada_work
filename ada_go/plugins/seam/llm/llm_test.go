package llm_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type fakeProvider struct{ name string }

func (f fakeProvider) Name() string { return f.name }
func (f fakeProvider) Version() int { return 1 }
func (f fakeProvider) Models(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "m"}}, nil
}
func (f fakeProvider) Complete(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}}, nil
}
func (f fakeProvider) Stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	if err := onDelta(llm.Delta{Kind: llm.DeltaText, Text: "ok"}); err != nil {
		return llm.Response{}, err
	}
	if err := onDelta(llm.Delta{Kind: llm.DeltaDone}); err != nil {
		return llm.Response{}, err
	}
	return f.Complete(ctx, req)
}

func TestRegistryDefaultAndDuplicate(t *testing.T) {
	r := llm.NewRegistry()
	if _, err := r.Get(""); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("空表应 EUNIMPLEMENTED, 实际 %v", err)
	}
	if _, err := r.Register(fakeProvider{name: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(fakeProvider{name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(fakeProvider{name: "a"}); err == nil {
		t.Fatal("重复 provider 应失败")
	}
	p, err := r.Get("")
	if err != nil || p.Name() != "a" {
		t.Fatalf("默认应取排序第一个 a, 实际 %v %v", p, err)
	}
}
