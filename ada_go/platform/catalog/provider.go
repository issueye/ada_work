package catalog

import (
	"fmt"
	"strings"

	"github.com/issueye/ada_go_v2/platform/plugin"
	openai "github.com/issueye/ada_go_v2/plugins/provider/llm-openai"
)

// DefaultAPIKeyEnv 是 llm-openai 读取密钥的环境变量名。
const DefaultAPIKeyEnv = "ADA_LLM_API_KEY"

// ProviderOverride 是宿主对 LLM 供应商的运行期选择;Provider 为空表示跟随 preset。
//
// 它属于「清单推导」而不是某个宿主适配器:CLI、server 与 ada-rpc-agent 都用它把
// 同一套宿主环境变量折成清单的 disable/config;放在 host/* 会让三处各写一份。
type ProviderOverride struct {
	Provider  string // openai | replay
	Endpoint  string
	Model     string
	APIKeyEnv string // 密钥所在环境变量名;空用 DefaultAPIKeyEnv
}

// ProviderOverrideFromEnv 读取宿主注入的供应商选择环境变量。
func ProviderOverrideFromEnv(getenv func(string) string) ProviderOverride {
	return ProviderOverride{
		Provider:  strings.TrimSpace(getenv("ADA_LLM_PROVIDER")),
		Endpoint:  strings.TrimSpace(getenv("ADA_LLM_ENDPOINT")),
		Model:     strings.TrimSpace(getenv("ADA_LLM_MODEL")),
		APIKeyEnv: DefaultAPIKeyEnv,
	}
}

// ApplyProviderOverride 按宿主选择打开一个 provider、关掉另一个,并把 agent-loop
// 指到对应模型;它只改清单的配置与 disable 状态,不碰插件身份与依赖图。
//
// 目标插件不在 preset 里(或还没被 enable 收口)时报错:静默回落会让宿主以为
// 自己选的模型生效了,实际仍在用 preset 的默认回放。
func ApplyProviderOverride(manifests []*plugin.Manifest, ov ProviderOverride) error {
	provider := strings.ToLower(strings.TrimSpace(ov.Provider))
	if provider == "" {
		return nil // 跟随 preset
	}
	openaiM := findManifest(manifests, "llm-openai")
	replayM := findManifest(manifests, "llm-replay")
	switch provider {
	case "openai":
		if openaiM == nil {
			return fmt.Errorf("preset 不包含 llm-openai,无法切到 OpenAI 兼容端点")
		}
		openaiM.Disabled = false
		if replayM != nil {
			replayM.Disabled = true
		}
		endpoint := firstNonBlank(ov.Endpoint, openai.DefaultEndpoint)
		model := firstNonBlank(ov.Model, openai.DefaultModel)
		setManifestConfig(openaiM, "endpoint", endpoint)
		setManifestConfig(openaiM, "model", model)
		setManifestConfig(openaiM, "apiKeyEnv", firstNonBlank(ov.APIKeyEnv, DefaultAPIKeyEnv))
		setManifestConfig(findManifest(manifests, "agent-loop"), "model", model)
	case "replay":
		if replayM == nil {
			return fmt.Errorf("preset 不包含 llm-replay,无法切到回放")
		}
		replayM.Disabled = false
		if openaiM != nil {
			openaiM.Disabled = true
		}
		setManifestConfig(findManifest(manifests, "agent-loop"), "model", replayDefaultModel(replayM))
	default:
		return fmt.Errorf("未知的 LLM provider: %q(只支持 openai / replay)", provider)
	}
	return nil
}

// findManifest 按 id 查找清单;preset 收口后清单可能不存在。
func findManifest(manifests []*plugin.Manifest, id string) *plugin.Manifest {
	for _, m := range manifests {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// setManifestConfig 写入清单配置(nil Config 与 nil 清单都要安全)。
func setManifestConfig(m *plugin.Manifest, key string, value any) {
	if m == nil {
		return
	}
	if m.Config == nil {
		m.Config = map[string]any{}
	}
	m.Config[key] = value
}

// replayDefaultModel 取回放清单的默认模型,没有就回落到 "replay"。
func replayDefaultModel(m *plugin.Manifest) string {
	if v, ok := m.Config["defaultModel"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if models, ok := m.Config["models"].([]any); ok && len(models) > 0 {
		if first, ok := models[0].(map[string]any); ok {
			if id, ok := first["id"].(string); ok && strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	return "replay"
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
