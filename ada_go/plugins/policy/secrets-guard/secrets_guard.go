// Package secretsguard 是 secrets 的授权策略:principal + secrets.<name> 能力检查。
//
// 门禁(装饰器);priority=200。优先使用注入的 caps 服务(与 caps-static 共享)。
package secretsguard

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

// PolicyName 是插件 id。
const PolicyName = "secrets-guard"

// Priority 是装饰器优先级。
const Priority = 200

// New 构造授权装饰器(测试可直接使用)。
func New(inner seamsecrets.Getter, broker *caps.Broker) seamsecrets.Getter {
	return &guard{inner: inner, broker: broker}
}

type guard struct {
	inner  seamsecrets.Getter
	broker *caps.Broker
}

func (g *guard) Get(ctx context.Context, name string) (string, error) {
	if err := seamsecrets.ValidateName(name); err != nil {
		return "", err
	}
	id, ok := kernel.Principal(ctx)
	if !ok {
		return "", seamsecrets.Denied("缺少调用者身份:请使用 ctx.Context()")
	}
	if err := g.broker.Authorize(ctx, id, "secrets."+name, name); err != nil {
		return "", seamsecrets.Denied(err.Error())
	}
	return g.inner.Get(ctx, name)
}

type config struct {
	Grants map[string][]string `json:"grants,omitempty"`
}

// Plugin 返回 secrets-guard 插件:inject secrets(可选 caps 服务),注册授权装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamsecrets.Chain](ctx, "secrets")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "secrets-guard: 解析配置失败: %v", err)
		}
		broker := caps.NewBroker(func(d caps.Deny) { ctx.Emit(caps.EventDeny, d) })
		fromService := false
		if v, ok := ctx.TryService("caps"); ok {
			if b, ok := v.(*caps.Broker); ok {
				broker = b
				fromService = true
			}
		}
		if !fromService {
			for plugin, list := range conf.Grants {
				if err := broker.Grant(plugin, list...); err != nil {
					return nil, kernel.Fail(kernel.CodeInvalid, "secrets-guard: 授予 %q 失败: %v", plugin, err)
				}
			}
		}
		return chain.Decorate(func(inner seamsecrets.Getter) seamsecrets.Getter {
			return New(inner, broker)
		}, Priority)
	}).WithOptional("caps", "secrets")
}
