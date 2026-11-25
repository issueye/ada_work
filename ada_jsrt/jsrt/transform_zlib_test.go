package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestTransformFlushAndZlibStreams(t *testing.T) {
	art, err := compile.Build("transform-zlib", "plugin.ts", map[string]string{
		"plugin.ts": `
import { Readable, Transform, pipeline } from "node:stream";
import zlib from "node:zlib";
import { Buffer } from "node:buffer";

class Upper extends Transform {
  _transform(chunk: any, enc: string, cb: (err?: any, out?: any) => void) {
    cb(null, String(chunk).toUpperCase());
  }
  _flush(cb: (err?: any, out?: any) => void) {
    this.push("!");
    cb();
  }
}

export function flushTail(): Promise<string> {
  return new Promise((resolve, reject) => {
    const t = new Upper();
    let out = "";
    t.on("data", (chunk: any) => { out += String(chunk); });
    t.on("end", () => resolve(out));
    t.on("error", reject);
    t.write("a");
    t.end("b");
  });
}

export function zlibRoundTrip(): Promise<string> {
  return new Promise((resolve, reject) => {
    const gzip = zlib.createGzip();
    const gunzip = zlib.createGunzip();
    const chunks: any[] = [];
    gunzip.on("data", (chunk: any) => chunks.push(Buffer.from(chunk)));
    gunzip.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    pipeline(
      Readable.from([Buffer.from("hello "), Buffer.from("world")], { objectMode: false }),
      gzip,
      gunzip,
      (err: any) => { if (err) reject(err); },
    );
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

	got, err := rt.Call(ctx, "flushTail")
	if err != nil || got != "AB!" {
		t.Fatalf("Transform._flush 结果不符: got=%v err=%v", got, err)
	}
	got, err = rt.Call(ctx, "zlibRoundTrip")
	if err != nil || got != "hello world" {
		t.Fatalf("zlib 流 roundtrip 不符: got=%v err=%v", got, err)
	}
}
