package rpc

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
)

const pluginID = "seam-rpc"

// Plugin 返回 seam/rpc 插件:提供 "rpc"(空 Chain)与 "rpc-transport"(Registry)。
//
// provider 只做 Install/Register,监听是宿主/消费者的显式动作。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain := NewChain()
		reg := NewRegistry()
		d1, err := ctx.Provide("rpc", chain, kernel.WithVersion(1), kernel.WithKind("object"))
		if err != nil {
			return nil, err
		}
		d2, err := ctx.Provide("rpc-transport", reg, kernel.WithVersion(1), kernel.WithKind("object"))
		if err != nil {
			_ = d1(ctx.Context())
			return nil, err
		}
		return func(c context.Context) error {
			_ = d2(c)
			return d1(c)
		}, nil
	}).WithProvide("rpc", "rpc-transport")
}
