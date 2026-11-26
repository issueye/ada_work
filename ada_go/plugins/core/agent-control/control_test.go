package controlgo_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlgo "github.com/issueye/ada_go_v2/plugins/core/agent-control"
	memory "github.com/issueye/ada_go_v2/plugins/provider/session-memory"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	seamagent "github.com/issueye/ada_go_v2/plugins/seam/agent"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
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

// streamingRunner 在 Run 里先发两段 agent/delta,再落一条 assistant/message;
// 控制层应当只发增量、跳过整条消息。
type streamingRunner struct {
	reg *session.Registry
	bus *kernel.Context
}

func (r *streamingRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return r.reg.Open(ctx, "", meta)
}

func (r *streamingRunner) Run(ctx context.Context, sess session.Session, _ string) (seamagent.Result, error) {
	emit := func(kind, text string) {
		r.bus.EmitContext(ctx, seamagent.EventDelta, seamagent.DeltaEvent{
			SessionID: sess.ID(), TurnID: "t1", StepID: "s1", Kind: kind, Text: text,
		})
	}
	emit("reasoning", "想")
	emit("text", "你")
	emit("text", "好")
	if _, err := sess.Append(session.Event{
		Type: session.TypeAssistantMessage, TurnID: "t1", StepID: "s1",
		Data: json.RawMessage(`{"message":{"content":[{"kind":"text","text":"你好"}]}}`),
	}); err != nil {
		return seamagent.Result{}, err
	}
	return seamagent.Result{TurnID: "t1", Text: "你好"}, nil
}

func TestGoControlStreamsDeltas(t *testing.T) {
	sr := &streamingRunner{}
	app, c := mount(t, func(reg *session.Registry) seamagent.Runner {
		sr.reg = reg
		return sr
	})
	sr.bus = app.Root()
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "stream"})
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan control.Update, 16)
	dispose := app.Root().On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				updates <- u
			}
		}
		return nil, nil
	})
	defer func() { _ = dispose(context.Background()) }()

	turn, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"})
	if err != nil || turn.Text != "你好" {
		t.Fatalf("prompt 结果不符: %+v err=%v", turn, err)
	}
	var texts, kinds []string
	for {
		select {
		case u := <-updates:
			if u.Kind == session.TypeAssistantMessage {
				t.Fatalf("已有增量时不应再发整条 assistant 消息: %+v", u)
			}
			if u.Kind == control.UpdateAssistantDelta || u.Kind == control.UpdateAssistantReasoning {
				var d struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(u.Data, &d)
				texts = append(texts, d.Text)
				kinds = append(kinds, u.Kind)
			}
		default:
			goto drained
		}
	}
drained:
	wantKinds := []string{
		control.UpdateAssistantReasoning, control.UpdateAssistantDelta, control.UpdateAssistantDelta,
	}
	if len(kinds) != len(wantKinds) || strings.Join(kinds, ",") != strings.Join(wantKinds, ",") {
		t.Fatalf("增量类型不符: %+v", kinds)
	}
	if len(texts) != 3 || texts[0] != "想" || texts[1] != "你" || texts[2] != "好" {
		t.Fatalf("增量顺序不符: %+v", texts)
	}
}

// cancelRunner 一直阻塞到 ctx 取消,用于验证取消映射成 cancelled。
type cancelRunner struct {
	reg     *session.Registry
	started chan struct{}
}

func (r cancelRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return r.reg.Open(ctx, "", meta)
}

func (r cancelRunner) Run(ctx context.Context, _ session.Session, _ string) (seamagent.Result, error) {
	close(r.started)
	<-ctx.Done()
	return seamagent.Result{TurnID: "t1"}, ctx.Err()
}

func TestGoControlCancelReturnsCancelled(t *testing.T) {
	started := make(chan struct{})
	_, c := mount(t, func(reg *session.Registry) seamagent.Runner {
		return cancelRunner{reg: reg, started: started}
	})
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		turn control.TurnResult
		err  error
	}
	done := make(chan result, 1)
	go func() {
		turn, perr := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"})
		done <- result{turn, perr}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt 没有启动")
	}
	if err := c.Cancel(context.Background(), info.ID); err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.turn.StopReason != "cancelled" {
			t.Fatalf("取消应返回 cancelled: %+v err=%v", res.turn, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后 prompt 没有返回")
	}
}

