package fsredact_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	fsredact "github.com/issueye/ada_go_v2/plugins/policy/fs-redact"
	fsloc "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

type fakeFS struct {
	content string
	writes  int
}

func (f *fakeFS) Stat(context.Context, seamfs.Path) (seamfs.FileInfo, error) {
	return seamfs.FileInfo{}, nil
}
func (f *fakeFS) List(context.Context, seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return nil, nil
}
func (f *fakeFS) Read(context.Context, seamfs.ReadRequest) (seamfs.ReadResult, error) {
	return seamfs.ReadResult{Content: f.content, Kind: seamfs.KindText}, nil
}
func (f *fakeFS) Write(context.Context, seamfs.WriteRequest) (seamfs.WriteResult, error) {
	f.writes++
	return seamfs.WriteResult{Version: "v"}, nil
}
func (f *fakeFS) Edit(context.Context, seamfs.EditRequest) (seamfs.EditResult, error) {
	f.writes++
	return seamfs.EditResult{Version: "v"}, nil
}
func (f *fakeFS) Search(context.Context, seamfs.SearchRequest) (seamfs.SearchResult, error) {
	return seamfs.SearchResult{}, nil
}

// 1. 默认模式命中 GitHub token。
func TestRedactsDefault(t *testing.T) {
	fake := &fakeFS{content: "token=ghp_0123456789abcdefghijABCDEFGHIJ end"}
	fsys, err := fsredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, err := fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a"})
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if strings.Contains(res.Content, "ghp_0123456789") {
		t.Fatalf("token 未被脱敏: %q", res.Content)
	}
	if !strings.Contains(res.Content, fsredact.DefaultReplacement) {
		t.Fatalf("应出现替换文本: %q", res.Content)
	}
	if res.Kind != seamfs.KindText {
		t.Fatalf("Kind 不应改变: %s", res.Kind)
	}
}

// 2. 无命中内容原样返回。
func TestNoMatch(t *testing.T) {
	fake := &fakeFS{content: "hello world"}
	fsys, err := fsredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	res, _ := fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a"})
	if res.Content != "hello world" {
		t.Fatalf("无命中不应改写: %q", res.Content)
	}
}

// 3. 非法正则配置期报错。
func TestInvalidPattern(t *testing.T) {
	if _, err := fsredact.New(&fakeFS{}, []string{"("}, ""); err == nil {
		t.Fatalf("非法正则应报错")
	}
}

// 4. 写操作透传,不受影响。
func TestPassthrough(t *testing.T) {
	fake := &fakeFS{}
	fsys, err := fsredact.New(fake, nil, "")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a", Content: []byte("x")}); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if fake.writes != 1 {
		t.Fatalf("写应透传到底层")
	}
}

// 5. 内核集成:fs-local 读到 AWS key 被脱敏。
func TestKernelIntegration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("AKIAABCDEFGHIJKLMNOP"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	for _, p := range []kernel.Plugin{seamfs.Plugin(), fsloc.Plugin(), fsredact.Plugin()} {
		if _, err := app.Plugin(p, kernel.NewConfig(map[string]any{"workspace": dir})); err != nil {
			t.Fatalf("挂载失败: %v", err)
		}
	}
	var content string
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		res, err := fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/secret.txt"})
		if err != nil {
			return nil, err
		}
		content = res.Content
		return nil, nil
	}).WithInject("fs")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if strings.Contains(content, "AKIAABCDEFGHIJKLMNOP") {
		t.Fatalf("AWS key 未脱敏: %q", content)
	}
	if !strings.Contains(content, fsredact.DefaultReplacement) {
		t.Fatalf("应出现替换文本: %q", content)
	}
}
