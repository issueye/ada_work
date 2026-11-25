package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func permissionArtifact(t *testing.T) *compile.Artifact {
	t.Helper()
	art, err := compile.Build("perm", "plugin.ts", map[string]string{
		"plugin.ts": `
import fs from "node:fs";

export function read(p: string): string {
  return fs.readFileSync(p, "utf8");
}
export function write(p: string): string {
  try { fs.writeFileSync(p, "x"); return "ok" } catch (e: any) { return String(e.code) }
}
export function mkdtemp(p: string): string {
  try { return fs.mkdtempSync(p) } catch (e: any) { return String(e.code) }
}
export function openRead(p: string): string {
  try { const fd = fs.openSync(p, "r"); fs.closeSync(fd); return "ok" } catch (e: any) { return String(e.code) }
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	return art
}

func preparePermRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret", "s.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFSReadOnlyPermissionEndToEnd(t *testing.T) {
	root := preparePermRoot(t)
	var mu sync.Mutex
	var events []jsrt.Event
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: permissionArtifact(t),
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadOnly,
			Allow:   []string{"/workspace/**"},
			Deny:    []string{"/workspace/secret/**"},
		}},
		OnEvent: func(ev jsrt.Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	ctx := context.Background()
	got, err := rt.Call(ctx, "read", "/workspace/a.txt")
	if err != nil || got != "hello" {
		t.Fatalf("允许的读失败: got=%v err=%v", got, err)
	}
	if _, err := rt.Call(ctx, "read", "/workspace/secret/s.txt"); err == nil {
		t.Fatal("Deny 路径应被拒绝")
	} else if !strings.Contains(err.Error(), "EACCES") {
		t.Fatalf("期望 EACCES，实际 %v", err)
	}
	if code, err := rt.Call(ctx, "write", "/workspace/a.txt"); err != nil || code != "EROFS" {
		t.Fatalf("只读写应返回 EROFS: code=%v err=%v", code, err)
	}
	if code, err := rt.Call(ctx, "mkdtemp", "/workspace/tmp-"); err != nil || code != "EROFS" {
		t.Fatalf("只读 mkdtemp 应返回 EROFS: code=%v err=%v", code, err)
	}
	if _, err := rt.Call(ctx, "read", "/etc/passwd"); err == nil {
		t.Fatal("Mount 外路径应被拒绝")
	}

	view := rt.Permission()
	if !view.Has("fs.read", "/workspace/a.txt") {
		t.Fatal("fs.read 应允许")
	}
	if view.Has("fs.read", "/workspace/secret/s.txt") || view.Has("fs.write", "/workspace/a.txt") {
		t.Fatal("权限视图不应允许 deny 读/只读写")
	}

	mu.Lock()
	n := len(events)
	mu.Unlock()
	if n == 0 {
		t.Fatal("权限拒绝应产生 OnEvent")
	}
}

func TestFSSymlinkCannotBypassDeny(t *testing.T) {
	root := preparePermRoot(t)
	if err := os.Symlink("secret", filepath.Join(root, "linkdir")); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "o.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outlink")); err != nil {
		t.Skipf("当前环境无法创建符号链接: %v", err)
	}

	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: permissionArtifact(t),
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadOnly,
			Allow:   []string{"/workspace/**"},
			Deny:    []string{"/workspace/secret/**"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	if code, err := rt.Call(ctx, "openRead", "/workspace/linkdir/s.txt"); err != nil || code != "EACCES" {
		t.Fatalf("链接到 Deny 目录应 EACCES: code=%v err=%v", code, err)
	}
	if code, err := rt.Call(ctx, "openRead", "/workspace/outlink/o.txt"); err != nil || code != "EACCES" {
		t.Fatalf("链接到根外应 EACCES: code=%v err=%v", code, err)
	}
}

func TestFSWriteAllowPermissionEndToEnd(t *testing.T) {
	root := preparePermRoot(t)
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: permissionArtifact(t),
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled:    true,
			Root:       root,
			Access:     jsrt.FSReadWrite,
			Allow:      []string{"/workspace/**"},
			WriteAllow: []string{"/workspace/out/**"},
			Deny:       []string{"/workspace/out/locked/**"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	if code, err := rt.Call(ctx, "write", "/workspace/out/a.txt"); err != nil || code != "ok" {
		t.Fatalf("WriteAllow 路径应允许写: code=%v err=%v", code, err)
	}
	if code, err := rt.Call(ctx, "write", "/workspace/src/a.txt"); err != nil || code != "EACCES" {
		t.Fatalf("WriteAllow 外应 EACCES: code=%v err=%v", code, err)
	}
	if code, err := rt.Call(ctx, "write", "/workspace/out/locked/a.txt"); err != nil || code != "EACCES" {
		t.Fatalf("Deny 应优先: code=%v err=%v", code, err)
	}
	if dir, err := rt.Call(ctx, "mkdtemp", "/workspace/out/tmp-"); err != nil {
		t.Fatalf("fsx 允许的 mkdtemp 失败: %v", err)
	} else if s, _ := dir.(string); !strings.HasPrefix(s, "/workspace/out/tmp-") {
		t.Fatalf("mkdtemp 返回路径不符: %v", dir)
	}
	if code, err := rt.Call(ctx, "mkdtemp", "/workspace/src/tmp-"); err != nil || code != "EACCES" {
		t.Fatalf("fsx 越权 mkdtemp 应 EACCES: code=%v err=%v", code, err)
	}
}
