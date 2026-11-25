package assemble_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// GrantManifests:按 caps/trust 授予;trust 不足整批失败。
func TestGrantManifests(t *testing.T) {
	broker := caps.NewBroker(nil)
	ms := []*plugin.Manifest{
		{ID: "a", Caps: []string{"fs.read", "exec"}, Trust: "official"},
		{ID: "b", Caps: []string{"fs.read"}, Trust: "third-party"},
	}
	if err := assemble.GrantManifests(broker, ms); err != nil {
		t.Fatalf("合法清单授予失败: %v", err)
	}
	if err := broker.Authorize(context.TODO(), "a", "exec", "echo"); err != nil {
		t.Fatalf("a 的 exec 应被授予: %v", err)
	}
	if err := broker.Authorize(context.TODO(), "b", "fs.read", "/workspace/a"); err != nil {
		t.Fatalf("b 的 fs.read 应被授予: %v", err)
	}

	bad := []*plugin.Manifest{{ID: "c", Caps: []string{"fs.write"}, Trust: "third-party"}}
	if err := assemble.GrantManifests(caps.NewBroker(nil), bad); err == nil || !strings.Contains(err.Error(), "trust") {
		t.Fatalf("third-party 持有 fs.write 应报错,实际 %v", err)
	}
	badExec := []*plugin.Manifest{{ID: "d", Caps: []string{"exec"}, Trust: "verified"}}
	if err := assemble.GrantManifests(caps.NewBroker(nil), badExec); err == nil {
		t.Fatalf("verified 持有 exec 应报错")
	}
	if err := assemble.GrantManifests(nil, ms); err == nil {
		t.Fatalf("nil broker 应报错")
	}
}
