package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestChildProcessPermission(t *testing.T) {
	art, err := compile.Build("child", "plugin.ts", map[string]string{
		"plugin.ts": `
import cp from "node:child_process";
export function run(file: string): string {
  try { return String(cp.execFileSync(file, ["--version"])) } catch (e: any) { return "err:" + String((e && e.message) || e) }
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	var calls []string
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			ShellExec: func(_ context.Context, program string, _ []string, _ string, _ string) (string, error) {
				calls = append(calls, program)
				return `{"stdout":"ok","stderr":"","exitCode":0}`, nil
			},
		},
		Permissions: jsrt.Permissions{Child: jsrt.ChildPermission{
			Enabled: true,
			Allow:   []string{"git"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	got, err := rt.Call(ctx, "run", "git")
	if err != nil || got != "ok" {
		t.Fatalf("允许的子进程失败: got=%v err=%v", got, err)
	}
	got, err = rt.Call(ctx, "run", "bash")
	if err != nil {
		t.Fatalf("被拒的子进程不应变成宿主错误: %v", err)
	}
	if s, _ := got.(string); strings.HasPrefix(s, "ok") {
		t.Fatalf("未授权命令应被拒绝: %v", got)
	}
	if len(calls) != 1 || calls[0] != "git" {
		t.Fatalf("未授权命令不应调用 Host.ShellExec: %v", calls)
	}
}
