package workspaceguard_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	workspaceguard "github.com/issueye/ada_go_v2/plugins/policy/workspace-guard"
	workspacelocal "github.com/issueye/ada_go_v2/plugins/provider/workspace-local"
	seamerr "github.com/issueye/ada_go_v2/plugins/seam/seamerr"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

func newGuard(t *testing.T, conf workspaceguard.Config) (workspace.Service, *[]map[string]any) {
	t.Helper()
	var events []map[string]any
	inner, err := workspacelocal.New(workspacelocal.Options{Workspaces: []workspacelocal.Workspace{
		{ID: "repo", Root: t.TempDir()},
	}}, nil)
	if err != nil {
		t.Fatalf("创建 provider 失败: %v", err)
	}
	return workspaceguard.New(inner, conf, func(_ context.Context, name string, payload any) {
		if name == workspace.EventDenied {
			if m, ok := payload.(map[string]any); ok {
				events = append(events, m)
			}
		}
	}), &events
}

// 1. 固定前缀按路径段边界匹配:.git 拦 .git/config,不拦 .gitignore。
func TestWritePrefix(t *testing.T) {
	svc, events := newGuard(t, workspaceguard.Config{DenyWrite: []string{".git", "bin"}})
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/.git/config"); err == nil || !seamerr.Is(err, workspace.CodeDenied) {
		t.Fatalf(".git/config 写应 EDENIED,实际 %v", err)
	}
	if len(*events) != 1 {
		t.Fatalf("应发一次 workspace/denied: %v", *events)
	}
	if _, ok := (*events)[0]["path"]; !ok {
		t.Fatalf("事件应带 path: %+v", (*events)[0])
	}
	if _, ok := (*events)[0]["host"]; ok {
		t.Fatalf("事件不得带宿主路径")
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/.gitignore"); err != nil {
		t.Fatalf(".gitignore 不应被 .git 规则命中: %v", err)
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/src/a.go"); err != nil {
		t.Fatalf("普通路径应放行: %v", err)
	}
}

// 2. 读/执行使用各自列表;读拒绝不影响同路径的写。
func TestReadExecLists(t *testing.T) {
	svc, _ := newGuard(t, workspaceguard.Config{DenyRead: []string{".env"}, DenyExec: []string{".git"}})
	if _, err := svc.ResolveHost(context.Background(), workspace.OpRead, "/workspace/.env"); err == nil {
		t.Fatalf(".env 读应被拒绝")
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/.env"); err != nil {
		t.Fatalf("只配 denyRead 时写应放行: %v", err)
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpExec, "/workspace/.git"); err == nil {
		t.Fatalf(".git 执行应被拒绝")
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpList, "/workspace"); err != nil {
		t.Fatalf("根列目录应放行: %v", err)
	}
}

// 3. 单段 glob:*.secret 只匹配根级文件,不跨段。
func TestGlobSingleSegment(t *testing.T) {
	svc, _ := newGuard(t, workspaceguard.Config{DenyRead: []string{"*.secret"}})
	if _, err := svc.ResolveHost(context.Background(), workspace.OpRead, "/workspace/a.secret"); err == nil {
		t.Fatalf("根级 *.secret 应命中")
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpRead, "/workspace/sub/a.secret"); err != nil {
		t.Fatalf("单段 glob 不应跨目录命中: %v", err)
	}
}

// 4. 空配置不改变行为;非法模式不影响其它路径。
func TestEmptyAndInvalidPatterns(t *testing.T) {
	svc, _ := newGuard(t, workspaceguard.Config{})
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/.git/config"); err != nil {
		t.Fatalf("空配置应放行: %v", err)
	}
	svc, _ = newGuard(t, workspaceguard.Config{DenyWrite: []string{"["}})
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/a"); err != nil {
		t.Fatalf("非法 glob 不应误伤其它路径: %v", err)
	}
}

// 5. 插件装饰器:provider 安装在链上,策略拒绝写;事件负载可序列化。
func TestPluginDecoration(t *testing.T) {
	root := t.TempDir()
	app := kernel.New(kernel.Options{})
	if _, err := app.Plugin(workspace.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	if _, err := app.Plugin(workspacelocal.Plugin(), kernel.NewConfig(map[string]any{
		"workspaces": []any{map[string]any{"id": "repo", "root": root}},
	})); err != nil {
		t.Fatalf("挂载 provider 失败: %v", err)
	}
	if _, err := app.Plugin(workspaceguard.Plugin(), kernel.NewConfig(map[string]any{
		"denyWrite": []any{".git"},
	})); err != nil {
		t.Fatalf("挂载 policy 失败: %v", err)
	}
	chain, err := kernel.Service[*workspace.Chain](app.Root(), workspace.ServiceName)
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	svc, err := chain.Get()
	if err != nil {
		t.Fatalf("取实现失败: %v", err)
	}
	if _, err := svc.ResolveHost(context.Background(), workspace.OpWrite, "/workspace/.git/config"); err == nil {
		t.Fatalf("策略装饰器未生效")
	}
	info, err := svc.Current(context.Background())
	if err != nil {
		t.Fatalf("非 ResolveHost 方法应透传: %v", err)
	}
	if data, err := json.Marshal(info); err != nil || strings.Contains(string(data), root) {
		t.Fatalf("Info 不应泄露根: %s err=%v", data, err)
	}
}
