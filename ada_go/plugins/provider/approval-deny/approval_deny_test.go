package approvaldeny_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	approvaldeny "github.com/issueye/ada_go_v2/plugins/provider/approval-deny"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
)

// 1. 默认审批人永远拒绝,且理由可展示。
func TestAlwaysDenies(t *testing.T) {
	dec, err := approvaldeny.New().Approve(context.Background(), seamapproval.Request{Source: "fs", Op: "fs.write", Target: "/workspace/a"})
	if err != nil {
		t.Fatalf("审批不应报错: %v", err)
	}
	if dec.Allow {
		t.Fatalf("默认审批人必须拒绝")
	}
	if dec.Reason == "" || dec.Approver != approvaldeny.ProviderName {
		t.Fatalf("拒绝应带理由与审批人名: %+v", dec)
	}
}

// 2. 经内核装配:seam/approval + approval-deny,消费者拿到默认审批人。
func TestKernelIntegration(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	if _, err := app.Plugin(seamapproval.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	if _, err := app.Plugin(approvaldeny.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 provider 失败: %v", err)
	}

	var decision seamapproval.Decision
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*seamapproval.Registry](ctx, "approval")
		if err != nil {
			return nil, err
		}
		approver, err := registry.Get("")
		if err != nil {
			return nil, err
		}
		decision, err = approver.Approve(context.Background(), seamapproval.Request{Source: "shell", Op: "shell.exec", Target: "curl"})
		return nil, err
	}).WithInject("approval")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if decision.Allow {
		t.Fatalf("默认审批结果必须是拒绝")
	}
}

// 3. 没有审批人时 Get 返回 EUNIMPLEMENTED。
func TestNoApprover(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamapproval.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	registry, err := kernel.Service[*seamapproval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	if _, err := registry.Get(""); err == nil {
		t.Fatalf("没有审批人时应返回错误")
	}
}
