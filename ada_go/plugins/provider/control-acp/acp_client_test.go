package controlacp_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type call struct {
	Method string
	Params map[string]any
}

// TestACPClientCapabilities 验证 client 上报的 fs/terminal 能力与反向调用参数。
func TestACPClientCapabilities(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := jsonrpc2.New(ctx, a)
	t.Cleanup(func() { _ = client.Close() })
	server := jsonrpc2.New(ctx, b)
	ep := controlacp.New(server, control.New(&fakeControl{}))
	t.Cleanup(func() { _ = ep.Close() })

	if caps := ep.Capabilities(); caps.ReadTextFile || caps.WriteTextFile || caps.Terminal {
		t.Fatalf("initialize 前能力应为空: %+v", caps)
	}
	if _, err := ep.ReadTextFile(ctx, "s1", "/workspace/a.txt", 0, 0); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("未声明能力应 EUNIMPLEMENTED: %v", err)
	}

	calls := make(chan call, 8)
	handle := func(method string, result any) {
		client.Handle(method, func(_ context.Context, params json.RawMessage) (any, error) {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			calls <- call{Method: method, Params: p}
			return result, nil
		})
	}
	handle("fs/read_text_file", map[string]any{"content": "hello"})
	handle("fs/write_text_file", nil)
	handle("terminal/create", map[string]any{"terminalId": "term-1"})
	handle("terminal/output", map[string]any{"output": "out", "truncated": true})
	handle("terminal/wait_for_exit", map[string]any{"exitCode": 0})
	handle("terminal/kill", nil)
	handle("terminal/release", nil)

	var initRes map[string]any
	if err := client.Call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": true,
		},
	}, &initRes); err != nil {
		t.Fatalf("initialize 失败: %v", err)
	}
	if caps := ep.Capabilities(); !caps.ReadTextFile || !caps.WriteTextFile || !caps.Terminal {
		t.Fatalf("能力未记录: %+v", caps)
	}

	content, err := ep.ReadTextFile(ctx, "s1", "/workspace/a.txt", 2, 10)
	if err != nil || content != "hello" {
		t.Fatalf("ReadTextFile 不符: %q err=%v", content, err)
	}
	req := <-calls
	if req.Method != "fs/read_text_file" || req.Params["sessionId"] != "s1" ||
		req.Params["path"] != "/workspace/a.txt" || req.Params["line"] != float64(2) ||
		req.Params["limit"] != float64(10) {
		t.Fatalf("fs/read_text_file 参数不符: %+v", req)
	}

	if err := ep.WriteTextFile(ctx, "s1", "/workspace/a.txt", "hi"); err != nil {
		t.Fatalf("WriteTextFile 失败: %v", err)
	}
	req = <-calls
	if req.Method != "fs/write_text_file" || req.Params["content"] != "hi" {
		t.Fatalf("fs/write_text_file 参数不符: %+v", req)
	}

	term, err := ep.CreateTerminal(ctx, "s1", acpseam.TerminalRequest{
		Command: "go", Args: []string{"test"}, Cwd: "/workspace",
	})
	if err != nil || term != "term-1" {
		t.Fatalf("CreateTerminal 不符: %q err=%v", term, err)
	}
	req = <-calls
	if req.Method != "terminal/create" || req.Params["command"] != "go" || req.Params["cwd"] != "/workspace" {
		t.Fatalf("terminal/create 参数不符: %+v", req)
	}
	if args, ok := req.Params["args"].([]any); !ok || len(args) != 1 || args[0] != "test" {
		t.Fatalf("terminal/create args 不符: %+v", req.Params["args"])
	}

	out, err := ep.TerminalOutput(ctx, "s1", term)
	if err != nil || out.Output != "out" || !out.Truncated {
		t.Fatalf("TerminalOutput 不符: %+v err=%v", out, err)
	}
	if req = <-calls; req.Method != "terminal/output" || req.Params["terminalId"] != "term-1" {
		t.Fatalf("terminal/output 参数不符: %+v", req)
	}

	exit, err := ep.WaitForExit(ctx, "s1", term)
	if err != nil || exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Fatalf("WaitForExit 不符: %+v err=%v", exit, err)
	}
	<-calls
	if err := ep.KillTerminal(ctx, "s1", term); err != nil {
		t.Fatalf("KillTerminal 失败: %v", err)
	}
	<-calls
	if err := ep.ReleaseTerminal(ctx, "s1", term); err != nil {
		t.Fatalf("ReleaseTerminal 失败: %v", err)
	}
	<-calls
}
