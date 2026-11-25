package jsrt_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// 一条 Node 风格的综合链路：fs/promises 写入 → createReadStream → gzip → gunzip → Writable。
func TestNodeCompatE2E(t *testing.T) {
	root := t.TempDir()
	rt := newResident(t, `
import fs from "node:fs";
import fsp from "node:fs/promises";
import zlib from "node:zlib";
import { Writable } from "node:stream";
import { pipeline } from "node:stream/promises";
import { Buffer } from "node:buffer";

export async function run(): Promise<string> {
  await fsp.writeFile("/workspace/in.txt", "hello compat");
  const chunks: any[] = [];
  const sink = new Writable({
    write(chunk: any, enc: string, cb: () => void) {
      chunks.push(Buffer.from(chunk));
      cb();
    },
  });
  await pipeline(
    fs.createReadStream("/workspace/in.txt"),
    zlib.createGzip(),
    zlib.createGunzip(),
    sink,
  );
  return Buffer.concat(chunks).toString("utf8");
}
`, jsrt.Options{
		Permissions: jsrt.Permissions{FS: jsrt.FSPermission{
			Enabled: true,
			Root:    root,
			Access:  jsrt.FSReadWrite,
			Allow:   []string{"/workspace/**"},
		}},
	})

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("兼容链路失败: %v", err)
	}
	if got != "hello compat" {
		t.Fatalf("兼容链路结果不符: %v", got)
	}
	if data, err := os.ReadFile(filepath.Join(root, "in.txt")); err != nil || string(data) != "hello compat" {
		t.Fatalf("文件写入不符: %q err=%v", data, err)
	}
}
