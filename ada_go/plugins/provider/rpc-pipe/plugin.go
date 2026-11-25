// Package rpcpipe 是 `provider/rpc-pipe`:Windows 命名管道传输。
//
// 非 Windows 平台仍注册 `pipe` 传输,但方法返回 EUNIMPLEMENTED;
// 这样地址解析与错误码在各平台一致,不会「静默消失」。
package rpcpipe

import (
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// ProviderName 是插件 id。
const ProviderName = "rpc-pipe"

// Plugin 返回 rpc-pipe 插件:注册 pipe 传输。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamrpc.Registry](ctx, "rpc-transport")
		if err != nil {
			return nil, err
		}
		return reg.Register(seamrpc.Pipe, newTransport())
	}).WithInject("rpc-transport")
}
