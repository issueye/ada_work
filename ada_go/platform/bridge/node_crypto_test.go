package bridge_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/jsrt"
)

// TestNodeCrypto 验证补充进 Node 内置库的 node:crypto:sha256、randomBytes、randomUUID。
// 这些能力全部经 __adaHost 的字符串入口,跨边界不出现二进制对象。
func TestNodeCrypto(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { createHash, randomBytes, randomUUID } from "node:crypto"

type Ctx = { provide: (name: string, value: unknown) => void }

export function apply(ctx: Ctx) {
  const sum = createHash("sha256").update("hello").digest("hex")
  const b64 = createHash("sha256").update("hello").digest("base64")
  const uuid = randomUUID()
  const rnd = randomBytes(4).toString("hex")
  ctx.provide("node.crypto", { sum, b64, uuid, rnd })
}`,
	})
	p, err := bridge.New(bridge.Options{Name: "crypto-plugin", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("node.crypto")
	if err != nil {
		t.Fatal(err)
	}
	svc, ok := raw.(*jsrt.Service)
	if !ok {
		t.Fatalf("服务类型不对: %T", raw)
	}
	data := svc.Data()

	// 期望值由 Go 标准库算,避免把测试写成对实现的复述。
	want := sha256.Sum256([]byte("hello"))
	if data["sum"] != hex.EncodeToString(want[:]) {
		t.Fatalf("sha256(hex) 不正确: %v", data["sum"])
	}
	if data["b64"] != base64.StdEncoding.EncodeToString(want[:]) {
		t.Fatalf("sha256(base64) 不正确: %v", data["b64"])
	}

	uuid, _ := data["uuid"].(string)
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(uuid) {
		t.Fatalf("randomUUID 不是 v4 UUID: %q", uuid)
	}
	rnd, _ := data["rnd"].(string)
	if len(rnd) != 8 {
		t.Fatalf("randomBytes(4) 应为 8 个 hex 字符: %q", rnd)
	}
}
