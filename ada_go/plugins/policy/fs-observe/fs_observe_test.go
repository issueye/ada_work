package fsobserve_test

import (
	"context"
	"errors"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	capsgate "github.com/issueye/ada_go_v2/plugins/policy/caps-gate"
	fsobserve "github.com/issueye/ada_go_v2/plugins/policy/fs-observe"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

type fakeFS struct {
	content string
	readErr error
	writes  int
}

func (f *fakeFS) Stat(context.Context, seamfs.Path) (seamfs.FileInfo, error) {
	return seamfs.FileInfo{}, nil
}
func (f *fakeFS) List(context.Context, seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return nil, nil
}
func (f *fakeFS) Read(context.Context, seamfs.ReadRequest) (seamfs.ReadResult, error) {
	if f.readErr != nil {
		return seamfs.ReadResult{}, f.readErr
	}
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

// 1. 读/写各产生一条记录,含插件身份与字节数。
func TestRecordsReadAndWrite(t *testing.T) {
	fake := &fakeFS{content: "hello"}
	var got []fsobserve.Record
	emit := func(_ context.Context, rec fsobserve.Record) { got = append(got, rec) }
	fsys := fsobserve.New(fake, emit, nil)
	ctx := kernel.WithPrincipal(context.Background(), "p1")

	if _, err := fsys.Read(ctx, seamfs.ReadRequest{Path: "/workspace/a"}); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if _, err := fsys.Write(ctx, seamfs.WriteRequest{Path: "/workspace/b", Content: []byte("xy")}); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条记录,实际 %d", len(got))
	}
	if got[0].Op != "read" || got[0].Path != "/workspace/a" || got[0].Bytes != 5 || got[0].Plugin != "p1" {
		t.Fatalf("读记录不正确: %+v", got[0])
	}
	if got[1].Op != "write" || got[1].Bytes != 2 {
		t.Fatalf("写记录不正确: %+v", got[1])
	}
}

// 2. 失败操作也记录(error 字段)。
func TestRecordsError(t *testing.T) {
	fake := &fakeFS{readErr: errors.New("boom")}
	var rec fsobserve.Record
	fsys := fsobserve.New(fake, func(_ context.Context, r fsobserve.Record) { rec = r }, nil)
	if _, err := fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("应返回错误")
	}
	if rec.Op != "read" || rec.Err != "boom" {
		t.Fatalf("失败记录不正确: %+v", rec)
	}
}

// 3. skipOps 跳过指定操作。
func TestSkipOps(t *testing.T) {
	fake := &fakeFS{content: "x"}
	var ops []string
	fsys := fsobserve.New(fake, func(_ context.Context, r fsobserve.Record) { ops = append(ops, r.Op) }, []string{"read"})
	_, _ = fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a"})
	_, _ = fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/b", Content: nil})
	if len(ops) != 1 || ops[0] != "write" {
		t.Fatalf("read 应被跳过: %v", ops)
	}
}

func mount(t *testing.T, app *kernel.App, name string, p kernel.Plugin, cfg map[string]any) {
	t.Helper()
	if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
		t.Fatalf("挂载 %s 失败: %v", name, err)
	}
}

// 4. 内核集成:消费者读文件 → fs/observe 事件含调用者身份。
func TestKernelIntegration(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	dir := t.TempDir()

	mount(t, app, "seam/fs", seamfs.Plugin(), nil)
	mount(t, app, "fs-local", fslocal.Plugin(), map[string]any{"workspace": dir})
	mount(t, app, "fs-observe", fsobserve.Plugin(), nil)

	var payload map[string]any
	mount(t, app, "listener", kernel.Func("listener", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		ctx.On(seamfs.EventObserve, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
			if len(args) > 0 {
				if m, ok := args[0].(map[string]any); ok {
					payload = m
				}
			}
			return nil, nil
		})
		return nil, nil
	}), nil)

	mount(t, app, "consumer", kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		_, err = fsys.Read(ctx.Context(), seamfs.ReadRequest{Path: "/workspace/a.txt"})
		return nil, err
	}).WithInject("fs"), nil)

	if payload == nil || payload["op"] != "read" || payload["plugin"] != "consumer" {
		t.Fatalf("读事件不正确: %+v", payload)
	}
}

// 5. 被 caps-gate 拒绝的写也会被观察记录(观察在最外层)。
func TestObserveDeniedCalls(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	dir := t.TempDir()

	mount(t, app, "seam/fs", seamfs.Plugin(), nil)
	mount(t, app, "fs-local", fslocal.Plugin(), map[string]any{"workspace": dir})
	mount(t, app, "caps-gate", capsgate.Plugin(), map[string]any{"grants": map[string]any{}})
	mount(t, app, "fs-observe", fsobserve.Plugin(), nil)

	var payload map[string]any
	mount(t, app, "listener", kernel.Func("listener", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		ctx.On(seamfs.EventObserve, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
			if len(args) > 0 {
				if m, ok := args[0].(map[string]any); ok {
					payload = m
				}
			}
			return nil, nil
		})
		return nil, nil
	}), nil)

	mount(t, app, "consumer", kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		_, _ = fsys.Write(ctx.Context(), seamfs.WriteRequest{Path: "/workspace/denied.txt"})
		return nil, nil
	}).WithInject("fs"), nil)

	if payload == nil || payload["op"] != "write" {
		t.Fatalf("拒绝的写应被观察: %+v", payload)
	}
	if payload["error"] == nil || payload["plugin"] != "consumer" {
		t.Fatalf("记录应含错误与身份: %+v", payload)
	}
}
