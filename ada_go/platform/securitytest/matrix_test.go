// package securitytest 是 M5 C13 的拒绝矩阵:
// 能力 × 目标 × 沙箱模式,并验证拒绝来源(caps / sandbox / principal)。
//
// net 矩阵随 ADR 0012 移除:网络不再有受控 seam,Node 模块直连标准库。
package securitytest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	capsgate "github.com/issueye/ada_go_v2/plugins/policy/caps-gate"
	fssandbox "github.com/issueye/ada_go_v2/plugins/policy/fs-sandbox"
	shellguard "github.com/issueye/ada_go_v2/plugins/policy/shell-guard"
	fslocal "github.com/issueye/ada_go_v2/plugins/provider/fs-local"
	sandboxnone "github.com/issueye/ada_go_v2/plugins/provider/sandbox-none"
	shelllocal "github.com/issueye/ada_go_v2/plugins/provider/shell-local"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

func principalCtx(plugin string) context.Context {
	return kernel.WithPrincipal(context.Background(), plugin)
}

func newFS(t *testing.T, mode seamsandbox.Mode, grant string) seamfs.FS {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	impl, err := fslocal.New(fslocal.Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	reg := seamsandbox.NewRegistry()
	backend, err := sandboxnone.New(mode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register("none", backend); err != nil {
		t.Fatal(err)
	}
	broker := caps.NewBroker(nil)
	if grant != "" {
		for _, g := range strings.Split(grant, ",") {
			if err := broker.Grant("p", g); err != nil {
				t.Fatal(err)
			}
		}
	}
	chain := seamfs.NewChain()
	if _, err := chain.Install(impl); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Decorate(func(inner seamfs.FS) seamfs.FS {
		return fssandbox.New(inner, reg, "none", nil, "", nil)
	}, fssandbox.Priority); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Decorate(func(inner seamfs.FS) seamfs.FS {
		return capsgate.NewFS(inner, broker)
	}, capsgate.Priority); err != nil {
		t.Fatal(err)
	}
	f, err := chain.Get()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFSMatrix(t *testing.T) {
	ctx := principalCtx("p")
	cases := []struct {
		name  string
		mode  seamsandbox.Mode
		grant string
		call  func(seamfs.FS) error
	}{
		{"read-only read allowed", seamsandbox.ModeReadOnly, "fs.read", func(f seamfs.FS) error {
			_, err := f.Read(ctx, seamfs.ReadRequest{Path: "/workspace/a.txt"})
			return err
		}},
		{"read-only write denied", seamsandbox.ModeReadOnly, "fs.read,fs.write", func(f seamfs.FS) error {
			_, err := f.Write(ctx, seamfs.WriteRequest{Path: "/workspace/b.txt", Content: []byte("x")})
			return err
		}},
		{"workspace-write allowed", seamsandbox.ModeWorkspaceWrite, "fs.write", func(f seamfs.FS) error {
			_, err := f.Write(ctx, seamfs.WriteRequest{Path: "/workspace/b.txt", Content: []byte("x")})
			return err
		}},
		{"workspace-write outside denied", seamsandbox.ModeWorkspaceWrite, "fs.write", func(f seamfs.FS) error {
			_, err := f.Write(ctx, seamfs.WriteRequest{Path: "/etc/x", Content: []byte("x")})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFS(t, tc.mode, tc.grant)
			denied := tc.name != "read-only read allowed" && tc.name != "workspace-write allowed"
			err := tc.call(f)
			if denied && err == nil {
				t.Fatalf("应拒绝")
			}
			if !denied && err != nil {
				t.Fatalf("应放行: %v", err)
			}
		})
	}
}

func TestFSNoCapDenied(t *testing.T) {
	f := newFS(t, seamsandbox.ModeDanger, "")
	if _, err := f.Read(principalCtx("p"), seamfs.ReadRequest{Path: "/workspace/a.txt"}); err == nil {
		t.Fatal("无 fs.read 能力应拒绝")
	}
	if _, err := f.Read(context.Background(), seamfs.ReadRequest{Path: "/workspace/a.txt"}); err == nil {
		t.Fatal("无 principal 应拒绝")
	}
}

// shell 矩阵:exec 能力 × 程序 × shell 门。
func TestShellMatrix(t *testing.T) {
	workspace := t.TempDir()
	inner, err := shelllocal.New(shelllocal.Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	reg := seamsandbox.NewRegistry()
	backend, err := sandboxnone.New(seamsandbox.ModeReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register("none", backend); err != nil {
		t.Fatal(err)
	}
	program, args := "echo", []string{"ok"}
	if runtime.GOOS == "windows" {
		program, args = "cmd", []string{"/c", "echo", "ok"}
	}
	newGuard := func(grantExec bool) seamshell.Shell {
		broker := caps.NewBroker(nil)
		if grantExec {
			if err := broker.Grant("p", "exec"); err != nil {
				t.Fatal(err)
			}
		}
		return shellguard.NewWithCaps(inner, reg, "none", []string{program}, false, nil, "", nil, broker)
	}

	if _, err := newGuard(false).Run(principalCtx("p"), seamshell.Command{Program: program, Args: args}); err == nil {
		t.Fatal("无 exec 能力应拒绝")
	}
	if _, err := newGuard(true).Run(context.Background(), seamshell.Command{Program: program, Args: args}); err == nil {
		t.Fatal("无 principal 应拒绝")
	}
	if _, err := newGuard(true).Run(principalCtx("p"), seamshell.Command{Program: program, Args: args, Shell: true}); err == nil {
		t.Fatal("shell 模式未授权应拒绝")
	}
	if _, err := newGuard(true).Run(principalCtx("p"), seamshell.Command{Program: program, Args: args}); err != nil {
		t.Fatalf("已授权程序应放行: %v", err)
	}
}
