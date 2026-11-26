package controlgo_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlgo "github.com/issueye/ada_go_v2/plugins/core/agent-control"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

type fakeRunner struct {
	reg *session.Registry
}

func (f fakeRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return f.reg.Open(ctx, "", meta)
}

func (f fakeRunner) Run(_ context.Context, _ session.Session, text string) (seamagent.Result, error) {
	return seamagent.Result{TurnID: "t1", Text: "echo:" + text}, nil
}

func TestGoControlPlugin(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	for _, p := range []kernel.Plugin{
		session.Plugin(),
		memory.Plugin(),
	} {
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
	}
	reg, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Plugin(kernel.Func("fake-agent", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("agent.runner", fakeRunner{reg: reg}, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("agent.runner"), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 fake agent 失败: %v", err)
	}
	if _, err := app.Plugin(controlgo.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 agent-control 失败: %v", err)
	}

	raw, err := app.Root().Service(control.ServiceName)
	if err != nil {
		t.Fatalf("control 服务未注册: %v", err)
	}
	inv, ok := raw.(control.Invoker)
	if !ok {
		t.Fatalf("control 服务类型不对: %T", raw)
	}
	c := control.New(inv)
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "demo"})
	if err != nil || info.ID == "" || info.Title != "demo" {
		t.Fatalf("Go control create 不符: %+v err=%v", info, err)
	}
	turn, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"})
	if err != nil || turn.ID != "t1" || turn.Text != "echo:hi" {
		t.Fatalf("Go control prompt 不符: %+v err=%v", turn, err)
	}
	list, err := c.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("Go control list 不符: %+v err=%v", list, err)
	}
}
