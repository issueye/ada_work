package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestStreamWebInterop(t *testing.T) {
	art, err := compile.Build("stream-web", "plugin.ts", map[string]string{
		"plugin.ts": `
import { Readable, Writable } from "node:stream";

export async function readToWeb(): Promise<string> {
  const web = Readable.toWeb(Readable.from(["a", "b"], { objectMode: true }));
  let out = "";
  for await (const chunk of web) out += String(chunk);
  return out;
}

export async function readFromWeb(): Promise<string> {
  const web = Readable.toWeb(Readable.from(["x", "y"], { objectMode: true }));
  const node = Readable.fromWeb(web);
  let out = "";
  for await (const chunk of node) out += String(chunk);
  return out;
}

export async function writeToWeb(): Promise<string> {
  let out = "";
  const node = new Writable({ write(chunk: any, enc: string, cb: () => void) { out += String(chunk); cb(); } });
  const writer = Writable.toWeb(node).getWriter();
  await writer.write("a");
  await writer.write("b");
  await writer.close();
  return out;
}

export async function writeFromWeb(): Promise<string> {
  let out = "";
  const node = new Writable({ write(chunk: any, enc: string, cb: () => void) { out += String(chunk); cb(); } });
  const web = Writable.toWeb(node);
  const from = Writable.fromWeb(web);
  await new Promise<void>((resolve, reject) => {
    from.on("finish", () => resolve());
    from.on("error", reject);
    from.write("a");
    from.write("b");
    from.end();
  });
  return out;
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
	cases := map[string]string{
		"readToWeb":    "ab",
		"readFromWeb":  "xy",
		"writeToWeb":   "ab",
		"writeFromWeb": "ab",
	}
	for fn, want := range cases {
		got, err := rt.Call(ctx, fn)
		if err != nil {
			t.Fatalf("%s 失败: %v", fn, err)
		}
		if got != want {
			t.Fatalf("%s 期望 %q，实际 %v", fn, want, got)
		}
	}
}
