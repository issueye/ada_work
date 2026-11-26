package workspacelocal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	seamerr "github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

func newProvider(t *testing.T, opts workspacelocal.Options) (*workspacelocal.Provider, *[]string) {
	t.Helper()
	var events []string
	p, err := workspacelocal.New(opts, func(_ context.Context, name string, payload any) {
		events = append(events, name)
	})
	if err != nil {
		t.Fatalf("构造 provider 失败: %v", err)
	}
	return p, &events
}

func dir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(d); err == nil {
		d = resolved
	}
	return d
}

// 1. 配置错误必须在装配期拒绝。
func TestNewRejectsBadConfig(t *testing.T) {
	cases := []workspacelocal.Options{
		{},
		{Workspaces: []workspacelocal.Workspace{{ID: "Repo", Root: t.TempDir()}}},
		{Workspaces: []workspacelocal.Workspace{{ID: "repo"}}},
		{Workspaces: []workspacelocal.Workspace{{ID: "repo", Root: filepath.Join(t.TempDir(), "missing")}}},
	}
	for i, tc := range cases {
		if _, err := workspacelocal.New(tc, nil); err == nil {
			t.Fatalf("case %d 应报错", i)
		}
	}
}

// 2. 缺省 active 取 ID 字典序第一个;List 排序稳定。
func TestCurrentAndListDeterministic(t *testing.T) {
	r1, r2 := dir(t), dir(t)
	p, _ := newProvider(t, workspacelocal.Options{Workspaces: []workspacelocal.Workspace{
		{ID: "zeta", Root: r1},
		{ID: "alpha", Root: r2},
	}})
	cur, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current 失败: %v", err)
	}
	if cur.ID != "alpha" {
		t.Fatalf("缺省 active 应为 alpha,实际 %s", cur.ID)
	}
	list, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != "alpha" || list[1].ID != "zeta" {
		t.Fatalf("List 顺序不正确: %+v", list)
	}
	// Info 是值拷贝:改调用方拿到的 Ignore 不能影响 provider。
	list[0].Ignore = append(list[0].Ignore, "x")
	if got, _ := p.Get(context.Background(), "alpha"); len(got.Ignore) != 0 {
		t.Fatalf("Info 不应泄漏内部切片: %+v", got)
	}
}

// 3. 绑定优先于 Current;未知绑定不静默回退。
func TestResolveBinding(t *testing.T) {
	r1, r2 := dir(t), dir(t)
	p, _ := newProvider(t, workspacelocal.Options{Active: "alpha", Workspaces: []workspacelocal.Workspace{
		{ID: "alpha", Root: r1},
		{ID: "beta", Root: r2},
	}})
	target, err := p.ResolveHost(context.Background(), workspace.OpRead, "/workspace/a.txt")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if target.Workspace != "alpha" || target.Host != filepath.Join(r1, "a.txt") {
		t.Fatalf("默认解析不正确: %+v", target)
	}

	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "beta"})
	target, err = p.ResolveHost(ctx, workspace.OpWrite, "/workspace/a.txt")
	if err != nil {
		t.Fatalf("绑定解析失败: %v", err)
	}
	if target.Workspace != "beta" || target.Host != filepath.Join(r2, "a.txt") {
		t.Fatalf("绑定解析不正确: %+v", target)
	}

	ctx = workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s2", Workspace: "missing"})
	if _, err := p.ResolveHost(ctx, workspace.OpRead, "/workspace/a.txt"); err == nil || !seamerr.Is(err, workspace.CodeNotFound) {
		t.Fatalf("未知绑定应 ENOTFOUND,实际 %v", err)
	}
}

// 4. Switch 只改变新调用的缺省,已绑定会话不受影响,并广播一次。
func TestSwitchKeepsBinding(t *testing.T) {
	r1, r2 := dir(t), dir(t)
	p, events := newProvider(t, workspacelocal.Options{Active: "alpha", Workspaces: []workspacelocal.Workspace{
		{ID: "alpha", Root: r1},
		{ID: "beta", Root: r2},
	}})
	ctx := workspace.WithBinding(context.Background(), workspace.Binding{SessionID: "s1", Workspace: "alpha"})
	if _, err := p.Switch(ctx, "beta"); err != nil {
		t.Fatalf("Switch 失败: %v", err)
	}
	if len(*events) != 1 || (*events)[0] != workspace.EventChanged {
		t.Fatalf("应广播一次 workspace/changed: %v", *events)
	}
	bound, err := p.ResolveHost(ctx, workspace.OpRead, "/workspace/a")
	if err != nil {
		t.Fatalf("绑定解析失败: %v", err)
	}
	if bound.Workspace != "alpha" {
		t.Fatalf("Switch 不应改变已绑定会话: %+v", bound)
	}
	cur, err := p.ResolveHost(context.Background(), workspace.OpRead, "/workspace/a")
	if err != nil {
		t.Fatalf("当前解析失败: %v", err)
	}
	if cur.Workspace != "beta" {
		t.Fatalf("新调用应使用 beta: %+v", cur)
	}
	if _, err := p.Switch(ctx, "missing"); err == nil || !seamerr.Is(err, workspace.CodeNotFound) {
		t.Fatalf("切换未知工作区应 ENOTFOUND,实际 %v", err)
	}
}

