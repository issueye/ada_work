package jsrt_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// node:crypto 的契约测试:算法向量、编码、HMAC、PBKDF2、随机与定长比较。

func TestNodeCryptoDigests(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import crypto from "node:crypto"

export function digests(): any {
  return {
    md5: crypto.createHash("md5").update("abc").digest("hex"),
    sha1: crypto.createHash("sha1").update("abc").digest("hex"),
    // 大小写与连字符都要归一:"SHA-256" 就是 sha256。
    sha256: crypto.createHash("SHA-256").update("abc").digest("hex"),
    chained: crypto.createHash("sha256").update("a").update("b").update("c").digest("hex"),
    base64: crypto.createHash("sha256").update("abc").digest("base64"),
    sha384Len: crypto.createHash("sha384").update("abc").digest("hex").length,
    sha512Len: crypto.createHash("sha512").update("abc").digest("hex").length,
    defaultIsBuffer: Array.isArray(crypto.createHash("sha256").update("abc").digest()) === false,
    hashes: crypto.getHashes().join(","),
  }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "digests")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	want := map[string]string{
		"md5":    "900150983cd24fb0d6963f7d28e17f72",
		"sha1":   "a9993e364706816aba3e25717850c26c9cd0d89d",
		"sha256": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for key, value := range want {
		if s, _ := obj[key].(string); s != value {
			t.Fatalf("%s 向量不对: %q(期望 %q)", key, s, value)
		}
	}
	if s, _ := obj["chained"].(string); s != want["sha256"] {
		t.Fatalf("链式 update 应等价于一次 update: %q", s)
	}
	if s, _ := obj["base64"].(string); s != "ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=" {
		t.Fatalf("base64 编码不对: %q", s)
	}
	if n := asInt(t, obj["sha384Len"]); n != 96 {
		t.Fatalf("sha384 应是 96 位十六进制,实际 %v", n)
	}
	if n := asInt(t, obj["sha512Len"]); n != 128 {
		t.Fatalf("sha512 应是 128 位十六进制,实际 %v", n)
	}
	if b, _ := obj["defaultIsBuffer"].(bool); !b {
		t.Fatalf("digest() 不带编码时应返回 Buffer/Uint8Array")
	}
	if s, _ := obj["hashes"].(string); !strings.Contains(s, "sha256") || !strings.Contains(s, "sha512") {
		t.Fatalf("getHashes 缺少算法: %q", s)
	}
}

func TestNodeCryptoHMACAndPBKDF2(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import crypto from "node:crypto"

export function hmacHex(): string {
  return crypto.createHmac("sha256", "Jefe").update("what do ya want for nothing?").digest("hex")
}

export function pbkdf2SyncHex(): string {
  return crypto.pbkdf2Sync("password", "salt", 1, 20, "sha1").toString("hex")
}

export async function pbkdf2AsyncHex(): Promise<string> {
  return await new Promise<string>((resolve, reject) => {
    crypto.pbkdf2("password", "salt", 1, 20, "sha1", (err: any, key: any) => {
      if (err) reject(err)
      else resolve(key.toString("hex"))
    })
  })
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "hmacHex")
	if err != nil {
		t.Fatalf("HMAC 调用失败: %v", err)
	}
	if s, _ := got.(string); s != "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843" {
		t.Fatalf("HMAC-SHA256 向量不对: %q", s)
	}

	got, err = rt.Call(context.Background(), "pbkdf2SyncHex")
	if err != nil {
		t.Fatalf("PBKDF2 同步调用失败: %v", err)
	}
	const want = "0c60c80f961f0e71f3a9b524af6012062fe037a6"
	if s, _ := got.(string); s != want {
		t.Fatalf("PBKDF2 向量不对: %q(期望 %q)", s, want)
	}

	got, err = rt.Call(context.Background(), "pbkdf2AsyncHex")
	if err != nil {
		t.Fatalf("PBKDF2 异步调用失败: %v", err)
	}
	if s, _ := got.(string); s != want {
		t.Fatalf("PBKDF2 异步结果应与同步一致: %q", s)
	}
}

func TestNodeCryptoRandomAndCompare(t *testing.T) {
	rt := newRuntime(t, map[string]string{
		"plugin.ts": `
import crypto from "node:crypto"

export function probe(): any {
  const bytes = crypto.randomBytes(16)
  const target = new Uint8Array(8)
  crypto.randomFillSync(target)
  let mismatch = "no-error"
  try {
    crypto.timingSafeEqual(new Uint8Array([97, 98, 99]), new Uint8Array([97, 98]))
  } catch (e: any) {
    mismatch = e.code
  }
  return {
    len: bytes.length,
    filled: target.length,
    n: crypto.randomInt(5, 10),
    uuid: crypto.randomUUID(),
    same: crypto.timingSafeEqual(new Uint8Array([97, 98, 99]), new Uint8Array([97, 98, 99])),
    diff: crypto.timingSafeEqual(new Uint8Array([97, 98, 99]), new Uint8Array([97, 98, 100])),
    mismatch,
  }
}`,
	}, jsrt.Options{})

	got, err := rt.Call(context.Background(), "probe")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	obj, _ := got.(map[string]any)
	if n := asInt(t, obj["len"]); n != 16 {
		t.Fatalf("randomBytes(16) 长度不对: %v", n)
	}
	if n := asInt(t, obj["filled"]); n != 8 {
		t.Fatalf("randomFillSync 长度不对: %v", n)
	}
	if n := asInt(t, obj["n"]); n < 5 || n >= 10 {
		t.Fatalf("randomInt(5,10) 越界: %v", n)
	}
	if s, _ := obj["uuid"].(string); !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(s) {
		t.Fatalf("randomUUID 形状不对: %q", s)
	}
	if b, _ := obj["same"].(bool); !b {
		t.Fatalf("相同输入应返回 true")
	}
	if b, _ := obj["diff"].(bool); b {
		t.Fatalf("不同输入应返回 false")
	}
	if s, _ := obj["mismatch"].(string); s != "ERR_CRYPTO_TIMING_SAFE_EQUAL_LENGTH" {
		t.Fatalf("长度不同的错误码不对: %q", s)
	}
}
