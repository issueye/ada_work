package app

import (
	"path/filepath"
	"testing"
)

// TestAgentKindValue 守住运行协议归一化:空/未知值按 RPC,只有 acp 走 ACP。
func TestAgentKindValue(t *testing.T) {
	cases := map[string]string{
		"": "rpc", "rpc": "rpc", "ACP": "acp", "acp": "acp", "mystery": "rpc",
	}
	for in, want := range cases {
		if got := (Prefs{AgentKind: in}).AgentKindValue(); got != want {
			t.Fatalf("AgentKindValue(%q) = %q,期望 %q", in, got, want)
		}
	}
}

// TestEnsureProvidersMigratesLegacy 守住旧配置迁移:单供应商字段变成一条列表记录,
// 并回填 ActiveProvider 与模型。
func TestEnsureProvidersMigratesLegacy(t *testing.T) {
	p := Prefs{Provider: "openai", Endpoint: "https://api.deepseek.com", Model: "deepseek-chat", APIKey: "sk-x"}
	p.EnsureProviders()

	if len(p.Providers) != 1 {
		t.Fatalf("应迁移出 1 个供应商,实际 %d", len(p.Providers))
	}
	prov := p.Providers[0]
	if prov.Kind != "openai" || prov.Endpoint != "https://api.deepseek.com" || prov.APIKey != "sk-x" {
		t.Fatalf("供应商字段迁移不完整: %+v", prov)
	}
	if prov.Model != "deepseek-chat" || len(prov.Models) != 1 || prov.Models[0].ID != "deepseek-chat" {
		t.Fatalf("模型没有迁进列表: %+v", prov)
	}
	if p.ActiveProvider != prov.ID {
		t.Fatalf("ActiveProvider 应指向迁移出的供应商: %q", p.ActiveProvider)
	}
}

// TestSyncLegacyFollowsActiveProvider 切换当前供应商后,旧的单供应商字段跟着变。
func TestSyncLegacyFollowsActiveProvider(t *testing.T) {
	p := Prefs{Providers: []Provider{
		{ID: "a", Name: "A", Kind: "openai", Endpoint: "https://a.example", APIKey: "ka", Model: "m-a", Models: []ModelSpec{{ID: "m-a"}}},
		{ID: "b", Name: "B", Kind: "openai", Endpoint: "https://b.example", APIKey: "kb", Model: "m-b", Models: []ModelSpec{{ID: "m-b"}}},
	}, ActiveProvider: "b"}
	p.EnsureProviders()

	if p.Provider != "openai" || p.Endpoint != "https://b.example" || p.Model != "m-b" || p.APIKey != "kb" {
		t.Fatalf("旧字段没有跟随当前供应商: %+v", p)
	}

	p.ActiveProvider = "a"
	p.SyncLegacy()
	if p.Endpoint != "https://a.example" || p.Model != "m-a" {
		t.Fatalf("切回 A 后旧字段应更新: %+v", p)
	}
}

// TestEnsureProvidersFillsDefaults 空列表不造供应商;空模型 ID 不会被当成有效模型。
func TestEnsureProvidersFillsDefaults(t *testing.T) {
	p := Prefs{}
	p.EnsureProviders()
	if len(p.Providers) != 0 {
		t.Fatalf("没有旧字段时不应凭空造供应商: %+v", p.Providers)
	}
	if p.Provider != "" || p.Endpoint != "" || p.Model != "" {
		t.Fatalf("空偏好不应写回旧字段: %+v", p)
	}
	if len(p.ThinkingTags) != 3 || p.ThinkingTags[0] != "low" || p.ThinkingTags[1] != "medium" || p.ThinkingTags[2] != "high" {
		t.Fatalf("默认思考标签应为 low/medium/high: %v", p.ThinkingTags)
	}
}

// TestProviderModelParamsPersist 守住模型参数落盘:保存后重新加载仍在。
func TestProviderModelParamsPersist(t *testing.T) {
	dir := t.TempDir()
	restore := SetConfigPath(filepath.Join(dir, "config.json"))
	t.Cleanup(restore)

	a := New()
	a.Prefs.Providers = []Provider{{
		ID: "p", Name: "P", Kind: "openai", Endpoint: "https://api.deepseek.com", APIKey: "sk-x",
		Model: "m", Models: []ModelSpec{{
			ID: "m", ContextWindow: 131072, MaxOutput: 8192, SummaryThreshold: 80, Thinking: "high",
		}},
	}}
	a.Prefs.ActiveProvider = "p"
	a.Prefs.ThinkingTags = []string{"low", "high"}
	a.Prefs.SyncLegacy()
	if err := a.SavePrefs(); err != nil {
		t.Fatalf("保存偏好失败: %v", err)
	}

	b := New()
	if len(b.Prefs.Providers) != 1 || len(b.Prefs.Providers[0].Models) != 1 {
		t.Fatalf("重启后供应商/模型丢失: %+v", b.Prefs.Providers)
	}
	m := b.Prefs.Providers[0].Models[0]
	if m.ContextWindow != 131072 || m.MaxOutput != 8192 || m.SummaryThreshold != 80 || m.Thinking != "high" {
		t.Fatalf("模型参数没有落盘: %+v", m)
	}
	if len(b.Prefs.ThinkingTags) != 2 {
		t.Fatalf("思考标签没有落盘: %v", b.Prefs.ThinkingTags)
	}
}
