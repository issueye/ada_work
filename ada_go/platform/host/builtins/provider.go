// 本文件是宿主对 LLM 供应商的运行期选择:ada_chat 一类宿主用环境变量
// (ADA_LLM_PROVIDER / ADA_LLM_ENDPOINT / ADA_LLM_MODEL) 切换供应商,
// 装配前把选择落成清单的 disable 状态与配置,preset 本身不用改。
//
// 放在 builtins 而不是 preset:预设是纯数据层(不该 import 具体插件),而这里要读
// llm-openai 的默认端点/模型;builtins 本来就是「内置插件接线」的地方。
package builtins

import (
	"fmt"
	"strings"

	"github.com/issueye/ada_go_v2/platform/plugin"
	openai "github.com/issueye/ada_go_v2/plugins/provider/llm-openai"
)

// DefaultAPIKeyEnv 是 llm-openai 读取密钥的环境变量名。
const DefaultAPIKeyEnv = "ADA_LLM_API_KEY"

// ProviderOverride 是宿主对 LLM 供应商的选择;Provider 为空表示跟随 preset。
type ProviderOverride struct {
	Provider  string // openai | replay
	Endpoint  string
	Model     string
	APIKeyEnv string // 密钥所在环境变量名;空用 DefaultAPIKeyEnv
}

// FromEnv 读取宿主注入的供应商选择环境变量。
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
	openaiM := builtinManifest(manifests, "llm-openai")
	replayM := builtinManifest(manifests, "llm-replay")
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
		setBuiltinConfig(openaiM, "endpoint", endpoint)
		setBuiltinConfig(openaiM, "model", model)
		setBuiltinConfig(openaiM, "apiKeyEnv", firstNonBlank(ov.APIKeyEnv, DefaultAPIKeyEnv))
		setBuiltinConfig(findBuiltin(manifests, "agent-loop"), "model", model)
	case "replay":
		if replayM == nil {
			return fmt.Errorf("preset 不包含 llm-replay,无法切到回放")
		}
		replayM.Disabled = false
		if openaiM != nil {
			openaiM.Disabled = true
		}
		setBuiltinConfig(findBuiltin(manifests, "agent-loop"), "model", replayDefaultModel(replayM))
	default:
		return fmt.Errorf("未知的 LLM provider: %q(只支持 openai / replay)", provider)
	}
	return nil
}

// builtinManifest 查找清单;preset 收口后清单可能不存在。
func builtinManifest(manifests []*plugin.Manifest, id string) *plugin.Manifest {
	return findBuiltin(manifests, id)
}

func findBuiltin(manifests []*plugin.Manifest, id string) *plugin.Manifest {
	for _, m := range manifests {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// setBuiltinConfig 写入清单配置(nil Config 与 nil 清单都要安全)。
func setBuiltinConfig(m *plugin.Manifest, key string, value any) {
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
