// Package approvaldeny 是 `provider/approval-deny`:默认拒绝一切升级请求。
//
// 它让「没有配置审批人」不再是隐式行为:装配时显式装一个默认拒绝的审批人,
// 任何升级请求都会得到可展示的拒绝理由,并且不会阻塞等待 30 秒。
package approvaldeny

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
)

// ProviderName 是审批人名与插件 id。
const ProviderName = "approval-deny"

// Deny 是无交互的默认审批人。
type Deny struct{}

// New 创建默认拒绝审批人。
func New() *Deny { return &Deny{} }

// Name 返回审批人名。
func (Deny) Name() string { return ProviderName }

// Approve 永远拒绝,并给出明确理由。
func (Deny) Approve(context.Context, approval.Request) (approval.Decision, error) {
	return approval.Decision{
		Allow:    false,
		Reason:   "默认审批人:拒绝升级(未配置交互式审批)",
		Approver: ProviderName,
	}, nil
}

// Plugin 返回 approval-deny 插件:inject approval seam,注册默认拒绝审批人。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*approval.Registry](ctx, "approval")
		if err != nil {
			return nil, err
		}
		return registry.Register(New())
	}).WithInject("approval")
}
