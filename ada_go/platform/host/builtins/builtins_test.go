package builtins_test

import (
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// 内置 Go 插件注册表包含安全栈关键 id。
func TestBuiltinGoPlugins(t *testing.T) {
	got := builtins.Plugins()
	for _, id := range []string{
		"seam-fs", "seam-acp", "fs-local", "caps-static", "caps-gate", "shell-local", "seam-approval",
		"seam-llm", "llm-replay", "llm-openai", "seam-prompts", "system-prompt", "seam-tools",
		"agent-loop", "agent-control", "session-stats", "compaction-basic", "plan", "approval-gate",
		"audit-log", "llm-retry", "tool-timeout", "session-jsonl",
		"seam-rpc", "rpc-json", "rpc-net", "rpc-pipe",
		"seam-secrets", "secrets-env", "secrets-guard",
		"rpc-tls", "rpc-guard", "rpc-observe",
		"seam-workspace", "workspace-local", "workspace-guard", "workspace-helper",
		"tool-helper", "acp-helper",
	} {
		if _, ok := got[id]; !ok {
			t.Fatalf("内置注册表缺少 %q", id)
		}
	}
}

// applyAutoGrants 把清单 caps 灌进提供 caps 服务的插件配置。
func TestApplyAutoGrants(t *testing.T) {
	capsPlugin := &plugin.Manifest{ID: "caps-static", Kind: "go", Provide: []string{"caps"}}
	consumer := &plugin.Manifest{ID: "consumer", Kind: "go", Trust: "third-party", Caps: []string{"fs.read"}}
	manifests := []*plugin.Manifest{capsPlugin, consumer}
	if err := builtins.ApplyAutoGrants(manifests); err != nil {
		t.Fatalf("灌入失败: %v", err)
	}
	grants, ok := capsPlugin.Config["grants"].(map[string][]string)
	if !ok {
		t.Fatalf("caps-static 配置缺少 grants: %+v", capsPlugin.Config)
	}
	if len(grants["consumer"]) != 1 || grants["consumer"][0] != "fs.read" {
		t.Fatalf("授权表不正确: %+v", grants)
	}

	// 既有 config grants 应保留(并集)。
	capsPlugin.Config = map[string]any{"grants": map[string][]string{"other": {"exec"}}}
	if err := builtins.ApplyAutoGrants(manifests); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	merged := capsPlugin.Config["grants"].(map[string][]string)
	if len(merged["other"]) != 1 || len(merged["consumer"]) != 1 {
		t.Fatalf("并集不正确: %+v", merged)
	}
}

// verified trust 的插件应能获得高信任能力(安装来源灌入 caps 的最后一环)。
func TestApplyAutoGrantsVerified(t *testing.T) {
	capsPlugin := &plugin.Manifest{ID: "caps-static", Kind: "go", Provide: []string{"caps"}}
	trusted := &plugin.Manifest{ID: "trusted", Kind: "ts", Trust: "verified", Caps: []string{"fs.write"}}
	if err := builtins.ApplyAutoGrants([]*plugin.Manifest{capsPlugin, trusted}); err != nil {
		t.Fatalf("verified 灌入失败: %v", err)
	}
	grants := capsPlugin.Config["grants"].(map[string][]string)
	if len(grants["trusted"]) != 1 || grants["trusted"][0] != "fs.write" {
		t.Fatalf("verified 授权不正确: %+v", grants)
	}
}

// 非法 caps 应使灌入失败(不静默)。
func TestApplyAutoGrantsInvalid(t *testing.T) {
	bad := &plugin.Manifest{ID: "bad", Kind: "go", Trust: "third-party", Caps: []string{"fs.write"}}
	if err := builtins.ApplyAutoGrants([]*plugin.Manifest{bad}); err == nil {
		t.Fatalf("trust 不足应报错")
	}
}
