package bridge_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/jsrt"
)

// TestNodeZlib 验证 node:zlib 的同步压缩/解压真的走宿主实现:
// 压缩结果必须能被 Go 的 gzip 解开,解压结果必须与原文一致。
func TestNodeZlib(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	script := `import zlib from "node:zlib"

type Ctx = { provide: (name: string, value: unknown) => void }

export function apply(ctx: Ctx) {
  const text = "ada:zlib 内置库 —— gzip/deflate/raw"
  const gz = zlib.gzipSync(text)
  const df = zlib.deflateSync(text)
  const raw = zlib.deflateRawSync(text)
  ctx.provide("zlib.probe", {
    gzipBack: zlib.gunzipSync(gz).toString("utf8"),
    deflateBack: zlib.inflateSync(df).toString("utf8"),
    rawBack: zlib.inflateRawSync(raw).toString("utf8"),
    unzipBack: zlib.unzipSync(gz).toString("utf8"),
    gzipB64: gz.toString("base64"),
    gzipLen: gz.length,
  })
}`

	art := buildArtifact(t, map[string]string{"plugin.ts": script})
	p, err := bridge.New(bridge.Options{Name: "zlib-probe", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("zlib.probe")
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(*jsrt.Service).Data()

	const want = "ada:zlib 内置库 —— gzip/deflate/raw"
	for _, key := range []string{"gzipBack", "deflateBack", "rawBack", "unzipBack"} {
		if got, _ := data[key].(string); got != want {
			t.Fatalf("%s 解压结果不对: %q", key, data[key])
		}
	}

	gzipB64, _ := data["gzipB64"].(string)
	if gzipB64 == "" {
		t.Fatalf("没有拿到 gzip 的 base64: %+v", data)
	}
	gz, err := base64.StdEncoding.DecodeString(gzipB64)
	if err != nil {
		t.Fatalf("gzip base64 非法: %v", err)
	}
	rd, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("JS 产出的字节不是合法 gzip: %v", err)
	}
	plain, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil || string(plain) != want {
		t.Fatalf("Go 解 JS 的 gzip 失败: %v %q", err, plain)
	}
	if numOf(data["gzipLen"]) <= 0 {
		t.Fatalf("gzip 长度应大于 0: %+v", data)
	}
}
