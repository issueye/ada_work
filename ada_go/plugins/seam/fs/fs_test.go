package fs_test

import (
	"context"
	"strings"
	"testing"

	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

// base 是最内层的假实现。
type base struct{ calls []string }

func (b *base) Stat(context.Context, seamfs.Path) (seamfs.FileInfo, error) {
	return seamfs.FileInfo{}, nil
}
func (b *base) List(context.Context, seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return nil, nil
}
func (b *base) Read(context.Context, seamfs.ReadRequest) (seamfs.ReadResult, error) {
	b.calls = append(b.calls, "base")
	return seamfs.ReadResult{Content: "ok"}, nil
}
func (b *base) Write(context.Context, seamfs.WriteRequest) (seamfs.WriteResult, error) {
	b.calls = append(b.calls, "base")
	return seamfs.WriteResult{}, nil
}
func (b *base) Edit(context.Context, seamfs.EditRequest) (seamfs.EditResult, error) {
	b.calls = append(b.calls, "base")
	return seamfs.EditResult{}, nil
}
func (b *base) Search(context.Context, seamfs.SearchRequest) (seamfs.SearchResult, error) {
	return seamfs.SearchResult{}, nil
}

// wrapper 记录自己被调用,然后转给内层。
type wrapper struct {
	inner seamfs.FS
	tag   string
	*base
}

func (w *wrapper) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.Read(ctx, req)
}
func (w *wrapper) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.Stat(ctx, p)
}
func (w *wrapper) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.List(ctx, req)
}
func (w *wrapper) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.Write(ctx, req)
}
func (w *wrapper) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.Edit(ctx, req)
}
func (w *wrapper) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	w.base.calls = append(w.base.calls, w.tag)
	return w.inner.Search(ctx, req)
}

// 1. 未安装 → EUNIMPLEMENTED;安装后按优先级组链(高者在外层)。
func TestChainCompose(t *testing.T) {
	b := &base{}
	chain := seamfs.NewChain()
	if _, err := chain.Get(); err == nil || !strings.Contains(err.Error(), "EUNIMPLEMENTED") {
		t.Fatalf("未安装底座应返回 EUNIMPLEMENTED,实际 %v", err)
	}
	if _, err := chain.Install(b); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := chain.Install(&base{}); err == nil {
		t.Fatalf("重复安装应报错")
	}

	low, err := chain.Decorate(func(inner seamfs.FS) seamfs.FS {
		return &wrapper{inner: inner, tag: "low", base: b}
	}, 20)
	if err != nil {
		t.Fatalf("注册装饰器失败: %v", err)
	}
	high, err := chain.Decorate(func(inner seamfs.FS) seamfs.FS {
		return &wrapper{inner: inner, tag: "high", base: b}
	}, 100)
	if err != nil {
		t.Fatalf("注册装饰器失败: %v", err)
	}

	fsys, err := chain.Get()
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if _, err := fsys.Read(context.Background(), seamfs.ReadRequest{}); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if got := strings.Join(b.calls, ">"); got != "high>low>base" {
		t.Fatalf("装饰器顺序不正确: %s", got)
	}
	if got := chain.Applied(); len(got) != 2 || got[0] != 100 || got[1] != 20 {
		t.Fatalf("Applied 顺序不正确: %v", got)
	}

	if err := high(context.Background()); err != nil {
		t.Fatalf("dispose 失败: %v", err)
	}
	fsys, err = chain.Get()
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	b.calls = nil
	if _, err := fsys.Read(context.Background(), seamfs.ReadRequest{}); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if got := strings.Join(b.calls, ">"); got != "low>base" {
		t.Fatalf("dispose 后装饰器顺序不正确: %s", got)
	}
	_ = low
}