// resumableRunner 模拟支持 session.Resumer 的 agent:按 id 从 registry 恢复会话。
type resumableRunner struct{ reg *session.Registry }

func (r resumableRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return r.reg.Open(ctx, "", meta)
}

func (r resumableRunner) Run(_ context.Context, sess session.Session, text string) (seamagent.Result, error) {
	_, _ = sess.Append(session.Event{Type: session.TypeAssistantMessage, Data: json.RawMessage(`{"text":"resumed:"` + text + `"}`)})
	return seamagent.Result{TurnID: "t1", Text: "resumed:" + text}, nil
}

func (r resumableRunner) Resume(ctx context.Context, id string) (session.Session, error) {
	p, err := r.reg.Provider("")
	if err != nil {
		return nil, err
	}
	res, ok := p.(session.Resumer)
	if !ok {
		return nil, errors.New("provider 不支持 Resumer")
	}
	return res.OpenID(ctx, id, session.Meta{})
}

func TestGoControlLoadResumes(t *testing.T) {
	app, c := mount(t, func(reg *session.Registry) seamagent.Runner { return resumableRunner{reg: reg} })
	reg, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	// 会话由 provider 直接创建,控制层内存表里没有它。
	sess, err := reg.Open(context.Background(), "", session.Meta{Title: "persisted"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Load(context.Background(), sess.ID())
	if err != nil || info.ID != sess.ID() || info.Title != "persisted" {
		t.Fatalf("load 应恢复既有会话: %+v err=%v", info, err)
	}
	turn, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: sess.ID(), Text: "hi"})
	if err != nil || turn.Text != "resumed:hi" {
		t.Fatalf("恢复后 prompt 不符: %+v err=%v", turn, err)
	}
	if _, err := c.Load(context.Background(), "missing"); err == nil {
		t.Fatal("未知 id 应失败")
	}
}

func TestGoControlHistory(t *testing.T) {
	_, c := mount(t, func(reg *session.Registry) seamagent.Runner { return fakeRunner{reg: reg} })
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "history"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	history, err := c.History(context.Background(), info.ID)
	if err != nil || len(history) == 0 {
		t.Fatalf("history 结果不符: %d err=%v", len(history), err)
	}
	if history[len(history)-1].Kind != session.TypeAssistantMessage {
		t.Fatalf("最后一条应是 assistant/message: %+v", history[len(history)-1])
	}
	for _, u := range history {
		if u.SessionID != info.ID {
			t.Fatalf("history 混入其它会话: %+v", u)
		}
	}
	if _, err := c.History(context.Background(), "missing"); err == nil {
		t.Fatal("未知会话的 history 应失败")
	}
}

// reasoningOnlyRunner 只发推理增量、不流正文;整段 assistant 消息不能被抑制。
type reasoningOnlyRunner struct {
	reg *session.Registry
	bus *kernel.Context
}

func (r *reasoningOnlyRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	return r.reg.Open(ctx, "", meta)
}

func (r *reasoningOnlyRunner) Run(ctx context.Context, sess session.Session, _ string) (seamagent.Result, error) {
	r.bus.EmitContext(ctx, seamagent.EventDelta, seamagent.DeltaEvent{
		SessionID: sess.ID(), TurnID: "t1", StepID: "s1", Kind: "reasoning", Text: "想",
	})
	if _, err := sess.Append(session.Event{
		Type: session.TypeAssistantMessage, TurnID: "t1", StepID: "s1",
		Data: json.RawMessage(`{"message":{"content":[{"kind":"text","text":"答案"}]}}`),
	}); err != nil {
		return seamagent.Result{}, err
	}
	return seamagent.Result{TurnID: "t1", Text: "答案"}, nil
}

