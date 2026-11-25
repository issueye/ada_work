package workspace_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// fake 是只记录调用顺序的底座。
type fake struct{ calls *[]string }

func (f fake) Current(context.Context) (workspace.Info, error) {
	*f.calls = append(*f.calls, "base")
	return workspace.Info{ID: "repo", Name: "repo", Mount: workspace.MountRoot}, nil
}
func (f fake) Get(context.Context, workspace.ID) (workspace.Info, error) {
	return workspace.Info{}, nil
}
func (f fake) List(context.Context) ([]workspace.Info, error) { return nil, nil }
func (f fake) ResolveHost(context.Context, workspace.Op, string) (workspace.Target, error) {
	*f.calls = append(*f.calls, "base")
	return workspace.Target{Workspace: "repo", Host: "x"}, nil
}
func (f fake) Switch(context.Context, workspace.ID) (workspace.Info, error) {
	return workspace.Info{}, nil
}

// wrapper 只覆盖 Current,其余方法由内嵌接口转发,用来验证装饰器顺序。
type wrapper struct {
	workspace.Service
	tag   string
	calls *[]string
}

func (w wrapper) Current(ctx context.Context) (workspace.Info, error) {
	*w.calls = append(*w.calls, w.tag)
	return w.Service.Current(ctx)
}

// 1. 未安装 → EUNIMPLEMENTED;安装与装饰器按优先级组链,dispose 后恢复。
func TestChainCompose(t *testing.T) {
	var calls []string
	chain := workspace.NewChain()
	if _, err := chain.Get(); err == nil || !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("未安装底座应返回 EUNIMPLEMENTED,实际 %v", err)
	}
	if _, err := chain.Install(fake{calls: &calls}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, err := chain.Install(fake{calls: &calls}); err == nil {
		t.Fatalf("重复安装应报错")
	}

	low, err := chain.Decorate(func(inner workspace.Service) workspace.Service {
		return wrapper{Service: inner, tag: "low", calls: &calls}
	}, 20)
	if err != nil {
		t.Fatalf("注册装饰器失败: %v", err)
	}
	high, err := chain.Decorate(func(inner workspace.Service) workspace.Service {
		return wrapper{Service: inner, tag: "high", calls: &calls}
	}, 100)
	if err != nil {
		t.Fatalf("注册装饰器失败: %v", err)
	}

	svc, err := chain.Get()
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if _, err := svc.Current(context.Background()); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got := strings.Join(calls, ">"); got != "high>low>base" {
		t.Fatalf("装饰器顺序不正确: %s", got)
	}
	if got := chain.Applied(); len(got) != 2 || got[0] != 100 || got[1] != 20 {
		t.Fatalf("Applied 顺序不正确: %v", got)
	}

	if err := high(context.Background()); err != nil {
		t.Fatalf("dispose 失败: %v", err)
	}
	svc, err = chain.Get()
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	calls = nil
	if _, err := svc.Current(context.Background()); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got := strings.Join(calls, ">"); got != "low>base" {
		t.Fatalf("dispose 后装饰器顺序不正确: %s", got)
	}
	_ = low
}

// 2. nil 安装/装饰器与空值语义必须明确报错,不能半注册。
func TestChainRejectsNil(t *testing.T) {
	chain := workspace.NewChain()
	if _, err := chain.Install(nil); err == nil {
		t.Fatalf("nil 底座应报错")
	}
	if _, err := chain.Decorate(nil, 1); err == nil {
		t.Fatalf("nil 装饰器应报错")
	}
}

// 3. 绑定上下文往返;空工作区视为无绑定。
func TestBindingContext(t *testing.T) {
	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "repo"})
	b, ok := workspace.BindingFrom(ctx)
	if !ok || b.SessionID != "s1" || b.Workspace != "repo" {
		t.Fatalf("绑定往返失败: %+v ok=%v", b, ok)
	}
	if _, ok := workspace.BindingFrom(workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s2"})); ok {
		t.Fatalf("空工作区不应产生绑定")
	}
	if _, ok := workspace.BindingFrom(context.Background()); ok {
		t.Fatalf("空 ctx 不应有绑定")
	}
}

// 4. 错误构造器必须带码;Op 语义必须明确。
func TestErrorsAndOps(t *testing.T) {
	cases := []struct {
		err  error
		code seamerr.Code
	}{
		{workspace.NoWorkspace(), workspace.CodeNoWorkspace},
		{workspace.NotFound("missing"), workspace.CodeNotFound},
		{workspace.Invalid("bad"), workspace.CodeInvalid},
		{workspace.Conflict("conflict"), workspace.CodeConflict},
		{workspace.Denied("workspace", "escape"), workspace.CodeDenied},
	}
	for _, tc := range cases {
		if !seamerr.Is(tc.err, tc.code) {
			t.Fatalf("错误码不匹配: %v 期望 %s", tc.err, tc.code)
		}
	}

	for _, op := range []workspace.Op{workspace.OpRead, workspace.OpWrite, workspace.OpEdit, workspace.OpList, workspace.OpSearch, workspace.OpExec} {
		if !op.Valid() {
			t.Fatalf("%s 应为合法 op", op)
		}
	}
	if workspace.Op("bogus").Valid() {
		t.Fatalf("未知 op 不应合法")
	}
	for _, op := range []workspace.Op{workspace.OpRead, workspace.OpList, workspace.OpSearch} {
		if op.Mutating() {
			t.Fatalf("%s 不应是 mutating", op)
		}
	}
	for _, op := range []workspace.Op{workspace.OpWrite, workspace.OpEdit, workspace.OpExec} {
		if !op.Mutating() {
			t.Fatalf("%s 应是 mutating", op)
		}
	}
}

// 5. seam 插件只提供服务值;未装 provider 时消费者拿到 EUNIMPLEMENTED。
func TestPluginProvidesChain(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = app.Stop(ctx)
	})
	if _, err := app.Plugin(workspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 插件失败: %v", err)
	}
	chain, err := kernel.Service[*workspace.Chain](app.Root(), workspace.ServiceName)
	if err != nil {
		t.Fatalf("取 workspace 服务失败: %v", err)
	}
	if _, err := chain.Get(); err == nil || !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("无 provider 应返回 EUNIMPLEMENTED,实际 %v", err)
	}
}
