package sandboxnone_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	sandboxnone "github.com/issueye/ada_go_v2/plugins/provider/sandbox-none"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// 1. 三模式 × 操作/路径的判定矩阵。
func TestResolveMatrix(t *testing.T) {
	cases := []struct {
		name        string
		mode        seamsandbox.Mode
		req         seamsandbox.Request
		allow       bool
		escalatable bool
	}{
		{"只读读", seamsandbox.ModeReadOnly, seamsandbox.Request{Op: seamsandbox.OpFSRead, Path: "/workspace/a"}, true, false},
		{"只读写", seamsandbox.ModeReadOnly, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/workspace/a"}, false, true},
		{"只读shell", seamsandbox.ModeReadOnly, seamsandbox.Request{Op: seamsandbox.OpShellExec, Program: "echo"}, true, false},
		{"工作区写工作区", seamsandbox.ModeWorkspaceWrite, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/workspace/a"}, true, false},
		{"工作区写tmp", seamsandbox.ModeWorkspaceWrite, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/tmp/a"}, true, false},
		{"工作区写插件目录", seamsandbox.ModeWorkspaceWrite, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/plugin/x/a"}, false, true},
		{"危险模式写工作区", seamsandbox.ModeDanger, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/workspace/a"}, true, false},
		{"危险模式写插件目录", seamsandbox.ModeDanger, seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/plugin/x/a"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := sandboxnone.New(tc.mode)
			if err != nil {
				t.Fatalf("创建失败: %v", err)
			}
			d, err := n.Resolve(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("判定失败: %v", err)
			}
			if d.Allow != tc.allow || d.Escalatable != tc.escalatable {
				t.Fatalf("判定不正确: %+v", d)
			}
			if !d.Allow && len(d.Reasons) == 0 {
				t.Fatalf("拒绝必须带可展示的理由")
			}
		})
	}
}

// 2. Facts 必须如实报告「未隔离」。
func TestFacts(t *testing.T) {
	n, err := sandboxnone.New(seamsandbox.ModeWorkspaceWrite)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	f := n.Facts(context.Background(), seamsandbox.Request{})
	if f.Enforced {
		t.Fatalf("policy-only 后端不应声称已隔离")
	}
	if f.Backend != sandboxnone.ProviderName || f.Mode != seamsandbox.ModeWorkspaceWrite {
		t.Fatalf("Facts 不正确: %+v", f)
	}
	if len(f.Roots) == 0 {
		t.Fatalf("Facts 应列出允许根")
	}
	infos := n.Backends()
	if len(infos) != 1 || infos[0].Probe != nil || infos[0].Kind != "policy-only" {
		t.Fatalf("Backends 不正确: %+v", infos)
	}
}

// 3. 非法模式在构造期拒绝。
func TestInvalidMode(t *testing.T) {
	if _, err := sandboxnone.New("nope"); err == nil || !seamerr.Is(err, seamerr.ErrInvalid) {
		t.Fatalf("非法模式应 EINVALID,实际 %v", err)
	}
}

// 4. 经内核装配:seam + provider,消费者按默认后端判定。
func TestKernelIntegration(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	if _, err := app.Plugin(seamsandbox.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	if _, err := app.Plugin(sandboxnone.Plugin(), kernel.NewConfig(map[string]any{"mode": string(seamsandbox.ModeWorkspaceWrite)})); err != nil {
		t.Fatalf("挂载 provider 失败: %v", err)
	}

	var (
		allowWorkspace bool
		denyPlugin     bool
	)
	consumer := kernel.Func("consumer", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*seamsandbox.Registry](ctx, "sandbox")
		if err != nil {
			return nil, err
		}
		sb, err := registry.Get("")
		if err != nil {
			return nil, err
		}
		d1, err := sb.Resolve(context.Background(), seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/workspace/a"})
		if err != nil {
			return nil, err
		}
		allowWorkspace = d1.Allow
		d2, err := sb.Resolve(context.Background(), seamsandbox.Request{Op: seamsandbox.OpFSWrite, Path: "/plugin/x/a"})
		if err != nil {
			return nil, err
		}
		denyPlugin = !d2.Allow
		return nil, nil
	}).WithInject("sandbox")
	if _, err := app.Plugin(consumer, kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载消费者失败: %v", err)
	}
	if !allowWorkspace || !denyPlugin {
		t.Fatalf("判定结果不正确: allowWorkspace=%v denyPlugin=%v", allowWorkspace, denyPlugin)
	}
}

// 5. 无 provider 时 Get 返回 EUNIMPLEMENTED。
func TestRegistryWithoutProvider(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamsandbox.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam 失败: %v", err)
	}
	registry, err := kernel.Service[*seamsandbox.Registry](app.Root(), "sandbox")
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	if _, err := registry.Get(""); err == nil {
		t.Fatalf("无后端时应返回错误")
	}
}
