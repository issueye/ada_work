package jsrt

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
)

func testArtifact(t *testing.T) *compile.Artifact {
	t.Helper()
	art, err := compile.Build("test", "plugin.ts", map[string]string{
		"plugin.ts": `export function ping(): string { return "pong" }`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	return art
}

func mustFSPolicy(t *testing.T, p FSPermission) *fsPolicy {
	t.Helper()
	pol, err := newFSPolicy(p)
	if err != nil {
		t.Fatalf("构建文件策略失败: %v", err)
	}
	if pol == nil {
		t.Fatal("策略不应为 nil")
	}
	return pol
}

func mustNetPolicy(t *testing.T, p NetPermission) *netPolicy {
	t.Helper()
	pol, err := newNetPolicy(p)
	if err != nil {
		t.Fatalf("构建网络策略失败: %v", err)
	}
	if pol == nil {
		t.Fatal("策略不应为 nil")
	}
	return pol
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("期望 PermissionError，实际 %v", err)
	}
	if pe.Code != code {
		t.Fatalf("错误码期望 %s，实际 %s(%v)", code, pe.Code, err)
	}
}

func TestFSPolicyReadAllowDeny(t *testing.T) {
	pol := mustFSPolicy(t, FSPermission{
		Enabled: true,
		Root:    t.TempDir(),
		Access:  FSReadOnly,
		Allow:   []string{"src/**", "/workspace/docs/*.md"},
		Deny:    []string{"/workspace/src/secret/**"},
	})

	if err := pol.check("read", "/workspace/src/a.ts"); err != nil {
		t.Fatalf("允许的读被拒: %v", err)
	}
	if err := pol.check("read", "src/sub/b.ts"); err != nil {
		t.Fatalf("相对路径应按 Cwd 解析: %v", err)
	}
	if err := pol.check("read", "/workspace/docs/readme.md"); err != nil {
		t.Fatalf("单段 glob 失败: %v", err)
	}
	assertCode(t, pol.check("read", "/workspace/docs/sub/readme.md"), "EACCES")
	assertCode(t, pol.check("read", "/workspace/src/secret/x"), "EACCES")
	assertCode(t, pol.check("read", "/etc/passwd"), "EACCES")
	assertCode(t, pol.check("write", "/workspace/src/a.ts"), "EROFS")
}

func TestFSPolicyWriteAllow(t *testing.T) {
	pol := mustFSPolicy(t, FSPermission{
		Enabled:    true,
		Root:       t.TempDir(),
		Access:     FSReadWrite,
		Allow:      []string{"/workspace/**"},
		WriteAllow: []string{"/workspace/out/**"},
		Deny:       []string{"/workspace/out/locked/**"},
	})
	if err := pol.check("write", "/workspace/out/a.txt"); err != nil {
		t.Fatalf("允许的写被拒: %v", err)
	}
	if err := pol.check("read", "/workspace/src/a.txt"); err != nil {
		t.Fatalf("读应允许: %v", err)
	}
	assertCode(t, pol.check("write", "/workspace/src/a.txt"), "EACCES")
	assertCode(t, pol.check("write", "/workspace/out/locked/a.txt"), "EACCES")
	if err := pol.checkAccess("write"); err != nil {
		t.Fatalf("可写策略的 fd 写被拒: %v", err)
	}
}

func TestFSPolicyReadOnlyFD(t *testing.T) {
	pol := mustFSPolicy(t, FSPermission{
		Enabled: true, Root: t.TempDir(), Access: FSReadOnly,
	})
	assertCode(t, pol.checkAccess("write"), "EROFS")
	if err := pol.checkAccess("read"); err != nil {
		t.Fatalf("只读策略的 fd 读被拒: %v", err)
	}
}

func TestFSPolicyRelativeGetters(t *testing.T) {
	pol := mustFSPolicy(t, FSPermission{
		Enabled: true,
		Root:    t.TempDir(),
		Mount:   "/work",
		Cwd:     "/work/app",
		Allow:   []string{"**"},
	})
	if err := pol.check("read", "a.txt"); err != nil {
		t.Fatalf("相对路径应相对 Cwd: %v", err)
	}
	if err := pol.check("read", "/work/../etc"); err == nil {
		t.Fatal("越界路径应被拒绝")
	}
}

func TestFSPermissionValidate(t *testing.T) {
	if err := (FSPermission{}).Validate(); err != nil {
		t.Fatalf("零值应合法: %v", err)
	}
	err := (FSPermission{Enabled: true}).Validate()
	if err == nil || !strings.Contains(err.Error(), "Root") {
		t.Fatalf("缺少 Root 应报错，实际 %v", err)
	}
	err = (FSPermission{Enabled: true, Root: "x", Cwd: "/other"}).Validate()
	if err == nil {
		t.Fatal("Cwd 越界应报错")
	}
	err = (FSPermission{Enabled: true, Root: "x", Allow: []string{"/etc/**"}}).Validate()
	if err == nil {
		t.Fatal("Mount 外的模式应报错")
	}
}

func TestNetPolicyDial(t *testing.T) {
	pol := mustNetPolicy(t, NetPermission{
		Enabled:  true,
		Outbound: true,
		DNS:      true,
		Allow: []string{
			"https://api.example.com:443",
			"*.cdn.example.com",
			"tcp://10.0.0.0/8:443",
		},
		Deny: []string{"https://blocked.example.com:443"},
	})

	cases := []struct {
		host, port, scheme string
		ok                 bool
	}{
		{"api.example.com", "443", "https", true},
		{"api.example.com", "80", "https", false},
		{"api.example.com", "443", "http", false},
		{"cdn.example.com", "443", "https", false},
		{"www.cdn.example.com", "80", "http", true},
		{"a.b.cdn.example.com", "80", "http", true},
		{"example.com", "443", "https", false},
		{"blocked.example.com", "443", "https", false},
		{"10.1.2.3", "443", "tcp", true},
		{"10.1.2.3", "80", "tcp", false},
		{"8.8.8.8", "443", "tcp", false},
	}
	for _, c := range cases {
		err := pol.checkDial(c.host, c.port, c.scheme)
		if c.ok && err != nil {
			t.Fatalf("%s:%s/%s 应允许，实际 %v", c.host, c.port, c.scheme, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%s:%s/%s 应拒绝", c.host, c.port, c.scheme)
		}
	}
}

func TestNetPolicyEmptyAllowDenies(t *testing.T) {
	pol := mustNetPolicy(t, NetPermission{Enabled: true, Outbound: true})
	err := pol.checkDial("example.com", "443", "https")
	assertCode(t, err, "ERR_ACCESS_DENIED")
}

func TestNetPolicyPrivateIP(t *testing.T) {
	pol := mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true,
		Allow: []string{"https://api.example.com:443"},
	})
	if err := pol.checkIP(net.ParseIP("8.8.8.8"), 443, "https"); err != nil {
		t.Fatalf("公网 IP 应允许: %v", err)
	}
	assertCode(t, pol.checkIP(net.ParseIP("127.0.0.1"), 443, "https"), "ERR_ACCESS_DENIED")
	assertCode(t, pol.checkIP(net.ParseIP("10.0.0.1"), 443, "https"), "ERR_ACCESS_DENIED")

	pol2 := mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true,
		Allow:           []string{"tcp://10.0.0.0/8:443"},
		AllowPrivateIPs: false,
	})
	if err := pol2.checkIP(net.ParseIP("10.1.2.3"), 443, "tcp"); err != nil {
		t.Fatalf("CIDR 明确允许的私网 IP 应通过: %v", err)
	}
}

