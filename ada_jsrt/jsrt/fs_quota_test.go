package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

func TestFSQuotas(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), []byte("01234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, "dir", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rt := newResident(t, `
import fs from "node:fs";
import { Buffer } from "node:buffer";

export function writeBeyond(): string {
  const fd = fs.openSync("/workspace/out.bin", "w");
  try {
    fs.writeSync(fd, Buffer.from("12345"));
    return "ok";
  } catch (err: any) {
    return String((err && err.code) || err);
  } finally {
    try { fs.closeSync(fd); } catch (_) {}
  }
}

export function readBeyond(): string {
  const fd = fs.openSync("/workspace/big.bin", "r");
  try {
    fs.readSync(fd, Buffer.alloc(16), 0, 16, 0);
    return "ok";
  } catch (err: any) {
    return String((err && err.code) || err);
  } finally {
    try { fs.closeSync(fd); } catch (_) {}
  }
}

export function fdLimit(): string {
  const a = fs.openSync("/workspace/big.bin", "r");
  const b = fs.openSync("/workspace/big.bin", "r");
  try {
    const c = fs.openSync("/workspace/big.bin", "r");
    fs.closeSync(c);
    return "ok";
  } catch (err: any) {
    return String((err && err.code) || err);
  } finally {
    fs.closeSync(a);
    fs.closeSync(b);
  }
}

export function dirLimit(): string {
  const d = fs.opendirSync("/workspace/dir");
  try {
    d.readSync();
    d.readSync();
    d.readSync();
    return "ok";
  } catch (err: any) {
    return String((err && err.code) || err);
  } finally {
    try { d.closeSync(); } catch (_) {}
  }
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled:       true,
			Root:          root,
			Access:        jsrt.FSReadWrite,
			Allow:         []string{"/workspace/**"},
			MaxFileBytes:  4,
			MaxOpenFiles:  2,
			MaxDirEntries: 2,
		}},
	})
	ctx := context.Background()
	cases := []struct {
		fn   string
		want string
	}{
		{"writeBeyond", "EFBIG"},
		{"readBeyond", "EFBIG"},
		{"fdLimit", "EMFILE"},
		{"dirLimit", "EFBIG"},
	}
	for _, tc := range cases {
		got, err := rt.Call(ctx, tc.fn)
		if err != nil {
			t.Fatalf("%s 调用失败: %v", tc.fn, err)
		}
		if got != tc.want {
			t.Fatalf("%s 期望 %s，实际 %v", tc.fn, tc.want, got)
		}
	}
}

func TestCoreFSPathQuotas(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), []byte("01234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, "dir", name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt := newResident(t, `
import fs from "node:fs";
import fsp from "node:fs/promises";

export function syncRead(): string {
  try { fs.readFileSync("/workspace/big.bin"); return "ok"; }
  catch (err: any) { return String((err && err.code) || err); }
}
export function syncWrite(): string {
  try { fs.writeFileSync("/workspace/out.bin", "12345"); return "ok"; }
  catch (err: any) { return String((err && err.code) || err); }
}
export function syncReaddir(): string {
  try { fs.readdirSync("/workspace/dir"); return "ok"; }
  catch (err: any) { return String((err && err.code) || err); }
}
export async function promiseRead(): Promise<string> {
  try { await fsp.readFile("/workspace/big.bin"); return "ok"; }
  catch (err: any) { return String((err && err.code) || err); }
}
export function callbackRead(): Promise<string> {
  return new Promise((resolve) => {
    fs.readFile("/workspace/big.bin", (err: any) => resolve(err ? String(err.code || err) : "ok"));
  });
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled:       true,
			Root:          root,
			Access:        jsrt.FSReadWrite,
			Allow:         []string{"/workspace/**"},
			MaxFileBytes:  4,
			MaxDirEntries: 2,
		}},
	})
	ctx := context.Background()
	for _, name := range []string{"syncRead", "syncWrite", "syncReaddir", "promiseRead", "callbackRead"} {
		got, err := rt.Call(ctx, name)
		if err != nil {
			t.Fatalf("%s 调用失败: %v", name, err)
		}
		if got != "EFBIG" {
			t.Fatalf("%s 期望 EFBIG，实际 %v", name, got)
		}
	}
}
