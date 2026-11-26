package acphelper_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/issueye/ada_go_v2/platform/kernel"
	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	seamtools "github.com/issueye/ada_go_v2/plugins/seam/tools"
	acphelper "github.com/issueye/ada_go_v2/plugins/tool/acp-helper"
)

// fakeClient 记录调用并按配置返回结果。
type fakeClient struct {
	mu       sync.Mutex
	caps     acpseam.Capabilities
	calls    []string
	content  string
	written  string
	output   acpseam.TerminalOutput
	exit     acpseam.ExitStatus
	waitErr  error
	block    bool
	killed   bool
	released bool
}

func (f *fakeClient) record(method string) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
}

func (f *fakeClient) Capabilities() acpseam.Capabilities { return f.caps }

func (f *fakeClient) ReadTextFile(_ context.Context, sessionID, path string, line, limit int) (string, error) {
	f.record("read")
	return f.content, nil
}

func (f *fakeClient) WriteTextFile(_ context.Context, sessionID, path, content string) error {
	f.record("write")
	f.mu.Lock()
	f.written = content
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) CreateTerminal(_ context.Context, sessionID string, req acpseam.TerminalRequest) (string, error) {
	f.record("create")
	return "term-1", nil
}

func (f *fakeClient) TerminalOutput(context.Context, string, string) (acpseam.TerminalOutput, error) {
	f.record("output")
	return f.output, nil
}

func (f *fakeClient) WaitForExit(ctx context.Context, _ string, _ string) (acpseam.ExitStatus, error) {
	f.record("wait")
	if f.block {
		<-ctx.Done()
		return acpseam.ExitStatus{}, ctx.Err()
	}
	return f.exit, f.waitErr
}

func (f *fakeClient) KillTerminal(context.Context, string, string) error {
	f.record("kill")
	f.mu.Lock()
	f.killed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) ReleaseTerminal(context.Context, string, string) error {
	f.record("release")
	f.mu.Lock()
	f.released = true
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) hasCall(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method {
			return true
		}
	}
	return false
}

func (f *fakeClient) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeClient) writtenText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written
}

func (f *fakeClient) isKilled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killed
}

func (f *fakeClient) isReleased() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// mount 挂载 seam-tools、seam-acp、acp-helper;client 可为 nil(没有连接)。
func mount(t *testing.T, client acpseam.Client) *seamtools.Registry {
	t.Helper()
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	mountPlugin := func(p kernel.Plugin) {
		t.Helper()
		f, err := app.Plugin(p, kernel.NewConfig(nil))
		if err != nil {
			t.Fatalf("挂载 %s 失败: %v", p.Name(), err)
		}
		if err := f.Ready(context.Background()); err != nil {
			t.Fatalf("%s 未就绪: %v", p.Name(), err)
		}
	}
	mountPlugin(seamtools.Plugin())
	mountPlugin(acpseam.Plugin())
	if client != nil {
		raw, err := app.Root().Service("acp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.(*acpseam.Registry).Set(client); err != nil {
			t.Fatal(err)
		}
	}
	mountPlugin(acphelper.Plugin())

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*seamtools.Registry)
}

func invoke(t *testing.T, reg *seamtools.Registry, name, session string, args map[string]any) seamtools.Result {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(context.Background(), seamtools.CallInfo{
		Name: name, SessionID: session, Args: data,
	})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	return res
}

func text(res seamtools.Result) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].Text
}

func TestACPHelperReadWrite(t *testing.T) {
	fake := &fakeClient{
		caps:    acpseam.Capabilities{ReadTextFile: true, WriteTextFile: true},
		content: "hello",
	}
	reg := mount(t, fake)
	if res := invoke(t, reg, "acp_read_file", "s1", map[string]any{"path": "/a.txt"}); res.Status != seamtools.StatusOK || text(res) != "hello" {
		t.Fatalf("读取结果不符: %+v", res)
	}
	if res := invoke(t, reg, "acp_write_file", "s1", map[string]any{"path": "/a.txt", "content": "hi"}); res.Status != seamtools.StatusOK {
		t.Fatalf("写入结果不符: %+v", res)
	}
	if fake.writtenText() != "hi" {
		t.Fatalf("写入内容不符: %q", fake.writtenText())
	}
}

func TestACPHelperDeniedWithoutCapabilityOrSession(t *testing.T) {
	fake := &fakeClient{}
	reg := mount(t, fake)
	if res := invoke(t, reg, "acp_read_file", "s1", map[string]any{"path": "/a.txt"}); res.Status != seamtools.StatusDenied {
		t.Fatalf("未声明能力应拒绝: %+v", res)
	}
	if fake.hasCall("read") {
		t.Fatal("未声明能力时不应发请求")
	}

	fake.caps = acpseam.Capabilities{ReadTextFile: true}
	if res := invoke(t, reg, "acp_read_file", "", map[string]any{"path": "/a.txt"}); res.Status != seamtools.StatusDenied {
		t.Fatalf("缺少 sessionId 应拒绝: %+v", res)
	}

	// 没有连接:Registry 为空。
	reg = mount(t, nil)
	if res := invoke(t, reg, "acp_run", "s1", map[string]any{"command": "echo"}); res.Status != seamtools.StatusDenied {
		t.Fatalf("没有连接应拒绝: %+v", res)
	}
}

func TestACPHelperRun(t *testing.T) {
	fake := &fakeClient{
		caps:   acpseam.Capabilities{Terminal: true},
		output: acpseam.TerminalOutput{Output: "ok"},
	}
	reg := mount(t, fake)
	res := invoke(t, reg, "acp_run", "s1", map[string]any{"command": "echo", "args": []string{"hi"}})
	if res.Status != seamtools.StatusOK || !strings.Contains(text(res), "ok") {
		t.Fatalf("run 结果不符: %+v", res)
	}
	for _, want := range []string{"create", "wait", "output", "release"} {
		if !fake.hasCall(want) {
			t.Fatalf("缺少调用 %s: %+v", want, fake.snapshot())
		}
	}
}

func TestACPHelperRunExitCodeAndTimeout(t *testing.T) {
	code := 2
	fake := &fakeClient{
		caps:   acpseam.Capabilities{Terminal: true},
		exit:   acpseam.ExitStatus{ExitCode: &code},
		output: acpseam.TerminalOutput{Output: "boom"},
	}
	reg := mount(t, fake)
	if res := invoke(t, reg, "acp_run", "s1", map[string]any{"command": "false"}); res.Status != seamtools.StatusError {
		t.Fatalf("非零退出应是失败: %+v", res)
	}

	blocking := &fakeClient{caps: acpseam.Capabilities{Terminal: true}, block: true}
	reg = mount(t, blocking)
	res := invoke(t, reg, "acp_run", "s1", map[string]any{"command": "sleep", "timeoutSeconds": 1})
	if res.Status != seamtools.StatusTimeout {
		t.Fatalf("超时应是 StatusTimeout: %+v", res)
	}
	if !blocking.isKilled() || !blocking.isReleased() {
		t.Fatalf("超时后应 kill 并 release: killed=%v released=%v", blocking.isKilled(), blocking.isReleased())
	}
}
