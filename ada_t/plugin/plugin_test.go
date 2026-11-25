package plugin

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validManifest() *Manifest {
	return &Manifest{
		ID:       "demo",
		Name:     "Demo",
		Version:  "0.1.0",
		Entry:    "Demo",
		Deps:     []string{"base"},
		Capabilities: []string{"host.bots.list", "host.*.append"},
		Slots: []Contribution{
			{Slot: SlotSidebarItem, Component: "DemoSidebar", Order: 10, Width: 220},
			{Slot: SlotComposerAction, Component: "DemoAction"},
		},
	}
}

func TestValidateOK(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("最小合法清单被拒绝: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := map[string]func(m *Manifest){
		"空 id":      func(m *Manifest) { m.ID = "" },
		"非法 id":     func(m *Manifest) { m.ID = "a/b" },
		"缺 version":  func(m *Manifest) { m.Version = " " },
		"缺 entry":    func(m *Manifest) { m.Entry = "" },
		"依赖自己":      func(m *Manifest) { m.Deps = []string{"demo"} },
		"重复依赖":      func(m *Manifest) { m.Deps = []string{"a", "a"} },
		"能力含空格":     func(m *Manifest) { m.Capabilities = []string{"host .x"} },
		"重复能力":      func(m *Manifest) { m.Capabilities = []string{"a", "a"} },
		"槽位缺组件":     func(m *Manifest) { m.Slots = []Contribution{{Slot: "s"}} },
		"槽位缺名字":     func(m *Manifest) { m.Slots = []Contribution{{Component: "c"}} },
		"槽位重复":      func(m *Manifest) { m.Slots = []Contribution{{Slot: "s", Component: "c"}, {Slot: "s", Component: "c"}} },
		"负尺寸":       func(m *Manifest) { m.Slots = []Contribution{{Slot: "s", Component: "c", Width: -1}} },
		"非法摘要":      func(m *Manifest) { m.SHA256 = "sha256:xyz" },
	}
	for name, mutate := range cases {
		m := validManifest()
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

func TestAllows(t *testing.T) {
	m := validManifest()
	cases := map[string]bool{
		"host.bots.list":     true,
		"host.sessions.list": false,
		"host.anything":      false,
		"host.x.append":      true, // 前缀通配 host.*.append
		"user.flow.run":      false,
	}
	for cap, want := range cases {
		if got := m.Allows(cap); got != want {
			t.Errorf("Allows(%q) = %v, want %v", cap, got, want)
		}
	}
	star := &Manifest{Capabilities: []string{"*"}}
	if !star.Allows("whatever.deep") {
		t.Fatal("* 应匹配任意能力")
	}
	if star.AllowsCall("host", "x") != true {
		t.Fatal("AllowsCall 应命中 *")
	}
}

func TestParseAndLoad(t *testing.T) {
	dir := t.TempDir()
	m := validManifest()
	m.SHA256 = "sha256:" + strings.Repeat("a", 64)
	path := filepath.Join(dir, ManifestName)
	if err := os.WriteFile(path, mustJSON(t, m), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != "demo" || len(got.Slots) != 2 {
		t.Fatalf("Load = %+v", got)
	}

	// 未知字段被拒绝(防拼错)
	if _, err := Parse([]byte(`{"id":"x","version":"1","entry":"E","unknown":1}`)); err == nil {
		t.Fatal("未知字段应被拒绝")
	}
	// 非法 JSON
	if _, err := Parse([]byte(`{`)); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

func TestSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := validManifest()
	m.SHA256 = "sha256:" + strings.Repeat("b", 64)
	if err := Sign(m, priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := m.VerifySignature(pub); err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	// 摘要被改 → 验签失败
	bad := *m
	bad.SHA256 = "sha256:" + strings.Repeat("c", 64)
	if err := bad.VerifySignature(pub); err == nil {
		t.Fatal("摘要被改后验签应失败")
	}
	// 错公钥
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := m.VerifySignature(otherPub); err == nil {
		t.Fatal("错误公钥应验签失败")
	}
	// 没有签名/摘要
	if err := (&Manifest{ID: "x"}).VerifySignature(pub); err == nil {
		t.Fatal("无签名应报错")
	}
	if err := m.VerifySignature(); err == nil {
		t.Fatal("无公钥应报错")
	}
}

func TestDigestHex(t *testing.T) {
	m := validManifest()
	m.SHA256 = "sha256:deadbeef"
	if m.DigestHex() != "deadbeef" {
		t.Fatalf("DigestHex = %q", m.DigestHex())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
