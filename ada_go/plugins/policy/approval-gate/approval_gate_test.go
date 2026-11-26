package approvalgate_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	approvalgate "github.com/issueye/ada_go_v2/plugins/policy/approval-gate"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
)

type approver struct {
	name  string
	allow bool
}

func (a approver) Name() string { return a.name }
func (a approver) Approve(context.Context, seamapproval.Request) (seamapproval.Decision, error) {
	return seamapproval.Decision{Allow: a.allow, Approver: a.name, Reason: "test"}, nil
}

// captureApprover 记录 gate 传给审批人的请求。
type captureApprover struct {
	req seamapproval.Request
}

func (a *captureApprover) Name() string { return "capture" }
func (a *captureApprover) Approve(_ context.Context, req seamapproval.Request) (seamapproval.Decision, error) {
	a.req = req
	return seamapproval.Decision{Allow: true, Approver: a.Name()}, nil
}

func setup(t *testing.T, allow *bool, withApproval bool) (*kernel.App, *seamtools.Registry, *int32) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	// 手工挂载顺序:tools → approval → gate,避免测试依赖 preset。
	if _, err := app.Plugin(seamtools.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatal(err)
	}
	if withApproval {
		if _, err := app.Plugin(seamapproval.Plugin(), kernel.NewConfig(nil)); err != nil {
			t.Fatal(err)
		}
		reg, err := kernel.Service[*seamapproval.Registry](app.Root(), "approval")
		if err != nil {
			t.Fatal(err)
		}
		ok := false
		if allow != nil {
			ok = *allow
		}
		if _, err := reg.Register(approver{name: "test", allow: ok}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Plugin(approvalgate.Plugin(), kernel.NewConfig(map[string]any{"tools": []string{"danger"}})); err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	_, err = reg.Register(seamtools.ToolDef{
		Name: "danger",
		Handler: func(context.Context, seamtools.CallInfo) (seamtools.Result, error) {
			calls++
			return seamtools.Result{Status: seamtools.StatusOK}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, reg, &calls
}

func TestApprovalGateAllowDeny(t *testing.T) {
	ctx := kernel.WithPrincipal(context.Background(), "p")
	allow := true
	app, reg, calls := setup(t, &allow, true)
	defer func() { _ = app.Stop(context.Background()) }()
	res, err := reg.Invoke(ctx, seamtools.CallInfo{Name: "danger"})
	if err != nil || res.Status != seamtools.StatusOK || *calls != 1 {
		t.Fatalf("批准后应执行: %+v %v calls=%d", res, err, *calls)
	}

	deny := false
	app2, reg2, calls2 := setup(t, &deny, true)
	defer func() { _ = app2.Stop(context.Background()) }()
	res, _ = reg2.Invoke(ctx, seamtools.CallInfo{Name: "danger"})
	if res.Status != seamtools.StatusDenied || *calls2 != 0 {
		t.Fatalf("拒绝后不应执行: %+v calls=%d", res, *calls2)
	}
}

func TestApprovalGateFailClosedWithoutService(t *testing.T) {
	app, reg, calls := setup(t, nil, false)
	defer func() { _ = app.Stop(context.Background()) }()
	res, _ := reg.Invoke(kernel.WithPrincipal(context.Background(), "p"), seamtools.CallInfo{Name: "danger"})
	if res.Status != seamtools.StatusDenied || *calls != 0 {
		t.Fatalf("无 approval 服务应拒绝: %+v calls=%d", res, *calls)
	}
}

func TestApprovalGatePassesSessionDetails(t *testing.T) {
	app := kernel.New(kernel.Options{})
	defer func() { _ = app.Stop(context.Background()) }()
	if _, err := app.Plugin(seamtools.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Plugin(seamapproval.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatal(err)
	}
	reg, err := kernel.Service[*seamapproval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatal(err)
	}
	capture := &captureApprover{}
	if _, err := reg.Register(capture); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Plugin(approvalgate.Plugin(), kernel.NewConfig(map[string]any{"tools": []string{"danger"}})); err != nil {
		t.Fatal(err)
	}
	toolsReg, err := kernel.Service[*seamtools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolsReg.Register(seamtools.ToolDef{
		Name: "danger",
		Handler: func(context.Context, seamtools.CallInfo) (seamtools.Result, error) {
			return seamtools.Result{Status: seamtools.StatusOK}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := toolsReg.Invoke(context.Background(), seamtools.CallInfo{
		ID: "call-1", Name: "danger", SessionID: "sess-1",
	})
	if err != nil || res.Status != seamtools.StatusOK {
		t.Fatalf("审批后应执行: %+v %v", res, err)
	}
	if got := capture.req.Details["sessionId"]; got != "sess-1" {
		t.Fatalf("审批请求应带 sessionId,实际 %+v", capture.req.Details)
	}
	if got := capture.req.Details["toolCallId"]; got != "call-1" {
		t.Fatalf("审批请求应带 toolCallId,实际 %+v", capture.req.Details)
	}
}
