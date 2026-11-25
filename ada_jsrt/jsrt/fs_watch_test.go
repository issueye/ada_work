package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/jsrt"
)

func TestFSWatchPolling(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "w.txt")
	if err := os.WriteFile(file, []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := newResident(t, `
import fs from "node:fs";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    let watcher: any;
    try {
      watcher = fs.watch("/workspace/w.txt", { interval: 20 }, (event: any, name: any) => {
        watcher.close();
        resolve(String(event) + ":" + String(name));
      });
    } catch (err) {
      reject(err);
      return;
    }
    setTimeout(() => fs.writeFileSync("/workspace/w.txt", "changed"), 40);
  });
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadWrite,
			Allow:   []string{"/workspace/**"},
			Watch:   true,
		}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := rt.Call(ctx, "run")
	if err != nil {
		t.Fatalf("fs.watch 失败: %v", err)
	}
	if s, _ := got.(string); !strings.HasPrefix(s, "change:w.txt") {
		t.Fatalf("watch 事件不符: %v", got)
	}
}

func TestFSWatchPermissionDenied(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "w.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newResident(t, `
import fs from "node:fs";
export function run(): string {
  try {
    const w = fs.watch("/workspace/w.txt");
    w.close();
    return "ok";
  } catch (err: any) {
    return String((err && err.code) || err);
  }
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadWrite,
			Allow:   []string{"/workspace/**"},
			Watch:   false,
		}},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if got != "ERR_ACCESS_DENIED" {
		t.Fatalf("未授权 watch 应返回 ERR_ACCESS_DENIED，实际 %v", got)
	}
}

func TestFSWatchFilePolling(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "w.txt"), []byte("initial"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := newResident(t, `
import fs from "node:fs";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    let called = false;
    fs.watchFile("/workspace/w.txt", { interval: 20 }, (curr: any, prev: any) => {
      if (called) return;
      called = true;
      try { fs.unwatchFile("/workspace/w.txt"); } catch (_) {}
      resolve(String(curr.size) + "/" + String(prev.size));
    });
    setTimeout(() => fs.writeFileSync("/workspace/w.txt", "changed!"), 40);
  });
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadWrite,
			Allow:   []string{"/workspace/**"},
			Watch:   true,
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := rt.Call(ctx, "run")
	if err != nil {
		t.Fatalf("fs.watchFile 失败: %v", err)
	}
	if got != "8/7" {
		t.Fatalf("watchFile curr/prev 不符: %v", got)
	}
}
