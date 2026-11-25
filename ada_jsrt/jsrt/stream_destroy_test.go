package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestStreamDestroyHooks(t *testing.T) {
	art, err := compile.Build("stream-destroy", "plugin.ts", map[string]string{
		"plugin.ts": `
import { Readable, Writable } from "node:stream";

export function readDestroy(): Promise<string> {
  let called = false;
  class R extends Readable {
    _read() {}
    _destroy(err: any, cb: (err?: any) => void) { called = true; cb(null); }
  }
  const r = new R();
  return new Promise((resolve) => {
    r.on("close", () => resolve(called ? "ok" : "no"));
    r.destroy();
  });
}

export function writeDestroy(): Promise<string> {
  let called = false;
  class W extends Writable {
    _write(chunk: any, enc: string, cb: () => void) { cb(); }
    _destroy(err: any, cb: (err?: any) => void) { called = true; cb(null); }
  }
  const w = new W();
  return new Promise((resolve) => {
    w.on("close", () => resolve(called ? "ok" : "no"));
    w.destroy();
  });
}

export function fromDestroy(): Promise<string> {
  let closed = false;
  const iterable = {
    [Symbol.asyncIterator]() {
      return {
        next() { return Promise.resolve({ value: 1, done: false }); },
        return() { closed = true; return Promise.resolve({ value: undefined, done: true }); },
      };
    },
  };
  const r = Readable.from(iterable);
  r.on("error", () => {});
  return new Promise((resolve) => {
    r.on("close", () => resolve(closed ? "ok" : "no"));
    r.destroy(new Error("stop"));
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
	for _, fn := range []string{"readDestroy", "writeDestroy", "fromDestroy"} {
		got, err := rt.Call(ctx, fn)
		if err != nil {
			t.Fatalf("%s 失败: %v", fn, err)
		}
		if got != "ok" {
			t.Fatalf("%s 期望 ok，实际 %v", fn, got)
		}
	}
}