func TestGoControlReasoningDoesNotSuppressMessage(t *testing.T) {
	rr := &reasoningOnlyRunner{}
	app, c := mount(t, func(reg *session.Registry) seamagent.Runner {
		rr.reg = reg
		return rr
	})
	rr.bus = app.Root()
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "reasoning"})
	if err != nil {
		t.Fatal(err)
	}
	updates := make(chan control.Update, 16)
	dispose := app.Root().On(control.EventUpdate, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
		if len(args) > 0 {
			if u, ok := args[0].(control.Update); ok {
				updates <- u
			}
		}
		return nil, nil
	})
	defer func() { _ = dispose(context.Background()) }()

	if _, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	var reasoning, message int
	for {
		select {
		case u := <-updates:
			switch u.Kind {
			case control.UpdateAssistantReasoning:
				reasoning++
			case session.TypeAssistantMessage:
				message++
			case control.UpdateAssistantDelta:
				t.Fatalf("没有正文增量时不应发 assistant/delta: %+v", u)
			}
		default:
			goto drained
		}
	}
drained:
	if reasoning != 1 || message != 1 {
		t.Fatalf("推理=1 且整段消息=1,实际 reasoning=%d message=%d", reasoning, message)
	}
}

func TestGoControlRejectsConcurrentPrompt(t *testing.T) {
	started := make(chan struct{})
	_, c := mount(t, func(reg *session.Registry) seamagent.Runner {
		return cancelRunner{reg: reg, started: started}
	})
	info, err := c.Create(context.Background(), control.CreateRequest{Title: "one"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "first"})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("第一个 prompt 没有启动")
	}
	if _, err := c.Prompt(context.Background(), control.PromptRequest{SessionID: info.ID, Text: "second"}); err == nil {
		t.Fatal("并发 prompt 应被拒绝")
	}
	if err := c.Cancel(context.Background(), info.ID); err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("取消后第一个 prompt 没有返回")
	}
}

// metaRunner 记录 Open 收到的 meta,用于验证 cwd → workspace 绑定。
type metaRunner struct {
	reg  *session.Registry
	meta session.Meta
}

func (r *metaRunner) Open(ctx context.Context, meta session.Meta) (session.Session, error) {
	r.meta = meta
	return r.reg.Open(ctx, "", meta)
}

func (r *metaRunner) Run(context.Context, session.Session, string) (seamagent.Result, error) {
	return seamagent.Result{TurnID: "t1"}, nil
}

func TestGoControlCreateBindsWorkspaceByCwd(t *testing.T) {
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin, cfg kernel.Config) {
		t.Helper()
		if _, err := app.Plugin(p, cfg); err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
	}
	mount(session.Plugin(), kernel.NewConfig(nil))
	mount(memory.Plugin(), kernel.NewConfig(nil))
	mount(approval.Plugin(), kernel.NewConfig(nil))
	mount(workspace.Plugin(), kernel.NewConfig(nil))
	mount(workspacelocal.Plugin(), kernel.NewConfig(map[string]any{
		"active": "repo",
		"workspaces": []any{
			map[string]any{"id": "repo", "root": root},
		},
	}))
	reg, err := kernel.Service[*session.Registry](app.Root(), "session")
	if err != nil {
		t.Fatal(err)
	}
	runner := &metaRunner{reg: reg}
	mount(kernel.Func("fake-agent", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("agent.runner", runner, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("agent.runner"), kernel.NewConfig(nil))
	mount(controlgo.Plugin(), kernel.NewConfig(nil))

	raw, err := app.Root().Service(control.ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	c := control.New(raw.(control.Invoker))
	if _, err := c.Create(context.Background(), control.CreateRequest{Cwd: root}); err != nil {
		t.Fatalf("cwd 应绑定到工作区: %v", err)
	}
	if runner.meta.Workspace != "repo" {
		t.Fatalf("会话 meta.Workspace 应为 repo,实际 %+v", runner.meta)
	}
	if _, err := c.Create(context.Background(), control.CreateRequest{Cwd: filepath.Join(root, "other")}); err == nil {
		t.Fatal("cwd 不属于任何工作区时应拒绝")
	}
}
