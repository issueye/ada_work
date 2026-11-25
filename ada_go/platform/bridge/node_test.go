package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/jsrt"
)

// TestNodeBuiltins 用真实 fs-local 验证 node:fs / node:path / node:os / node:buffer。
func TestNodeBuiltins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello node"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	// 先装 fs seam 与本地实现,再挂 JS 插件;node:fs 的根从 Chain.Root 取。
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})
	if _, err := app.Root().Service("fs"); err != nil {
		t.Fatal(err)
	}

	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import fs from "node:fs"
import fsp from "node:fs/promises"
import path from "node:path"
import os from "node:os"
import { EventEmitter } from "node:events"
import { format } from "node:util"
import assert from "node:assert"
import { parse } from "node:querystring"
import { StringDecoder } from "node:string_decoder"
import { fileURLToPath } from "node:url"
import process from "node:process"
import { Buffer } from "node:buffer"

type Ctx = { provide: (name: string, value: unknown) => void }

export function apply(ctx: Ctx) {
  fs.writeFileSync("b.txt", "written")
  const ee = new EventEmitter()
  let emitted = ""
  ee.on("ping", (v: string) => { emitted = v })
  ee.emit("ping", "pong")
  assert.strictEqual(1, 1)
  const dec = new StringDecoder("utf8")
  const decoded = dec.write(Buffer.from("hi"))
  ctx.provide("node.probe", {
    text: fs.readFileSync("a.txt", "utf8"),
    exists: fs.existsSync("b.txt"),
    joined: path.join("/workspace", "a.txt"),
    platform: os.platform(),
    tmpdir: os.tmpdir(),
    hex: Buffer.from("hi").toString("hex"),
    hasPromise: typeof fsp.readFile === "function",
    emitted,
    formatted: format("%s-%d", "n", 7),
    hasQuerystring: typeof parse === "function",
    decoded,
    hasURL: typeof fileURLToPath === "function",
    cwd: process.cwd(),
    procPlatform: process.platform,
  })
}`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "node-plugin", Artifact: art,
		Inject: kernel.Inject{Names: []string{"fs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("node.probe")
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := raw.(*jsrt.Service)
	if !ok {
		t.Fatalf("服务类型不对: %T", raw)
	}
	data := svc.Data()
	if data["text"] != "hello node" || data["exists"] != true {
		t.Fatalf("node:fs 结果不正确: %+v", data)
	}
	if data["joined"] != "/workspace/a.txt" {
		t.Fatalf("node:path 结果不正确: %+v", data["joined"])
	}
	if data["platform"] == "" {
		t.Fatalf("node:os.platform 为空: %+v", data)
	}
	if data["tmpdir"] != "/workspace/.ada-tmp" {
		t.Fatalf("node:os.tmpdir 不正确: %+v", data["tmpdir"])
	}
	if _, err := os.Stat(filepath.Join(dir, ".ada-tmp")); err != nil {
		t.Fatalf("os.tmpdir 目录未创建: %v", err)
	}
	if data["hex"] != "6869" {
		t.Fatalf("node:buffer 结果不正确: %+v", data["hex"])
	}
	if data["hasPromise"] != true {
		t.Fatalf("node:fs/promises 不可用: %+v", data)
	}
	if data["emitted"] != "pong" || data["formatted"] != "n-7" {
		t.Fatalf("node:events/util 结果不正确: %+v", data)
	}
	if data["decoded"] != "hi" || data["hasQuerystring"] != true || data["hasURL"] != true {
		t.Fatalf("node:string_decoder/querystring/url 结果不正确: %+v", data)
	}
	if data["cwd"] != "/workspace" || data["procPlatform"] != data["platform"] {
		t.Fatalf("node:process 结果不正确: %+v", data)
	}
	written, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil || string(written) != "written" {
		t.Fatalf("node:fs 写入失败: %v %q", err, written)
	}

}

// TestNodeFSDisabledByPolicy 钉住安全边界:fs 链有策略装饰器时,node:fs 必须不可用,
// 否则它会成为绕过 fs-sandbox/caps-gate 的第二条路径。
func TestNodeFSDisabledByPolicy(t *testing.T) {
	dir := t.TempDir()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-fs", nil)
	mountBuiltin(t, app, "fs-local", map[string]any{"workspace": dir})
	mountBuiltin(t, app, "seam-sandbox", nil)
	mountBuiltin(t, app, "sandbox-none", map[string]any{"mode": "read-only"})
	mountBuiltin(t, app, "fs-sandbox", map[string]any{"backend": "sandbox-none"})

	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import fs from "node:fs"
export function apply() { fs.readFileSync("a.txt") }`,
	})
	p, err := bridge.New(bridge.Options{
		Name: "node-fs-policy", Artifact: art,
		Inject: kernel.Inject{Names: []string{"fs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "no file system") {
		t.Fatalf("有 fs 策略时 node:fs 应不可用,实际 %v", err)
	}
}
