package shellobserve_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	shellobserve "github.com/issueye/ada_go_v2/plugins/policy/shell-observe"
	shelllocal "github.com/issueye/ada_go_v2/plugins/provider/shell-local"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

type fakeShell struct {
	runErr error
	result seamshell.Result
}

func (f *fakeShell) Run(context.Context, seamshell.Command) (seamshell.Result, error) {
	return f.result, f.runErr
}
func (f *fakeShell) Stream(_ context.Context, _ seamshell.Command, _ func(seamshell.Chunk) error) (seamshell.Result, error) {
	return f.result, f.runErr
}
func (f *fakeShell) Env() seamshell.Env { return nil }

// 1. Run 成功记录 program/exit/bytes/plugin。
func TestRecordsRun(t *testing.T) {
	fake := &fakeShell{result: seamshell.Result{ExitCode: 0, Stdout: []byte("ok")}}
	var rec shellobserve.Record
	shell := shellobserve.New(fake, func(_ context.Context, r shellobserve.Record) { rec = r }, nil)
	ctx := kernel.WithPrincipal(context.Background(), "p1")
	if _, err := shell.Run(ctx, seamshell.Command{Program: "echo"}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if rec.Op != "run" || rec.Program != "echo" || rec.Bytes != 2 || rec.Plugin != "p1" {
		t.Fatalf("记录不正确: %+v", rec)
	}
}

// 2. 失败也记录。
func TestRecordsError(t *testing.T) {
	fake := &fakeShell{runErr: errors.New("boom")}
	var rec shellobserve.Record
	shell := shellobserve.New(fake, func(_ context.Context, r shellobserve.Record) { rec = r }, nil)
	if _, err := shell.Run(context.Background(), seamshell.Command{Program: "bad"}); err == nil {
		t.Fatalf("应返回错误")
	}
	if rec.Err != "boom" || rec.Program != "bad" {
		t.Fatalf("失败记录不正确: %+v", rec)
	}
}

// 3. Stream 记录 op=stream。
func TestRecordsStream(t *testing.T) {
	fake := &fakeShell{result: seamshell.Result{ExitCode: 1}}
	var rec shellobserve.Record
	shell := shellobserve.New(fake, func(_ context.Context, r shellobserve.Record) { rec = r }, nil)
	if _, err := shell.Stream(context.Background(), seamshell.Command{Program: "cat"}, func(seamshell.Chunk) error { return nil }); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if rec.Op != "stream" || rec.Program != "cat" {
		t.Fatalf("stream 记录不正确: %+v", rec)
	}
}

// 4. skipOps 跳过 run、保留 stream。
func TestSkipOps(t *testing.T) {
	fake := &fakeShell{}
	var ops []string
	shell := shellobserve.New(fake, func(_ context.Context, r shellobserve.Record) { ops = append(ops, r.Op) }, []string{"run"})
	_, _ = shell.Run(context.Background(), seamshell.Command{Program: "echo"})
	_, _ = shell.Stream(context.Background(), seamshell.Command{Program: "cat"}, func(seamshell.Chunk) error { return nil })
	if len(ops) != 1 || ops[0] != "stream" {
		t.Fatalf("run 应被跳过: %v", ops)
	}
}

// 5. 内核集成:消费者执行 go version → shell/observe 含身份。
func TestKernelIntegration(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境中没有 go 可执行文件")
	}
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	dir := t.TempDir()

	mount := func(name string, p kernel.Plugin, cfg map[string]any) {
		t.Helper()
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载 %s 失败: %v", name, err)
		}
	}
	mount("seam/shell", seamshell.Plugin(), nil)
	mount("shell-local", shelllocal.Plugin(), map[string]any{"workspace": dir})
	mount("shell-observe", shellobserve.Plugin(), nil)

	var payload map[string]any
	mount("listener", kernel.Func("listener", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		ctx.On(seamshell.EventObserve, func(_ context.Context, _ *kernel.Context, args []any) (any, error) {
			if len(args) > 0 {
				if m, ok := args[0].(map[string]any); ok {
					payload = m
				}
			}
			return nil, nil
		})
		return nil, nil
	}), nil)

	mount("consumer", kernel.Func("consumer", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		sh, err := chain.Get()
		if err != nil {
			return nil, err
		}
		_, err = sh.Run(ctx.Context(), seamshell.Command{Program: "go", Args: []string{"version"}})
		return nil, err
	}).WithInject("shell"), nil)

	if payload == nil || payload["op"] != "run" || payload["program"] != "go" || payload["plugin"] != "consumer" {
		t.Fatalf("shell/observe payload 不正确: %+v", payload)
	}
}
