package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestZlibStreamingIncremental(t *testing.T) {
	art, err := compile.Build("zlib-stream", "plugin.ts", map[string]string{
		"plugin.ts": `
import zlib from "node:zlib";
import { Buffer } from "node:buffer";

export function compressBeforeEnd(): Promise<string> {
  return new Promise((resolve) => {
    const gzip = zlib.createGzip();
    let got = false;
    gzip.on("data", () => { got = true; });
    gzip.write(Buffer.alloc(64 * 1024, 7));
    setTimeout(() => {
      gzip.end();
      resolve(got ? "ok" : "no");
    }, 20);
  });
}

export function decompressChunks(): Promise<string> {
  const raw = Buffer.alloc(64 * 1024, 9);
  const compressed = zlib.gzipSync(raw);
  const gunzip = zlib.createGunzip();
  let total = 0;
  gunzip.on("data", (chunk: any) => { total += chunk.length; });
  return new Promise((resolve, reject) => {
    gunzip.on("end", () => resolve(String(total)));
    gunzip.on("error", reject);
    gunzip.write(compressed.subarray(0, Math.floor(compressed.length / 2)));
    gunzip.write(compressed.subarray(Math.floor(compressed.length / 2)));
    gunzip.end();
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	got, err := rt.Call(ctx, "compressBeforeEnd")
	if err != nil || got != "ok" {
		t.Fatalf("gzip 未在 end 前产生输出: got=%v err=%v", got, err)
	}
	got, err = rt.Call(ctx, "decompressChunks")
	if err != nil || got != "65536" {
		t.Fatalf("分块 gunzip 结果不符: got=%v err=%v", got, err)
	}
}
