package builtins

import (
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/plugin"
	openai "github.com/issueye/ada_go_v2/plugins/provider/llm-openai"
)

func manifestsForProvider() []*plugin.Manifest {
	return []*plugin.Manifest{
		{ID: "agent-loop", Config: map[string]any{"model": "replay"}},
		{ID: "llm-replay", Config: map[string]any{"defaultModel": "replay"}},
		{ID: "llm-openai", Disabled: true, Config: map[string]any{}},
	}
}

// TestApplyProviderOverrideOpenAI 覆盖宿主切到 OpenAI 兼容端点时的四个动作:
// 启用 openai、停用 replay、写 endpoint/model/apiKeyEnv、把 agent-loop 指到所选模型。
func TestApplyProviderOverrideOpenAI(t *testing.T) {
	ms := manifestsForProvider()
	ov := ProviderOverride{Provider: "openai", Endpoint: "https://example.com/v1", Model: "mock-model"}
	if err := ApplyProviderOverride(ms, ov); err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	openaiM := findBuiltin(ms, "llm-openai")
	replayM := findBuiltin(ms, "llm-replay")
	if openaiM.Disabled {
		t.Fatal("llm-openai 应启用")
	}
	if !replayM.Disabled {
		t.Fatal("llm-replay 应停用")
	}
	if got := openaiM.Config["endpoint"]; got != "https://example.com/v1" {
		t.Fatalf("endpoint=%v", got)
	}
	if got := openaiM.Config["model"]; got != "mock-model" {
		t.Fatalf("model=%v", got)
	}
	if got := openaiM.Config["apiKeyEnv"]; got != DefaultAPIKeyEnv {
		t.Fatalf("apiKeyEnv=%v", got)
	}
	if got := findBuiltin(ms, "agent-loop").Config["model"]; got != "mock-model" {
		t.Fatalf("agent-loop model=%v", got)
	}
}

// TestApplyProviderOverrideDefaults 只给 provider 时用 llm-openai 的默认端点与模型。
func TestApplyProviderOverrideDefaults(t *testing.T) {
	ms := manifestsForProvider()
	if err := ApplyProviderOverride(ms, ProviderOverride{Provider: "openai"}); err != nil {
		t.Fatal(err)
	}
	openaiM := findBuiltin(ms, "llm-openai")
	if openaiM.Config["endpoint"] != openai.DefaultEndpoint || openaiM.Config["model"] != openai.DefaultModel {
		t.Fatalf("默认端点/模型未生效: %v", openaiM.Config)
	}
}

// TestApplyProviderOverrideReplay 切回回放:启用 replay、停用 openai、模型取回放清单的默认值。
func TestApplyProviderOverrideReplay(t *testing.T) {
	ms := manifestsForProvider()
	if err := ApplyProviderOverride(ms, ProviderOverride{Provider: "replay"}); err != nil {
		t.Fatal(err)
	}
	if findBuiltin(ms, "llm-openai").Disabled == false {
		t.Fatal("llm-openai 应停用")
	}
	if findBuiltin(ms, "llm-replay").Disabled {
		t.Fatal("llm-replay 应启用")
	}
	if got := findBuiltin(ms, "agent-loop").Config["model"]; got != "replay" {
		t.Fatalf("agent-loop model=%v", got)
	}
}

// TestApplyProviderOverrideErrors 空 provider 跟随 preset;未知 provider 与缺插件都要报错。
func TestApplyProviderOverrideErrors(t *testing.T) {
	ms := manifestsForProvider()
	if err := ApplyProviderOverride(ms, ProviderOverride{}); err != nil {
		t.Fatalf("空 provider 应跟随 preset: %v", err)
	}
	if err := ApplyProviderOverride(ms, ProviderOverride{Provider: "vendor-x"}); err == nil ||
		!strings.Contains(err.Error(), "未知的 LLM provider") {
		t.Fatalf("未知 provider 应报错,得到 %v", err)
	}
	var none []*plugin.Manifest
	if err := ApplyProviderOverride(none, ProviderOverride{Provider: "openai"}); err == nil {
		t.Fatal("preset 缺 llm-openai 时应报错,不能静默回落")
	}
}

// TestProviderOverrideFromEnv 只读宿主的四个变量,并补上默认的密钥变量名。
func TestProviderOverrideFromEnv(t *testing.T) {
	env := map[string]string{
		"ADA_LLM_PROVIDER": "openai",
		"ADA_LLM_ENDPOINT": "http://127.0.0.1:3050/v1",
		"ADA_LLM_MODEL":    "local-model",
		"ADA_LLM_API_KEY":  "sk-x",
	}
	ov := ProviderOverrideFromEnv(func(k string) string { return env[k] })
	if ov.Provider != "openai" || ov.Endpoint != "http://127.0.0.1:3050/v1" || ov.Model != "local-model" {
		t.Fatalf("解析结果不符: %+v", ov)
	}
	if ov.APIKeyEnv != DefaultAPIKeyEnv {
		t.Fatalf("apiKeyEnv=%q, want %q", ov.APIKeyEnv, DefaultAPIKeyEnv)
	}
}