func TestNetPolicyListenDNS(t *testing.T) {
	pol := mustNetPolicy(t, NetPermission{
		Enabled: true, Inbound: true, DNS: true,
		Listen: []string{"127.0.0.1:0"},
		Deny:   []string{"secret.example.com"},
	})
	if err := pol.checkListen("127.0.0.1", "0"); err != nil {
		t.Fatalf("回环监听应允许: %v", err)
	}
	assertCode(t, pol.checkListen("0.0.0.0", "8080"), "ERR_ACCESS_DENIED")
	if err := pol.checkDNS("api.example.com"); err != nil {
		t.Fatalf("DNS 应允许: %v", err)
	}
	assertCode(t, pol.checkDNS("secret.example.com"), "ERR_ACCESS_DENIED")

	noDNS := mustNetPolicy(t, NetPermission{Enabled: true, DNS: false})
	assertCode(t, noDNS.checkDNS("example.com"), "ERR_ACCESS_DENIED")
}

func TestNetRuleParsing(t *testing.T) {
	bad := []string{"", "https://", "example.com:abc", "[::1", "10.0.0.0/8:99999"}
	for _, s := range bad {
		if _, err := parseNetRule(s); err == nil {
			t.Fatalf("规则 %q 应报错", s)
		}
	}
	good := []string{"*", "example.com", "*.example.com:443", "https://api.example.com:443", "10.0.0.0/8:443", "[::1]:8080"}
	for _, s := range good {
		if _, err := parseNetRule(s); err != nil {
			t.Fatalf("规则 %q 应合法: %v", s, err)
		}
	}
}

