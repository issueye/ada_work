// Package capsstatic 是 `provider/caps-static`:提供一个静态的能力授权表。
//
// 它让宿主可以「先建 broker → 灌入清单 caps → 再由 caps-gate 强制」,
// 而不必把 grants 写死在 caps-gate 的配置里。
package capsstatic

import (
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
)

// ProviderName 是插件 id 与服务提供者名。
const ProviderName = "caps-static"

// ServiceName 是所提供的服务名。
const ServiceName = "caps"

// Plugin 返回 caps-static 插件:provide "caps" 服务(*caps.Broker)。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		var conf struct {
			Grants map[string][]string `json:"grants"`
		}
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "caps-static: 解析 grants 失败: %v", err)
		}
		broker := caps.NewBroker(func(d caps.Deny) {
			ctx.Emit(caps.EventDeny, d)
		})
		for plugin, list := range conf.Grants {
			if err := broker.Grant(plugin, list...); err != nil {
				return nil, kernel.Fail(kernel.CodeInvalid, "caps-static: 授予 %q 失败: %v", plugin, err)
			}
		}
		_, err := ctx.Provide(ServiceName, broker, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide(ServiceName)
}
