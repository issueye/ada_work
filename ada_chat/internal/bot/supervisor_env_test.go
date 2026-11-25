package bot

import "testing"

// TestApplyBotEnv 守住注入规则:机器人自己的变量能覆盖全局,但宿主保留的不能。
func TestApplyBotEnv(t *testing.T) {
	env := []string{"BASE=1", EnvName + "=old"}
	out := applyBotEnv(env, map[string]string{
		"ADA_LLM_API_KEY":     "sk-bot",
		EnvName:               "hijack",
		"ADA_GO_AGENT_PRESET": "evil.json",
	})
	seen := map[string]string{}
	for _, kv := range out {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				seen[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	if seen["ADA_LLM_API_KEY"] != "sk-bot" {
		t.Fatalf("机器人自己的变量应注入: %v", seen)
	}
	if seen[EnvName] != "old" {
		t.Fatalf("宿主保留变量不能被覆盖: %v", seen)
	}
	if _, ok := seen["ADA_GO_AGENT_PRESET"]; ok {
		t.Fatalf("preset 不能被机器人环境覆盖: %v", seen)
	}
}
