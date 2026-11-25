// Package secretsenv 是 `provider/secrets-env`:从进程环境读 secret。
//
// 安全默认:必须显式 allow 名单;名单为空则任何 secret 都不可读(fail-closed)。
// 环境变量名 = ADA_SECRET_<name>。
package secretsenv

import (
	"context"
	"os"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

// ProviderName 是插件 id。
const ProviderName = "secrets-env"

// EnvPrefix 是环境变量前缀。
const EnvPrefix = "ADA_SECRET_"

type impl struct {
	allow map[string]bool
}

// New 构造实现;allow 为空表示什么都不允许。
func New(allow []string) seamsecrets.Getter {
	set := make(map[string]bool, len(allow))
	for _, name := range allow {
		set[name] = true
	}
	return &impl{allow: set}
}

func (i *impl) Get(ctx context.Context, name string) (string, error) {
	if err := seamsecrets.ValidateName(name); err != nil {
		return "", err
	}
	if !i.allow[name] {
		return "", seamsecrets.NotFound(name)
	}
	v := os.Getenv(EnvPrefix + name)
	if v == "" {
		return "", seamsecrets.NotFound(name)
	}
	return v, nil
}

type config struct {
	Allow []string `json:"allow,omitempty"`
}

// Plugin 返回 secrets-env 插件:inject secrets seam,Install 实现。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamsecrets.Chain](ctx, "secrets")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "secrets-env: 解析配置失败: %v", err)
		}
		for _, name := range conf.Allow {
			if err := seamsecrets.ValidateName(name); err != nil {
				return nil, kernel.Fail(kernel.CodeInvalid, "secrets-env: %v", err)
			}
		}
		return chain.Install(New(conf.Allow))
	}).WithInject("secrets")
}
