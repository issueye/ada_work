package capsgate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	capsgate "github.com/issueye/ada_go_v2/plugins/policy/caps-gate"
	capsstatic "github.com/issueye/ada_go_v2/plugins/provider/caps-static"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

type fakeFS struct{ writes, reads int }

func (f *fakeFS) Stat(context.Context, seamfs.Path) (seamfs.FileInfo, error) {
	return seamfs.FileInfo{}, nil
}
func (f *fakeFS) List(context.Context, seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return nil, nil
}
func (f *fakeFS) Read(context.Context, seamfs.ReadRequest) (seamfs.ReadResult, error) {
	f.reads++
	return seamfs.ReadResult{Content: "ok"}, nil
}
func (f *fakeFS) Write(context.Context, seamfs.WriteRequest) (seamfs.WriteResult, error) {
	f.writes++
	return seamfs.WriteResult{Version: "v1"}, nil
}
func (f *fakeFS) Edit(context.Context, seamfs.EditRequest) (seamfs.EditResult, error) {
	f.writes++
	return seamfs.EditResult{Replacements: 1, Version: "v1"}, nil
}
func (f *fakeFS) Search(context.Context, seamfs.SearchRequest) (seamfs.SearchResult, error) {
	return seamfs.SearchResult{}, nil
}

type fakeShell struct{ runs int }

func (f *fakeShell) Run(context.Context, seamshell.Command) (seamshell.Result, error) {
	f.runs++
	return seamshell.Result{}, nil
}
func (f *fakeShell) Stream(_ context.Context, _ seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	f.runs++
	_ = onChunk(seamshell.Chunk{Stream: "stdout", Text: "ok", Seq: 1})
	return seamshell.Result{}, nil
}
func (f *fakeShell) Env() seamshell.Env { return nil }

func brokerWith(t *testing.T, grants map[string][]string) *caps.Broker {
	t.Helper()
	broker := caps.NewBroker(nil)
	for plugin, list := range grants {
		if err := broker.Grant(plugin, list...); err != nil {
			t.Fatalf("授予 %q 失败: %v", plugin, err)
		}
	}
	return broker
}

// 1. fs:已授权放行、未授权/无身份拒绝。
func TestFSAuthorization(t *testing.T) {
	fake := &fakeFS{}
	gate := capsgate.NewFS(fake, brokerWith(t, map[string][]string{"p1": {"fs.read"}}))

	auth := kernel.WithPrincipal(context.Background(), "p1")
	if _, err := gate.Read(auth, seamfs.ReadRequest{Path: "/workspace/a"}); err != nil {
		t.Fatalf("已授权读应放行: %v", err)
	}
	if _, err := gate.Write(auth, seamfs.WriteRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("未授权写应拒绝")
	}
	if _, err := gate.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("无身份应拒绝(context.Background() 绕过)")
	}
	if _, err := gate.Read(kernel.WithPrincipal(context.Background(), "p2"), seamfs.ReadRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("未登记插件应拒绝")
	}
	if fake.writes != 0 || fake.reads != 1 {
		t.Fatalf("底层调用次数不正确: reads=%d writes=%d", fake.reads, fake.writes)
	}
}

// 2. shell:exec 能力授权。
func TestShellAuthorization(t *testing.T) {
	fake := &fakeShell{}
	gate := capsgate.NewShell(fake, brokerWith(t, map[string][]string{"p1": {"exec"}}))
	auth := kernel.WithPrincipal(context.Background(), "p1")
	if _, err := gate.Run(auth, seamshell.Command{Program: "echo"}); err != nil {
		t.Fatalf("已授权 exec 应放行: %v", err)
	}
	if _, err := gate.Run(kernel.WithPrincipal(context.Background(), "p2"), seamshell.Command{Program: "echo"}); err == nil {
		t.Fatalf("未授权插件应拒绝")
	}
	if fake.runs != 1 {
		t.Fatalf("底层执行次数不正确: %d", fake.runs)
	}
}

// 3. 内核集成:消费者用 ctx.Context() 携带身份才能写入;无授权/无身份被拒。
func TestKernelIntegration(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mount := func(name string, p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/fs", seamfs.Plugin(), nil)
	mount("fs-local", fslocal.Plugin(), map[string]any{"workspace": dir})
	mount("caps-gate", capsgate.Plugin(), map[string]any{
		"grants": map[string][]string{"consumer": {"fs.read", "fs.write"}},
	})

	consumer := kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		if _, err := fsys.Write(ctx.Context(), seamfs.WriteRequest{Path: "/workspace/a.txt", Content: []byte("x")}); err != nil {
			return nil, err
		}
		// 用 background context 绕过 → 必须被拒绝(fail-closed)。
		if _, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/b.txt", Content: []byte("y")}); err == nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "无身份写入不应成功")
		}
		return nil, nil
	}).WithInject("fs")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("已授权写入应落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Fatalf("无身份写入不应落盘")
	}

	// 未授权插件:写入被拒。
	denied := kernel.Func("denied-consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		_, err = fsys.Write(ctx.Context(), seamfs.WriteRequest{Path: "/workspace/c.txt", Content: []byte("z")})
		return nil, err
	}).WithInject("fs")
	if _, err := app.Plugin(denied, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("未授权插件挂载应失败(写入被拒): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err == nil {
		t.Fatalf("未授权写入不应落盘")
	}
}

// 4. caps-gate 优先使用注入的 caps 服务(caps-static),忽略 config grants。
func TestServiceBrokerPreferred(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mount := func(name string, p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/fs", seamfs.Plugin(), nil)
	mount("fs-local", fslocal.Plugin(), map[string]any{"workspace": dir})
	mount("caps-static", capsstatic.Plugin(), map[string]any{
		"grants": map[string][]string{"consumer": {"fs.write"}},
	})
	// config grants 为空:如果 caps-gate 没使用服务,写会被拒绝。
	mount("caps-gate", capsgate.Plugin(), map[string]any{"grants": map[string]any{}})

	consumer := kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		_, err = fsys.Write(ctx.Context(), seamfs.WriteRequest{Path: "/workspace/a.txt", Content: []byte("x")})
		return nil, err
	}).WithInject("fs")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("消费者应能通过 caps 服务授权: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatalf("写入应落盘: %v", err)
	}
}
