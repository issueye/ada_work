// Package systemprompt 是 `core/system-prompt`:把 identity/rules/tools 说明注册为
// prompts seam 的 Section。
//
// 它不拼接整个系统提示——排序、预算与 waterfall 属于 seam/prompts 的引擎。
package systemprompt

import (
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/prompts"
)

// PluginName 是插件 id。
const PluginName = "system-prompt"

type config struct {
	Identity string   `json:"identity,omitempty"`
	Rules    []string `json:"rules,omitempty"`
	Tools    string   `json:"tools,omitempty"`
}

// Plugin 返回 system-prompt 插件:inject prompts,注册默认 section。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		engine, err := kernel.Service[*prompts.Engine](ctx, "prompts")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "system-prompt: 解析配置失败: %v", err)
		}

		sections := []prompts.Section{
			{Key: "identity", Title: "Identity", Body: conf.Identity, Order: 0, Scope: prompts.ScopeSession},
			{Key: "rules", Title: "Rules", Body: joinRules(conf.Rules), Order: 10, Scope: prompts.ScopeSession},
			{Key: "tools", Title: "Tools", Body: conf.Tools, Order: 20, Scope: prompts.ScopeSession},
		}
		for _, s := range sections {
			if s.Body == "" {
				continue
			}
			sec := s
			if _, err := ctx.Effect("prompt/"+sec.Key, func() (kernel.Dispose, error) {
				return engine.Section(sec)
			}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}).WithInject("prompts")
}

func joinRules(rules []string) string {
	out := ""
	for _, r := range rules {
		if r == "" {
			continue
		}
		out += "- " + r + "\n"
	}
	return out
}
