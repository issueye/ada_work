package bridge_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
)

// TS 插件实现控制层：ctx.provide 一个只有 invoke(method, params) 的对象，
// bridge 负责把它包装成 kernel 中的 control.Invoker。
func TestTSControlService(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `
type Ctx = { provide: (name: string, value: unknown) => void }
export function apply(ctx: Ctx): void {
  ctx.provide("control", {
    invoke(method: string, params: any) {
      if (method === "session.create") return { id: "s1", title: params.title }
      if (method === "session.prompt") return { id: "t1", text: "echo:" + params.text }
      if (method === "session.list") return [{ id: "s1" }]
      if (method === "session.permission") return { optionId: "allow" }
      throw new Error("unsupported: " + method)
    },
  })
}`,
	})
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	p, err := bridge.New(bridge.Options{Name: "ts-control", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

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
	if err != nil || info.ID != "s1" || info.Title != "demo" {
		t.Fatalf("TS control create 不符: %+v err=%v", info, err)
	}
	turn, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: "s1", Text: "hi"})
	if err != nil || turn.Text != "echo:hi" {
		t.Fatalf("TS control prompt 不符: %+v err=%v", turn, err)
	}
	decision, err := c.RequestPermission(context.Background(), control.PermissionRequest{SessionID: "s1"})
	if err != nil || decision.OptionID != "allow" {
		t.Fatalf("TS control permission 不符: %+v err=%v", decision, err)
	}
}
