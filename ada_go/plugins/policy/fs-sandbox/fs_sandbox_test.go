package fssandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	fssandbox "github.com/issueye/ada_go_v2/plugins/policy/fs-sandbox"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	sandboxnone "github.com/issueye/ada_go_v2/plugins/provider/sandbox-none"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
)

type fakeFS struct {
	writes []seamfs.Path
	reads  []seamfs.Path
}

func (f *fakeFS) Stat(context.Context, seamfs.Path) (seamfs.FileInfo, error) {
	return seamfs.FileInfo{}, nil
}
func (f *fakeFS) List(context.Context, seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return nil, nil
}
func (f *fakeFS) Read(_ context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	f.reads = append(f.reads, req.Path)
	return seamfs.ReadResult{Content: "ok"}, nil
}
func (f *fakeFS) Write(_ context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	f.writes = append(f.writes, req.Path)
	return seamfs.WriteResult{Version: "v1"}, nil
}
func (f *fakeFS) Edit(_ context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	f.writes = append(f.writes, req.Path)
	return seamfs.EditResult{Replacements: 1, Version: "v1"}, nil
}
func (f *fakeFS) Search(context.Context, seamfs.SearchRequest) (seamfs.SearchResult, error) {
	return seamfs.SearchResult{}, nil
}

// registryWith 把真实 sandbox-none 后端装进注册表,策略用与生产一致的判定。
func registryWith(t *testing.T, mode seamsandbox.Mode) *seamsandbox.Registry {
	t.Helper()
	impl, err := sandboxnone.New(mode)
	if err != nil {
		t.Fatalf("创建 sandbox-none 失败: %v", err)
	}
	registry := seamsandbox.NewRegistry()
	if _, err := registry.Register("none", impl); err != nil {
		t.Fatalf("注册后端失败: %v", err)
	}
	return registry
}

// 1. 三模式 × 写路径的判定矩阵(判定来自 sandbox 后端,不是策略里重写模式)。
func TestModeMatrix(t *testing.T) {
	fake := &fakeFS{}
	cases := []struct {
		mode    seamsandbox.Mode
		path    seamfs.Path
		allowed bool
	}{
		{seamsandbox.ModeReadOnly, "/workspace/a.txt", false},
		{seamsandbox.ModeWorkspaceWrite, "/workspace/a.txt", true},
		{seamsandbox.ModeWorkspaceWrite, "/tmp/a.txt", true},
		{seamsandbox.ModeWorkspaceWrite, "/plugin/x/a.txt", false},
		{seamsandbox.ModeDanger, "/workspace/a.txt", true},
		{seamsandbox.ModeDanger, "/plugin/x/a.txt", false},
	}
	for _, tc := range cases {
		fsys := fssandbox.New(fake, registryWith(t, tc.mode), "", nil, "", nil)
		_, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: tc.path})
		if tc.allowed && err != nil {
			t.Fatalf("%s 写 %s 应允许,实际 %v", tc.mode, tc.path, err)
		}
		if !tc.allowed && err == nil {
			t.Fatalf("%s 写 %s 应拒绝", tc.mode, tc.path)
		}
	}

	readOnly := fssandbox.New(fake, registryWith(t, seamsandbox.ModeReadOnly), "", nil, "", nil)
	if _, err := readOnly.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a.txt"}); err != nil {
		t.Fatalf("只读模式读应放行: %v", err)
	}
}

// 2. 拒绝时底层实现不会被调用。
func TestDeniedDoesNotReachInner(t *testing.T) {
	fake := &fakeFS{}
	fsys := fssandbox.New(fake, registryWith(t, seamsandbox.ModeReadOnly), "", nil, "", nil)
	if _, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/x"}); err == nil {
		t.Fatalf("应拒绝")
	}
	if len(fake.writes) != 0 {
		t.Fatalf("被拒绝的写不应到达底层,实际 %v", fake.writes)
	}
}

// 3. 沙箱后端不可用时 fail-closed。
func TestNoSandboxBackendFailsClosed(t *testing.T) {
	fake := &fakeFS{}
	fsys := fssandbox.New(fake, seamsandbox.NewRegistry(), "", nil, "", nil)
	if _, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("没有沙箱后端时写必须失败(不能放行)")
	}
}

// 4. 与内核装配:seam/sandbox + sandbox-none + seam/fs + fs-local + fs-sandbox。
func TestKernelIntegration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mount := func(name string, p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/fs", seamfs.Plugin(), nil)
	mount("seam/sandbox", seamsandbox.Plugin(), nil)
	mount("fs-local", fslocal.Plugin(), map[string]any{"workspace": dir})
	mount("sandbox-none", sandboxnone.Plugin(), map[string]any{"mode": string(seamsandbox.ModeReadOnly)})
	mount("fs-sandbox", fssandbox.Plugin(), nil)

	var (
		readContent string
		writeErr    error
	)
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		fsys, err := chain.Get()
		if err != nil {
			return nil, err
		}
		r, err := fsys.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a.txt"})
		if err != nil {
			return nil, err
		}
		readContent = r.Content
		_, writeErr = fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/new.txt", Content: []byte("x")})
		return nil, nil
	}).WithInject("fs")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}

	if readContent != "hello" {
		t.Fatalf("读内容不正确: %q", readContent)
	}
	if writeErr == nil {
		t.Fatalf("只读模式下写应被拒绝")
	}
}

// 5. 可升级的拒绝 → 审批允许则放行,审批拒绝/缺失则仍然拒绝(fail-closed)。
func TestApprovalEscalation(t *testing.T) {
	allowReg := seamapproval.NewRegistry()
	if _, err := allowReg.Register(allowApprover{}); err != nil {
		t.Fatalf("注册审批人失败: %v", err)
	}
	fake := &fakeFS{}
	fsys := fssandbox.New(fake, registryWith(t, seamsandbox.ModeReadOnly), "", allowReg, "", nil)
	if _, err := fsys.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a"}); err != nil {
		t.Fatalf("审批允许后写应放行: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("审批允许后应真正写入,实际 %d", len(fake.writes))
	}

	denyReg := seamapproval.NewRegistry()
	if _, err := denyReg.Register(denyApprover{}); err != nil {
		t.Fatalf("注册审批人失败: %v", err)
	}
	fake = &fakeFS{}
	denied := fssandbox.New(fake, registryWith(t, seamsandbox.ModeReadOnly), "", denyReg, "", nil)
	if _, err := denied.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("审批拒绝后写必须失败")
	}
	if len(fake.writes) != 0 {
		t.Fatalf("审批拒绝后不应触底写入")
	}

	// 没有审批服务:可升级的拒绝也必须 fail-closed。
	fake = &fakeFS{}
	noApproval := fssandbox.New(fake, registryWith(t, seamsandbox.ModeReadOnly), "", nil, "", nil)
	if _, err := noApproval.Write(context.Background(), seamfs.WriteRequest{Path: "/workspace/a"}); err == nil {
		t.Fatalf("没有审批服务时必须拒绝")
	}
}

type allowApprover struct{}

func (allowApprover) Name() string { return "allow" }
func (allowApprover) Approve(context.Context, seamapproval.Request) (seamapproval.Decision, error) {
	return seamapproval.Decision{Allow: true, Reason: "允许本次升级", Approver: "allow"}, nil
}

type denyApprover struct{}

func (denyApprover) Name() string { return "deny" }
func (denyApprover) Approve(context.Context, seamapproval.Request) (seamapproval.Decision, error) {
	return seamapproval.Decision{Allow: false, Reason: "审批人拒绝", Approver: "deny"}, nil
}
