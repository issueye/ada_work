package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func BenchmarkCallEcho(b *testing.B) {
	art, err := compile.Build("bench", "plugin.ts", map[string]string{
		"plugin.ts": `export function echo(x: number): number { return x }`,
	}, compile.Options{})
	if err != nil {
		b.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		b.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close(context.Background()) }()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Call(ctx, "echo", i); err != nil {
			b.Fatalf("Call 失败: %v", err)
		}
	}
}

func BenchmarkStreamPipeline(b *testing.B) {
	art, err := compile.Build("bench-stream", "plugin.ts", map[string]string{
		"plugin.ts": `
import { Readable, Writable, pipeline } from "node:stream";
export function run(): Promise<void> {
  return new Promise((resolve, reject) => {
    const sink = new Writable({ write(chunk: any, enc: string, cb: () => void) { cb(); } });
    pipeline(Readable.from(["a", "b", "c"], { objectMode: true }), sink, (err: any) => err ? reject(err) : resolve());
  });
}
`,
	}, compile.Options{})
	if err != nil {
		b.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		b.Fatalf("装配失败: %v", err)
	}
	defer func() { _ = rt.Close(context.Background()) }()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rt.Call(ctx, "run"); err != nil {
			b.Fatalf("Call 失败: %v", err)
		}
	}
}
