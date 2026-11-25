package caps_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/issueye/ada_go_v2/platform/caps"
)

// 1. 能力名语法:未知命名空间/层级错误/空名一律非法。
func TestParse(t *testing.T) {
	valid := []string{"fs.read", "fs.write", "exec", "process", "process.isolated",
		"secrets.GITHUB_TOKEN", "config.ada", "ui",
		"rpc.call", "rpc.call.agent.session.list", "rpc.listen.tcp", "rpc.listen.unix"}
	for _, spec := range valid {
		if _, err := caps.Parse(spec); err != nil {
			t.Fatalf("%q 应合法: %v", spec, err)
		}
	}
	bad := []string{"", "fs", "fs.exec", "exec.x", "process.foo",
		"net.fetch.example.com", "net.fetch", "net.read.a",
		"secrets", "config", "shell", "fs.read.deep",
		"rpc", "rpc.x", "rpc.call.", "rpc.listen", "rpc.listen.foo"}
	for _, spec := range bad {
		if _, err := caps.Parse(spec); err == nil {
			t.Fatalf("%q 应非法", spec)
		}
	}
}

// 2. 通配:授予前缀可覆盖目标;命名空间不互相放宽。
func TestGrantsWildcard(t *testing.T) {
	g, err := caps.NewGrants("rpc.call", "secrets.*", "fs.read")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if !g.Has("rpc.call.agent.session.list", "") {
		t.Fatalf("rpc.call 应覆盖具体方法")
	}
	if !g.Has("secrets.GITHUB_TOKEN", "") {
		t.Fatalf("secrets.* 应覆盖具体名字")
	}
	if !g.Has("fs.read", "/workspace/a") {
		t.Fatalf("fs.read 应被授予")
	}
	if g.Has("fs.write", "/workspace/a") {
		t.Fatalf("fs.read 不应放宽到 fs.write")
	}
	if g.Has("exec", "") {
		t.Fatalf("不应因其它能力获得 exec")
	}
}

// 3. 授权表:fail-closed、拒绝带错误码、审计、撤销。
func TestBrokerAuthorize(t *testing.T) {
	var denied []caps.Deny
	broker := caps.NewBroker(func(d caps.Deny) { denied = append(denied, d) })
	if err := broker.Grant("plugin-a", "fs.read", "exec"); err != nil {
		t.Fatalf("授予失败: %v", err)
	}

	if err := broker.Authorize(context.Background(), "plugin-a", "fs.read", "/workspace/a"); err != nil {
		t.Fatalf("已授权能力应通过: %v", err)
	}
	if err := broker.Authorize(context.Background(), "plugin-a", "fs.write", "/workspace/a"); err == nil || !caps.IsDenied(err) {
		t.Fatalf("未授权能力应 EDENIED,实际 %v", err)
	}
	if err := broker.Authorize(context.Background(), "plugin-b", "fs.read", "/workspace/a"); err == nil {
		t.Fatalf("未登记插件应拒绝")
	}
	if len(denied) != 2 {
		t.Fatalf("拒绝应写审计,实际 %d 条", len(denied))
	}

	// 非法能力名也按拒绝处理,而不是放行。
	if err := broker.Authorize(context.Background(), "plugin-a", "fs.exec", "/workspace/a"); err == nil {
		t.Fatalf("非法能力名应拒绝")
	}

	broker.Revoke("plugin-a")
	if err := broker.Authorize(context.Background(), "plugin-a", "fs.read", "/workspace/a"); err == nil {
		t.Fatalf("撤销后应拒绝")
	}
}

// 4. 信任等级:exec 需要 official;fs.write 需要 verified;fs.read 第三方可用。
func TestTrust(t *testing.T) {
	cases := []struct {
		cap     caps.Cap
		trust   caps.Trust
		allowed bool
	}{
		{"fs.read", caps.TrustThirdParty, true},
		{"fs.write", caps.TrustThirdParty, false},
		{"fs.write", caps.TrustVerified, true},
		{"exec", caps.TrustVerified, false},
		{"exec", caps.TrustOfficial, true},
		{"process", caps.TrustThirdParty, false},
	}
	for _, tc := range cases {
		if got := caps.AllowedByTrust(tc.cap, tc.trust); got != tc.allowed {
			t.Fatalf("%s @ %s = %v,期望 %v", tc.cap, tc.trust, got, tc.allowed)
		}
	}

}

// 5. 并发授予/授权(-race)。
func TestConcurrent(t *testing.T) {
	broker := caps.NewBroker(nil)
	if err := broker.Grant("p", "fs.read"); err != nil {
		t.Fatalf("授予失败: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := broker.Authorize(context.Background(), "p", "fs.read", "/workspace/a"); err != nil {
				t.Errorf("并发授权失败: %v", err)
			}
		}()
	}
	wg.Wait()
}

// 7. 拒绝审计带 reason,URL target 去掉 query/userinfo。
func TestDenyAuditReasonAndRedaction(t *testing.T) {
	var got caps.Deny
	broker := caps.NewBroker(func(d caps.Deny) { got = d })
	if err := broker.Grant("p", "fs.read"); err != nil {
		t.Fatal(err)
	}
	if err := broker.Authorize(context.Background(), "p", "fs.write", "https://user:pass@example.com/x?token=SECRET"); err == nil {
		t.Fatalf("未授权应拒绝")
	}
	if got.Reason != "能力未授予" {
		t.Fatalf("拒绝 reason 不正确: %+v", got)
	}
	if strings.Contains(got.Target, "SECRET") || strings.Contains(got.Target, "pass") {
		t.Fatalf("拒绝 target 未脱敏: %q", got.Target)
	}
	if !strings.Contains(got.Target, "REDACTED") {
		t.Fatalf("拒绝 target 缺少 REDACTED: %q", got.Target)
	}
}
func TestDeniedMessage(t *testing.T) {
	err := (&caps.DeniedError{Plugin: "p", Cap: "fs.write", Target: "/workspace/a", Reason: "未授予"}).Error()
	if !strings.Contains(err, "EDENIED") || !strings.Contains(err, "fs.write") {
		t.Fatalf("拒绝信息不完整: %s", err)
	}
}
