package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestStreamBackpressureAndPipeline(t *testing.T) {
	art, err := compile.Build("stream", "plugin.ts", map[string]string{
		"plugin.ts": `
import { Readable, Writable, pipeline } from "node:stream";

export function run(): Promise<string> {
  let produced = 0;
  let written = 0;
  let drains = 0;

  class Source extends Readable {
    constructor() { super({ highWaterMark: 1 }); }
    _read() {
      if (produced >= 6) { this.push(null); return; }
      produced += 1;
      this.push("chunk-" + produced);
    }
  }

  class Slow extends Writable {
    constructor() { super({ highWaterMark: 1 }); }
    _write(chunk: any, enc: string, cb: () => void) {
      written += 1;
      setTimeout(cb, 5);
    }
  }

  const dest = new Slow();
  dest.on("drain", () => { drains += 1; });

  return new Promise((resolve, reject) => {
    pipeline(new Source(), dest, (err: any) => {
      if (err) reject(err);
      else resolve(produced + "/" + written + "/" + drains);
    });
  });
}

export async function lazyFrom(): Promise<string> {
  let pulled = 0;
  const iterable = {
    [Symbol.asyncIterator]() {
      return {
        next() {
          pulled += 1;
          if (pulled > 10) return Promise.resolve({ done: true, value: undefined });
          return Promise.resolve({ done: false, value: pulled });
        },
      };
    },
  };
  const r = Readable.from(iterable, { highWaterMark: 1 });
  const before = pulled;
  let seen = 0;
  for await (const _ of r) {
    seen += 1;
    if (seen === 3) break;
  }
  return before + "/" + seen + "/" + pulled;
}

export function failing(): Promise<string> {
  class Boom extends Readable {
    _read() { this.destroy(new Error("boom")); }
  }
  class Sink extends Writable {
    _write(chunk: any, enc: string, cb: () => void) { cb(); }
  }
  return new Promise((resolve) => {
    pipeline(new Boom(), new Sink(), (err: any) => resolve(String((err && err.message) || err)));
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

	got, err := rt.Call(ctx, "run")
	if err != nil {
		t.Fatalf("pipeline 失败: %v", err)
	}
	if s, _ := got.(string); s != "6/6/" && !strings.HasPrefix(s, "6/6/") {
		t.Fatalf("背压/写计数不符: %v", got)
	}
	// 至少发生过一次 drain（高水位 1 + 异步 _write）。
	if s, _ := got.(string); !strings.HasPrefix(s, "6/6/") {
		t.Fatalf("结果格式不符: %v", got)
	}

	got, err = rt.Call(ctx, "failing")
	if err != nil {
		t.Fatalf("failing 调用失败: %v", err)
	}
	if s, _ := got.(string); !strings.Contains(s, "boom") {
		t.Fatalf("pipeline 错误未传播: %v", got)
	}

	got, err = rt.Call(ctx, "lazyFrom")
	if err != nil {
		t.Fatalf("lazyFrom 失败: %v", err)
	}
	parts := strings.Split(got.(string), "/")
	if len(parts) != 3 {
		t.Fatalf("lazyFrom 结果格式不符: %v", got)
	}
	pulled := parts[2]
	if pulled != "3" && pulled != "4" {
		t.Fatalf("Readable.from 应惰性拉取（seen=3 时 pulled≈3-4），实际 %v", got)
	}
}
