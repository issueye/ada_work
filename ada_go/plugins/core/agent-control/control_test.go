package controlgo_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlgo "github.com/issueye/ada_go_v2/plugins/core/agent-control"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
)

type fakeRunner struct {
	reg *session.Registry
}

func (f fakeRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return f.reg.Open(ctx, "", meta)
}

func (f fakeRunner) Run(_ context.Context, sess session.Session, text string) (seamagent.Result, error) {
	_, _ = sess.Append(session.Event{Type: session.TypeAssistantMessage, Data: json.RawMessage(`{"text":"echo:` + text + `"}`)})
	return seamagent.Result{TurnID: "t1", Text: "echo:" + text}, nil
}

// blockingRunner 在 Append 之后等待测试放行。
//
// 如果控制层把事件攒到 Run 返回后才转发,这里会一直等不到 update,
// 测试即超时失败——它验证的是「运行中转发」,不是「结束后补发」。
type blockingRunner struct {
	reg     *session.Registry
	release chan struct{}
}

func (f blockingRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return f.reg.Open(ctx, "", meta)
}

func (f blockingRunner) Run(_ context.Context, sess session.Session, text string) (seamagent.Result, error) {
	if _, err := sess.Append(session.Event{
		Type: session.TypeAssistantMessage,
		Data: json.RawMessage(`{"text":"stream:` + text + `"}`),
	}); err != nil {
		return seamagent.Result{}, err
	}
	<-f.release
	return seamagent.Result{TurnID: "t1", Text: "stream:" + text}, nil
}

// mount 装配 session + 内存 provider + 假 runner + agent-control,返回控制层门面。
func mount(t *testing.T, mk func(*session.Registry) seamagent.Runner) (*kernel.App, *control.Client) {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	for _, p := range []kernel.Plugin{
		session.Plugin(),
		memory.Plugin(),
		approval.Plugin(),
	} {
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
	}
	reg, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	runner := mk(reg)
	if _, err := app.Plugin(kernel.Func("fake-agent", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("agent.runner", runner, kernel.WithVersion(1), kernel.WithKind("object"))
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
	return app, control.New(inv)
}

func TestGoControlPlugin(t *testing.T) {
	app, c := mount(t, func(reg *session.Registry) seamagent.Runner { return fakeRunner{reg: reg} })
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "demo"})
	if err != nil || info.ID == "" || info.Title != "demo" {
		t.Fatalf("Go control create 不符: %+v err=%v", info, err)
	}
	updates := make(chan control.Update, 1)
	dispose := app.Root().On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				select {
				case updates <- u:
				default:
				}
			}
		}
		return nil, nil
	})
	defer func() { _ = dispose(context.Background()) }()
	turn, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"})
	if err != nil || turn.ID != "t1" || turn.Text != "echo:hi" {
		t.Fatalf("Go control prompt 不符: %+v err=%v", turn, err)
	}
	select {
	case u := <-updates:
		if u.SessionID != info.ID || u.Kind != session.TypeAssistantMessage {
			t.Fatalf("control/update 不符: %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到 control/update")
	}
	list, err := c.List(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("Go control list 不符: %+v err=%v", list, err)
	}
}

func TestGoControlStreamsWhileRunning(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	app, c := mount(t, func(reg *session.Registry) seamagent.Runner {
		return blockingRunner{reg: reg, release: release}
	})
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "demo"})
	if err != nil || info.ID == "" {
		t.Fatalf("create 失败: %+v err=%v", info, err)
	}
	updates := make(chan control.Update, 4)
	dispose := app.Root().On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				select {
				case updates <- u:
				default:
				}
			}
		}
		return nil, nil
	})
	defer func() { _ = dispose(context.Background()) }()

	done := make(chan error, 1)
	go func() {
		_, perr := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"})
		done <- perr
	}()

	select {
	case u := <-updates:
		if u.SessionID != info.ID || u.Kind != session.TypeAssistantMessage {
			t.Fatalf("control/update 不符: %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt 未结束时没有收到 control/update(事件被攒到 Run 之后才发)")
	}

	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("prompt 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt 未结束")
	}
}

// allowAll 是一个总是放行的审批人,用来验证选项选择逻辑。
type allowAll struct{}

func (allowAll) Name() string { return "test-allow" }

func (allowAll) Approve(context.Context, approval.Request) (approval.Decision, error) {
	return approval.Decision{Allow: true, Reason: "test"}, nil
}

func TestGoControlPermissionPicksAllowOption(t *testing.T) {
	app, c := mount(t, func(reg *session.Registry) seamagent.Runner { return fakeRunner{reg: reg} })
	reg, err := kernel.Service[*approval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(allowAll{}); err != nil {
		t.Fatalf("注册审批人失败: %v", err)
	}
	dec, err := c.RequestPermission(context.Background(), control.PermissionRequest{
		SessionID: "s1", Tool: "fs.write", Title: "写文件",
		Options: []control.PermissionOption{
			{ID: "reject_once", Label: "拒绝", Kind: control.OptionRejectOnce},
			{ID: "allow_once", Label: "允许", Kind: control.OptionAllowOnce},
		},
	})
	if err != nil {
		t.Fatalf("permission 失败: %v", err)
	}
	if dec.Cancelled || dec.OptionID != control.OptionAllowOnce {
		t.Fatalf("应选中放行选项: %+v", dec)
	}
}