// 5. 只读工作区拒绝所有 mutating 操作。
func TestReadOnly(t *testing.T) {
	p, _ := newProvider(t, workspacelocal.Options{Workspaces: []workspacelocal.Workspace{
		{ID: "repo", Root: dir(t), ReadOnly: true},
	}})
	if _, err := p.ResolveHost(context.Background(), workspace.OpRead, "/workspace/a"); err != nil {
		t.Fatalf("只读读应放行: %v", err)
	}
	for _, op := range []workspace.Op{workspace.OpWrite, workspace.OpEdit, workspace.OpExec} {
		if _, err := p.ResolveHost(context.Background(), op, "/workspace/a"); err == nil || !seamerr.Is(err, workspace.CodeDenied) {
			t.Fatalf("%s 应被拒绝,实际 %v", op, err)
		}
	}
}

// 6. 虚拟路径越界与非法 op 必须拒绝。
func TestResolveRejectsEscape(t *testing.T) {
	p, _ := newProvider(t, workspacelocal.Options{Workspaces: []workspacelocal.Workspace{{ID: "repo", Root: dir(t)}}})
	for _, pth := range []string{"", "a", `\a`, "/etc/passwd", "/workspace/../../x", "/workspace/a\x00b"} {
		if _, err := p.ResolveHost(context.Background(), workspace.OpRead, pth); err == nil {
			t.Fatalf("%q 应被拒绝", pth)
		}
	}
	if _, err := p.ResolveHost(context.Background(), workspace.Op("bogus"), "/workspace/a"); err == nil {
		t.Fatalf("非法 op 应被拒绝")
	}
}

// 7. 符号链接指向根外必须拒绝;不存在目标的父目录链接也要拦住。
func TestResolveSymlinkEscape(t *testing.T) {
	root := dir(t)
	outside := dir(t)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("当前平台无法创建符号链接: %v", err)
	}
	p, _ := newProvider(t, workspacelocal.Options{Workspaces: []workspacelocal.Workspace{{ID: "repo", Root: root}}})
	if _, err := p.ResolveHost(context.Background(), workspace.OpRead, "/workspace/link/a"); err == nil {
		t.Fatalf("链接到根外应被拒绝")
	}
}

// 8. 插件装配:inject workspace seam,安装实现后服务可用。
func TestPluginInstalls(t *testing.T) {
	root := dir(t)
	app := kernel.New(kernel.Options{})
	if _, err := app.Plugin(workspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	cfg := kernel.NewConfig(map[string]any{
		"active": "repo",
		"workspaces": []any{
			map[string]any{"id": "repo", "root": root},
		},
	})
	if _, err := app.Plugin(workspacelocal.Plugin(), cfg); err != nil {
		t.Fatalf("挂载 provider 失败: %v", err)
	}
	chain, err := kernel.Service[*workspace.Chain](app.Root(), workspace.ServiceName)
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	svc, err := chain.Get()
	if err != nil {
		t.Fatalf("取实现失败: %v", err)
	}
	info, err := svc.Current(context.Background())
	if err != nil {
		t.Fatalf("Current 失败: %v", err)
	}
	if info.ID != "repo" {
		t.Fatalf("工作区 ID 不正确: %+v", info)
	}
	if strings.Contains(info.Name, root) {
		t.Fatalf("Info 不应暴露宿主根: %+v", info)
	}
}

// 9. Locate 只接受工作区根本身,且路径不存在时不猜测。
func TestLocateByRoot(t *testing.T) {
	root := dir(t)
	p, _ := newProvider(t, workspacelocal.Options{Workspaces: []workspacelocal.Workspace{
		{ID: "repo", Root: root},
	}})
	info, found, err := p.Locate(context.Background(), root)
	if err != nil || !found || info.ID != "repo" {
		t.Fatalf("工作区根应能定位: %+v found=%v err=%v", info, found, err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, found, err := p.Locate(context.Background(), sub); err != nil || found {
		t.Fatalf("子目录不应模糊匹配工作区: found=%v err=%v", found, err)
	}
	if _, found, err := p.Locate(context.Background(), filepath.Join(root, "missing")); err != nil || found {
		t.Fatalf("不存在的路径不应定位: found=%v err=%v", found, err)
	}
}
