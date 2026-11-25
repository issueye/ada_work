package assemble_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/assemble"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

func newApp(t *testing.T) *kernel.App {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	return app
}

// 1. 按计划顺序挂载,并真的提供服务让消费者就绪。
func TestUpMountsInPlanOrder(t *testing.T) {
	var order []string
	lookup := func(m *plugin.Manifest) (kernel.Plugin, error) {
		id := m.ID
		return kernel.Func(id, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
			order = append(order, id)
			for _, name := range m.Provide {
				if _, err := ctx.Provide(name, id); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}), nil
	}
	ms := []*plugin.Manifest{
		mf("presenter", nil, []string{"upper"}, nil),
		mf("upper", []string{"upper"}, []string{"greeting"}, nil),
		mf("greeter", []string{"greeting"}, nil, nil),
	}
	app := newApp(t)
	rep := assemble.Plan(ms)
	if !rep.OK() {
		t.Fatalf("计划应通过: %v", rep.Err())
	}
	up, err := assemble.Up(app, ms, rep, lookup)
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if got := strings.Join(order, ","); got != "greeter,upper,presenter" {
		t.Fatalf("挂载顺序不正确: %s", got)
	}
	if len(up.Fibers) != 3 {
		t.Fatalf("应记录 3 个 fiber,实际 %+v", up.Fibers)
	}
}

// 2. 任一插件失败 → 已挂载的插件逆序卸载,不留半装状态。
func TestUpRollsBackOnFailure(t *testing.T) {
	disposed := false
	ok := kernel.Func("a", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		if _, err := ctx.Provide("x", "a"); err != nil {
			return nil, err
		}
		return func(context.Context) error { disposed = true; return nil }, nil
	})
	bad := kernel.Func("b", func(*kernel.Context, kernel.Config) (kernel.Dispose, error) {
		return nil, kernel.Fail(kernel.CodeInvalid, "故意失败")
	})
	lookup := func(m *plugin.Manifest) (kernel.Plugin, error) {
		if m.ID == "a" {
			return ok, nil
		}
		return bad, nil
	}
	ms := []*plugin.Manifest{mf("a", []string{"x"}, nil, nil), mf("b", nil, []string{"x"}, nil)}
	app := newApp(t)
	rep := assemble.Plan(ms)
	if !rep.OK() {
		t.Fatalf("计划应通过: %v", rep.Err())
	}

	_, err := assemble.Up(app, ms, rep, lookup)
	if err == nil {
		t.Fatalf("b 失败时应返回错误")
	}
	var ue *assemble.UpError
	// 失败插件本身已被 mountOne 回收,回滚计数只算此前成功挂载的插件。
	if !errors.As(err, &ue) || ue.Plugin != "b" || ue.RolledBack != 1 {
		t.Fatalf("错误应点名 b 且回滚 1 个,实际 %v", err)
	}
	if !disposed {
		t.Fatalf("已挂载的 a 应被回滚卸载")
	}
	if got := len(app.Inspect().Fibers); got != 0 {
		t.Fatalf("回滚后不应残留 fiber,实际 %d", got)
	}
}

// 3. 计划本身不通过时,Up 必须拒绝执行(不能「装一半再报错」)。
func TestUpRejectsBadPlan(t *testing.T) {
	app := newApp(t)
	ms := []*plugin.Manifest{mf("a", nil, []string{"missing"}, nil)}
	rep := assemble.Plan(ms)
	if rep.OK() {
		t.Fatalf("计划应失败")
	}
	_, err := assemble.Up(app, ms, rep, nil)
	if err == nil || !strings.Contains(err.Error(), assemble.CodePlan) {
		t.Fatalf("应返回计划错误,实际 %v", err)
	}
	if got := len(app.Inspect().Fibers); got != 0 {
		t.Fatalf("不应挂载任何插件,实际 %d", got)
	}
}

// 4. 缺少 Lookup 时给出明确错误,而不是 panic。
func TestUpWithoutLookup(t *testing.T) {
	app := newApp(t)
	ms := []*plugin.Manifest{mf("a", nil, nil, nil)}
	rep := assemble.Plan(ms)
	if _, err := assemble.Up(app, ms, rep, nil); err == nil || !strings.Contains(err.Error(), "Lookup") {
		t.Fatalf("应提示缺少 Lookup,实际 %v", err)
	}
}