func TestChildEnvPermissions(t *testing.T) {
	c := ChildPermission{Enabled: true, Allow: []string{"git", "go*"}}
	if err := c.check("git"); err != nil {
		t.Fatalf("git 应允许: %v", err)
	}
	if err := c.check("/usr/bin/git"); err != nil {
		t.Fatalf("绝对路径的 basename 应匹配: %v", err)
	}
	assertCode(t, c.check("bash"), "ERR_ACCESS_DENIED")
	assertCode(t, (ChildPermission{Enabled: true}).check("git"), "ERR_ACCESS_DENIED")

	e := EnvPermission{Allow: []string{"ADA_*"}}
	if !e.check("ADA_TOKEN") {
		t.Fatal("ADA_TOKEN 应允许")
	}
	if e.check("PATH") {
		t.Fatal("PATH 不应允许")
	}
}

func TestOptionsValidateAndLegacyFS(t *testing.T) {
	art := testArtifact(t)
	base := Options{Artifact: art}

	bad := base
	bad.Mode = LoopMode(99)
	if err := bad.validate(); err == nil {
		t.Fatal("非法 Mode 应报错")
	}
	bad = base
	bad.Limits.MemoryBytes = -1
	if err := bad.validate(); err == nil {
		t.Fatal("负内存限制应报错")
	}

	o := Options{
		Artifact: art,
		Node: &NodeOptions{FS: &NodeFSOptions{
			Root: "x", Mount: "/w", Write: true,
		}},
	}
	p, err := o.fsPermission()
	if err != nil {
		t.Fatalf("旧 Node.FS 映射失败: %v", err)
	}
	if !p.Enabled || p.Access != FSReadWrite || p.Mount != "/w" {
		t.Fatalf("旧 Node.FS 映射不符: %+v", p)
	}

	o.Permissions.FS = FSPermission{Enabled: true, Root: "y", Mount: "/p", Access: FSReadOnly}
	p, err = o.fsPermission()
	if err != nil {
		t.Fatalf("Permissions.FS 映射失败: %v", err)
	}
	if p.Root != "y" || p.Mount != "/p" || p.Access != FSReadOnly {
		t.Fatalf("Permissions.FS 应优先: %+v", p)
	}
}

func TestPermissionView(t *testing.T) {
	fsPol := mustFSPolicy(t, FSPermission{
		Enabled: true, Root: t.TempDir(), Access: FSReadOnly,
		Allow: []string{"/workspace/**"}, Deny: []string{"/workspace/secret/**"},
	})
	netPol := mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true, Inbound: true, DNS: true,
		Allow:  []string{"https://api.example.com:443"},
		Listen: []string{"127.0.0.1:0"},
	})
	r := &Runtime{
		fsPolicy:  fsPol,
		netPolicy: netPol,
		opts: Options{Permissions: Permissions{
			Child: ChildPermission{Enabled: true, Allow: []string{"git"}},
			Env:   EnvPermission{Allow: []string{"ADA_*"}},
		}},
	}
	v := r.Permission()
	if !v.Has("fs.read", "/workspace/a.txt") || v.Has("fs.read", "/workspace/secret/a") {
		t.Fatal("fs.read 视图不符")
	}
	if v.Has("fs.write", "/workspace/a.txt") {
		t.Fatal("只读策略不应允许 fs.write")
	}
	if !v.Has("net", "https://api.example.com:443") || v.Has("net", "https://other.example.com:443") {
		t.Fatal("net 视图不符")
	}
	if !v.Has("net.listen", "127.0.0.1:0") || v.Has("net.listen", "0.0.0.0:80") {
		t.Fatal("net.listen 视图不符")
	}
	if !v.Has("dns", "example.com") {
		t.Fatal("dns 视图不符")
	}
	if !v.Has("child", "git") || v.Has("child", "bash") {
		t.Fatal("child 视图不符")
	}
	if !v.Has("env", "ADA_X") || v.Has("env", "PATH") {
		t.Fatal("env 视图不符")
	}
}

func TestLimitsValidate(t *testing.T) {
	if err := (Limits{}).Validate(); err != nil {
		t.Fatalf("零值应合法: %v", err)
	}
	if err := (Limits{CallTimeout: time.Second}).Validate(); err != nil {
		t.Fatalf("合法限制被拒: %v", err)
	}
	if err := (Limits{MaxSockets: -1}).Validate(); err == nil {
		t.Fatal("负限制应报错")
	}
}
