package bridge_test

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// fakeEnv 是 shell seam 需要的受管环境占位。
type fakeEnv struct{}

func (fakeEnv) Get(string) (string, bool)   { return "", false }
func (fakeEnv) Snapshot() map[string]string { return nil }

// fakeShell 记录最后一次命令并返回固定结果,用于验证 ada:shell.exec 的结构化返回值。
type fakeShell struct {
	cmd seamshell.Command
}

func (f *fakeShell) Run(_ context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	f.cmd = cmd
	return seamshell.Result{
		Stdout: []byte("out"), Stderr: []byte("err"), ExitCode: 3,
		Truncated: true, Duration: 5 * time.Millisecond,
		Facts: seamshell.Facts{Enforced: true, Backend: "fake", Mode: "workspace-write"},
	}, nil
}

func (f *fakeShell) Stream(ctx context.Context, cmd seamshell.Command, _ func(seamshell.Chunk) error) (seamshell.Result, error) {
	return f.Run(ctx, cmd)
}

func (f *fakeShell) Env() seamshell.Env { return fakeEnv{} }

func mount(t *testing.T, app *kernel.App, p kernel.Plugin) *kernel.Fiber {
	t.Helper()
	f, err := app.Plugin(p, kernel.NewConfig(nil))
	if err != nil {
		t.Fatalf("挂载插件失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("插件未就绪: %v", err)
	}
	return f
}

//  1. 脚本工具返回 {status, content, details, terminate} 时,必须映射成真实工具结果;
//     普通字符串/对象返回值保持旧契约。
func TestJSToolResultMapping(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = {
  tool: (spec: any, handler: (args: any) => any) => void
}
export function apply(ctx: Ctx) {
  ctx.tool({name: "mapped", timeoutMs: 1234, input: {type: "object", properties: {}}}, (args: any) => ({
    status: "error",
    content: [{kind: "text", text: "bad " + (args && args.x ? args.x : "")}],
    details: {code: "E1"},
    terminate: true,
  }))
  ctx.tool({name: "plain"}, () => "hello")
}`,
	})

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount(t, app, tools.Plugin())

	p, err := bridge.New(bridge.Options{
		Name: "js-tools", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	reg, err := kernel.Service[*tools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	list, err := reg.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var mappedTimeout time.Duration
	for _, item := range list {
		if item.Name == "mapped" {
			mappedTimeout = item.Timeout
		}
	}
	if mappedTimeout != 1234*time.Millisecond {
		t.Fatalf("timeoutMs 未映射到 ToolDef.Timeout: %s", mappedTimeout)
	}

	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "mapped", Args: json.RawMessage(`{"x":"y"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != tools.StatusError || !res.Terminate {
		t.Fatalf("结构化结果未映射: %+v", res)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "bad y" {
		t.Fatalf("content 不正确: %+v", res.Content)
	}
	var details map[string]any
	if err := json.Unmarshal(res.Details, &details); err != nil || details["code"] != "E1" {
		t.Fatalf("details 不正确: %s %v", res.Details, err)
	}

	plain, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "plain"})
	if err != nil || plain.Status != tools.StatusOK || len(plain.Content) != 1 || plain.Content[0].Text != "hello" {
		t.Fatalf("旧字符串返回值契约被破坏: %+v %v", plain, err)
	}
}

// 2. ada:shell.exec 返回 stdout/stderr/exitCode/truncated/facts,并透传 timeout/dir/args。
func TestJSShellExecStructured(t *testing.T) {
	art := buildArtifact(t, map[string]string{
		"plugin.ts": `import { exec, platform } from "ada:shell"
type Ctx = { tool: (spec: any, handler: (args: any) => any) => void }
export function apply(ctx: Ctx) {
  ctx.tool({name: "exec_probe"}, () => {
    const r = exec("fake", ["arg"], "/workspace/sub", {timeoutMs: 1500})
    return {status: "ok", content: JSON.stringify({r, os: platform()})}
  })
}`,
	})

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount(t, app, seamshell.Plugin())
	mount(t, app, tools.Plugin())

	fake := &fakeShell{}
	mount(t, app, kernel.Func("fake-shell", func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamshell.Chain](ctx, "shell")
		if err != nil {
			return nil, err
		}
		return chain.Install(fake)
	}).WithInject("shell"))

	p, err := bridge.New(bridge.Options{
		Name: "js-shell", Artifact: art,
		Inject: kernel.Inject{Names: []string{"tools", "shell"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	reg, err := kernel.Service[*tools.Registry](app.Root(), "tools")
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "exec_probe"})
	if err != nil || res.Status != tools.StatusOK {
		t.Fatalf("exec_probe 调用失败: %+v %v", res, err)
	}
	if fake.cmd.Program != "fake" || len(fake.cmd.Args) != 1 || fake.cmd.Args[0] != "arg" {
		t.Fatalf("命令参数未透传: %+v", fake.cmd)
	}
	if fake.cmd.Dir != "/workspace/sub" || fake.cmd.Timeout != 1500*time.Millisecond {
		t.Fatalf("dir/timeout 未透传: %+v", fake.cmd)
	}
	var payload struct {
		R struct {
			Stdout     string `json:"stdout"`
			Stderr     string `json:"stderr"`
			ExitCode   int    `json:"exitCode"`
			Truncated  bool   `json:"truncated"`
			DurationMs int64  `json:"durationMs"`
			Enforced   bool   `json:"enforced"`
			Backend    string `json:"backend"`
			Mode       string `json:"mode"`
		} `json:"r"`
		OS string `json:"os"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &payload); err != nil {
		t.Fatalf("结构化结果不是合法 JSON: %v", err)
	}
	if payload.R.Stdout != "out" || payload.R.Stderr != "err" || payload.R.ExitCode != 3 || !payload.R.Truncated {
		t.Fatalf("shell 结果字段不正确: %+v", payload.R)
	}
	if !payload.R.Enforced || payload.R.Backend != "fake" || payload.R.Mode != "workspace-write" {
		t.Fatalf("shell facts 不正确: %+v", payload.R)
	}
	if payload.OS != runtime.GOOS {
		t.Fatalf("platform() 应为 %s,实际 %s", runtime.GOOS, payload.OS)
	}
}
