package approval_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type fakeApprover struct {
	name string
	dec  seamapproval.Decision
	err  error
}

func (f *fakeApprover) Name() string { return f.name }
func (f *fakeApprover) Approve(context.Context, seamapproval.Request) (seamapproval.Decision, error) {
	return f.dec, f.err
}

type deny struct{}

func (deny) Name() string { return "deny" }
func (deny) Approve(context.Context, seamapproval.Request) (seamapproval.Decision, error) {
	return seamapproval.Decision{Allow: false, Reason: "拒绝", Approver: "deny"}, nil
}

type slow struct{}

func (slow) Name() string { return "slow" }
func (slow) Approve(ctx context.Context, _ seamapproval.Request) (seamapproval.Decision, error) {
	<-ctx.Done()
	return seamapproval.Decision{}, ctx.Err()
}

// 1. 注册表:默认选择确定、重复注册拒绝、没有审批人报错。
func TestRegistry(t *testing.T) {
	registry := seamapproval.NewRegistry()
	if _, err := registry.Get(""); err == nil {
		t.Fatalf("空注册表应报错")
	}
	if _, err := registry.Register(deny{}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if got := registry.Names(); len(got) != 1 || got[0] != "deny" {
		t.Fatalf("Names 不正确: %v", got)
	}
	if _, err := registry.Register(deny{}); err == nil {
		t.Fatalf("重复注册应失败")
	}
	if _, err := registry.Get(""); err != nil {
		t.Fatalf("默认选择失败: %v", err)
	}
}

// 2. Ask:拒绝结果原样返回;没有服务时 fail-closed。
func TestAskDenied(t *testing.T) {
	registry := seamapproval.NewRegistry()
	_, _ = registry.Register(deny{})
	dec, err := seamapproval.Ask(context.Background(), registry, "", seamapproval.Request{Source: "fs"})
	if err != nil {
		t.Fatalf("审批拒绝不应报错: %v", err)
	}
	if dec.Allow || dec.Reason == "" {
		t.Fatalf("拒绝结果不正确: %+v", dec)
	}

	if _, err := seamapproval.Ask(context.Background(), nil, "", seamapproval.Request{}); err == nil {
		t.Fatalf("没有 approval 服务时应报错")
	}
}

// 3. Ask 强制超时:挂起的审批人不能阻塞调用方,超时按拒绝处理。
func TestAskTimeout(t *testing.T) {
	registry := seamapproval.NewRegistry()
	_, _ = registry.Register(slow{})
	start := time.Now()
	dec, err := seamapproval.Ask(context.Background(), registry, "", seamapproval.Request{Timeout: 50 * time.Millisecond})
	if err == nil || !seamerr.Is(err, seamerr.ErrTimeout) {
		t.Fatalf("超时应返回 ETIMEOUT,实际 %v", err)
	}
	if dec.Allow {
		t.Fatalf("超时后不应放行")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("超时没有及时返回: %s", elapsed)
	}
}

// requested 通知:在审批人之前触发,内容完整。
func TestRequestedNotifier(t *testing.T) {
	reg := seamapproval.NewRegistry()
	impl := &fakeApprover{name: "ui", dec: seamapproval.Decision{Allow: true}}
	if _, err := reg.Register(impl); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	var got seamapproval.Request
	calls := 0
	reg.SetNotifier(func(_ context.Context, req seamapproval.Request) {
		calls++
		got = req
	})
	req := seamapproval.Request{ID: "r1", Source: "fs", Op: "write", Target: "/workspace/a", Reason: "升级"}
	if _, err := seamapproval.Ask(context.Background(), reg, "", req); err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	if calls != 1 || got.ID != "r1" || got.Op != "write" || got.Target != "/workspace/a" {
		t.Fatalf("通知内容不正确: calls=%d got=%+v", calls, got)
	}
}

// 没有审批人时不触发 requested(fail-closed 先决条件)。
func TestNotifierNoApprover(t *testing.T) {
	reg := seamapproval.NewRegistry()
	calls := 0
	reg.SetNotifier(func(context.Context, seamapproval.Request) { calls++ })
	if _, err := seamapproval.Ask(context.Background(), reg, "", seamapproval.Request{ID: "r1"}); err == nil {
		t.Fatalf("没有审批人应失败")
	}
	if calls != 0 {
		t.Fatalf("没有审批人不应广播 requested,实际 %d", calls)
	}
}

// 通知器 panic 不影响审批结果。
func TestNotifierPanic(t *testing.T) {
	reg := seamapproval.NewRegistry()
	impl := &fakeApprover{name: "ui", dec: seamapproval.Decision{Allow: true}}
	if _, err := reg.Register(impl); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	reg.SetNotifier(func(context.Context, seamapproval.Request) { panic("boom") })
	if dec, err := seamapproval.Ask(context.Background(), reg, "", seamapproval.Request{ID: "r1"}); err != nil || !dec.Allow {
		t.Fatalf("通知器 panic 不应影响审批: dec=%+v err=%v", dec, err)
	}
}

// 内核集成:requested 事件带调用者身份。
func TestRequestedEvent(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(name string, p kernel.Plugin) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/approval", seamapproval.Plugin())

	var payload map[string]any
	mount("listener", kernel.Func("listener", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		ctx.On(seamapproval.EventRequested, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
			if len(args) > 0 {
				if m, ok := args[0].(map[string]any); ok {
					payload = m
				}
			}
			return nil, nil
		})
		return nil, nil
	}))
	mount("approver", kernel.Func("approver", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamapproval.Registry](ctx, "approval")
		if err != nil {
			return nil, err
		}
		_, err = reg.Register(&fakeApprover{name: "ui", dec: seamapproval.Decision{Allow: true}})
		return nil, err
	}).WithInject("approval"))
	mount("consumer", kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamapproval.Registry](ctx, "approval")
		if err != nil {
			return nil, err
		}
		_, err = seamapproval.Ask(ctx.Context(), reg, "", seamapproval.Request{
			ID: "r1", Source: "fs", Op: "write", Target: "/workspace/a",
		})
		return nil, err
	}).WithInject("approval"))

	if payload == nil || payload["id"] != "r1" || payload["plugin"] != "consumer" {
		t.Fatalf("requested payload 不正确: %+v", payload)
	}
}

// 5. 审批 target 的 URL query/userinfo 在提示与事件里脱敏。
func TestRedactTarget(t *testing.T) {
	got := seamapproval.RedactTarget("https://user:pass@example.com/x?token=SECRET#frag")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "pass") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("target 未脱敏: %q", got)
	}
	if got := seamapproval.RedactTarget("/workspace/a"); got != "/workspace/a" {
		t.Fatalf("非 URL 目标不应改变: %q", got)
	}
}
