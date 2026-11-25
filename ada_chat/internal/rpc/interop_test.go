// 端到端互操作测试：用本包的 RPC 客户端启动并连接真实的 ada-rpc-agent。
//
// 默认跳过；配置下面三个环境变量后才真正拉起外部进程：
//
//	ADA_INTEROP_AGENT   ada-rpc-agent(.exe) 的绝对路径
//	ADA_INTEROP_PRESET  examples/agent/agent.preset.json 的绝对路径
//	ADA_INTEROP_BASE    ada_go_v2 仓库根（preset 里相对路径的基目录）
//
// 它验证两件事：协议/方法/事件仍按 seam RPC v1 工作，以及宿主为每个 session
// 传入的身份（botName + instruction）会替换进程级默认身份段。
package rpc_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http"
	"net/http/httptest"

	"ada_chat/internal/rpc"
)

type interopFiles struct {
	agent  string
	preset string
	base   string
}

func interopFilesFromEnv(t *testing.T) interopFiles {
	t.Helper()
	files := interopFiles{
		agent:  os.Getenv("ADA_INTEROP_AGENT"),
		preset: os.Getenv("ADA_INTEROP_PRESET"),
		base:   os.Getenv("ADA_INTEROP_BASE"),
	}
	if files.agent == "" || files.preset == "" || files.base == "" {
		t.Skip("未配置 ADA_INTEROP_AGENT / ADA_INTEROP_PRESET / ADA_INTEROP_BASE")
	}
	if _, err := os.Stat(files.agent); err != nil {
		t.Fatalf("ada-rpc-agent 不存在: %v", err)
	}
	if _, err := os.Stat(files.preset); err != nil {
		t.Fatalf("preset 不存在: %v", err)
	}
	return files
}

// startAgent 启动 agent 进程并返回它的地址、token 与日志文件。
func startAgent(t *testing.T, files interopFiles, extra []string) (rpc.Address, []byte, string) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := randomHex(6)
	if err != nil {
		t.Fatal(err)
	}
	var addr rpc.Address
	if runtime.GOOS == "windows" {
		addr = rpc.Address{Network: rpc.Pipe, Name: "ada-interop-" + raw}
	} else {
		addr = rpc.Address{Network: rpc.Unix, Name: filepath.Join(workspace, "rpc.sock")}
	}
	tok, err := randomHex(16)
	if err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(workspace, "agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	env := make([]string, 0, len(os.Environ())+12)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ADA_BOT_") || strings.HasPrefix(kv, "ADA_GO_AGENT_") ||
			strings.HasPrefix(kv, "ADA_LLM_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"ADA_BOT_ADDR="+addr.String(),
		"ADA_BOT_TOKEN="+tok,
		"ADA_BOT_CLIENT=ada-chat",
		"ADA_GO_AGENT_PRESET="+files.preset,
		"ADA_GO_AGENT_BASE="+files.base,
		"ADA_BOT_NAME=互操作机器人",
		"ADA_BOT_WORKSPACE="+workspace,
	)
	env = append(env, extra...)

	cmd := exec.Command(files.agent)
	cmd.Dir = workspace
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 agent: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logPath); err == nil {
				t.Logf("agent 日志:\n%s", strings.TrimSpace(string(b)))
			}
		}
	})
	return addr, []byte(tok), logPath
}

// dialAgent 重试直到 agent 的监听器就绪。
func dialAgent(t *testing.T, addr rpc.Address, token []byte) rpc.Peer {
	t.Helper()
	svc := rpc.NewService()
	if err := rpc.RegisterNet(svc); err != nil {
		t.Fatal(err)
	}
	if err := rpc.RegisterPipe(svc); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		peer, err := svc.Dial(ctx, addr, rpc.DialOptions{
			Client: "ada-chat", Auth: rpc.Auth{Mode: rpc.AuthToken, Token: token}, Timeout: 2 * time.Second,
		})
		cancel()
		if err == nil {
			t.Cleanup(func() { _ = peer.Close() })
			return peer
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("连接 agent 超时: %v", lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func subscribe(t *testing.T, p rpc.Peer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Subscribe(ctx, "chat/*"); err != nil {
		t.Fatalf("订阅 chat/*: %v", err)
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TestInteropReplayChat 用 replay preset 跑一轮，验证握手、bot.info、流式与工具事件。
func TestInteropReplayChat(t *testing.T) {
	files := interopFilesFromEnv(t)
	addr, token, _ := startAgent(t, files, nil)
	p := dialAgent(t, addr, token)
	subscribe(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var info struct {
		Model     string `json:"model"`
		Workspace string `json:"workspace"`
	}
	if err := p.Call(ctx, "bot.info", nil, &info); err != nil {
		t.Fatalf("bot.info: %v", err)
	}
	if info.Model != "replay" || info.Workspace == "" {
		t.Errorf("bot.info = %+v", info)
	}

	var events struct {
		mu        sync.Mutex
		deltas    int
		tools     int
		done      bool
		text      string
		errReason string
	}
	events.mu.Lock()
	events.deltas, events.tools, events.done = 0, 0, false
	events.mu.Unlock()
	go func() {
		for ev := range p.Events() {
			events.mu.Lock()
			switch ev.Topic {
			case "chat/delta":
				var d struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(ev.Data, &d) == nil {
					events.deltas++
					events.text += d.Text
				}
			case "chat/tool":
				events.tools++
			case "chat/error":
				var d struct {
					Reason string `json:"reason"`
				}
				if json.Unmarshal(ev.Data, &d) == nil {
					events.errReason = d.Reason
				}
			case "chat/done":
				events.done = true
			}
			events.mu.Unlock()
		}
	}()

	var res struct {
		Text      string `json:"text"`
		ToolCalls int    `json:"toolCalls"`
	}
	if err := p.Call(ctx, "chat.send", map[string]any{
		"turn": 1, "text": "读取 a.txt 并总结", "botId": 1, "botName": "互操作机器人",
	}, &res); err != nil {
		t.Fatalf("chat.send: %v", err)
	}
	if !strings.Contains(res.Text, "hello") {
		t.Errorf("回答里没有 hello: %q", res.Text)
	}
	time.Sleep(300 * time.Millisecond)
	events.mu.Lock()
	deltas, tools, done, streamed := events.deltas, events.tools, events.done, events.text
	events.mu.Unlock()
	if deltas == 0 || !done {
		t.Errorf("事件不完整: deltas=%d tools=%d done=%v", deltas, tools, done)
	}
	if res.ToolCalls > 0 && tools == 0 {
		t.Error("chat.send 报告了工具调用，但没有收到 chat/tool 事件")
	}
	if !strings.Contains(streamed+res.Text, "hello") {
		t.Errorf("流式增量与结果都没有 hello: %q", streamed)
	}

	// 第二个机器人复用进程级 replay 游标，脚本耗尽必然失败；runtime 应先发 chat/error，
	// 宿主据此拿到可读原因，而不是只看到 EINTERNAL。
	var res2 struct {
		Text string `json:"text"`
	}
	if err := p.Call(ctx, "chat.send", map[string]any{
		"turn": 2, "text": "再来一次", "botId": 2, "botName": "第二机器人",
	}, &res2); err == nil {
		t.Logf("第二次 chat.send 意外成功: %q", res2.Text)
	}
	deadline := time.Now().Add(2 * time.Second)
	var reason string
	for time.Now().Before(deadline) {
		events.mu.Lock()
		reason = events.errReason
		events.mu.Unlock()
		if reason != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reason == "" {
		t.Error("失败的轮次没有收到 chat/error 事件")
	} else {
		t.Logf("chat/error: %s", reason)
	}
}

// TestInteropApproval 用 approval preset 验证宿主审批往返：
// agent 发 chat/approval，测试充当宿主用 chat.approve 允许，工具随后执行成功。
func TestInteropApproval(t *testing.T) {
	files := interopFilesFromEnv(t)
	preset := os.Getenv("ADA_INTEROP_APPROVAL_PRESET")
	if preset == "" {
		t.Skip("未配置 ADA_INTEROP_APPROVAL_PRESET")
	}
	if _, err := os.Stat(preset); err != nil {
		t.Fatalf("approval preset 不存在: %v", err)
	}
	files.preset = preset

	addr, token, _ := startAgent(t, files, nil)
	p := dialAgent(t, addr, token)
	subscribe(t, p)

	type approval struct {
		ID        string `json:"id"`
		Turn      int    `json:"turn"`
		Source    string `json:"source"`
		Op        string `json:"op"`
		Target    string `json:"target"`
		Reason    string `json:"reason"`
		TimeoutMS int64  `json:"timeoutMs"`
	}
	requests := make(chan approval, 1)
	go func() {
		for ev := range p.Events() {
			if ev.Topic != "chat/approval" {
				continue
			}
			var req approval
			if json.Unmarshal(ev.Data, &req) != nil || req.ID == "" {
				continue
			}
			requests <- req
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			// 测试充当桌面宿主：允许这一次工具调用。
			_ = p.Call(ctx, "chat.approve", map[string]any{
				"id": req.ID, "allow": true, "reason": "interop test allow",
			}, nil)
			cancel()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res struct {
		Text      string `json:"text"`
		ToolCalls int    `json:"toolCalls"`
	}
	if err := p.Call(ctx, "chat.send", map[string]any{
		"turn": 1, "text": "读取 a.txt 并总结", "botId": 1, "botName": "互操作机器人",
	}, &res); err != nil {
		t.Fatalf("chat.send: %v", err)
	}
	if !strings.Contains(res.Text, "hello") {
		t.Errorf("审批放行后回答里没有 hello: %q", res.Text)
	}
	select {
	case req := <-requests:
		if req.Turn != 1 || req.ID == "" || req.Target == "" {
			t.Errorf("审批请求字段不完整: %+v", req)
		}
		if req.TimeoutMS <= 0 {
			t.Errorf("审批请求缺少 timeoutMs: %+v", req)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到 chat/approval 事件")
	}
}

// TestInteropSessionIsolation 验证 sessionId：同一个机器人的不同对话拿到独立的
// agent session，新对话的第一轮不会带上旧对话的模型上下文。
func TestInteropSessionIsolation(t *testing.T) {
	files := interopFilesFromEnv(t)

	var mu sync.Mutex
	messageCounts := make([]int, 0, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			mu.Lock()
			messageCounts = append(messageCounts, len(body.Messages))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	addr, token, _ := startAgent(t, files, []string{
		"ADA_LLM_PROVIDER=openai",
		"ADA_LLM_ENDPOINT=" + srv.URL,
		"ADA_LLM_MODEL=interop-model",
		"ADA_LLM_API_KEY=interop-key",
	})
	p := dialAgent(t, addr, token)
	subscribe(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	send := func(turn int, sessionID, text string) {
		t.Helper()
		if err := p.Call(ctx, "chat.send", map[string]any{
			"turn": turn, "text": text, "botId": 1, "botName": "互操作机器人",
			"sessionId": sessionID,
		}, nil); err != nil {
			t.Fatalf("chat.send(%s): %v", sessionID, err)
		}
	}
	send(1, "conv-a", "第一句")
	send(2, "conv-a", "第二句")
	send(3, "conv-b", "新对话第一句")

	mu.Lock()
	defer mu.Unlock()
	if len(messageCounts) != 3 {
		t.Fatalf("请求次数 = %d, want 3 (%v)", len(messageCounts), messageCounts)
	}
	// system+user → 新会话；system+user+assistant+user → 同会话第二轮；再新会话回到 2。
	if messageCounts[0] != 2 {
		t.Errorf("conv-a 第一轮消息数 = %d, want 2", messageCounts[0])
	}
	if messageCounts[1] != 4 {
		t.Errorf("conv-a 第二轮消息数 = %d, want 4", messageCounts[1])
	}
	if messageCounts[2] != 2 {
		t.Errorf("conv-b 第一轮消息数 = %d, want 2（不应带旧会话上下文）", messageCounts[2])
	}
}

// TestInteropPerBotPersona 用 mock OpenAI 端点验证每 session 的身份替换：
// 同一个进程里的第二个机器人应当看到自己的名字与指令，而不是进程级默认身份。
func TestInteropPerBotPersona(t *testing.T) {
	files := interopFilesFromEnv(t)

	var mu sync.Mutex
	prompts := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			system := ""
			for _, m := range body.Messages {
				if m.Role == "system" {
					system = m.Content
					break
				}
			}
			mu.Lock()
			prompts = append(prompts, system)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	addr, token, _ := startAgent(t, files, []string{
		"ADA_LLM_PROVIDER=openai",
		"ADA_LLM_ENDPOINT=" + srv.URL,
		"ADA_LLM_MODEL=interop-model",
		"ADA_LLM_API_KEY=interop-key",
	})
	p := dialAgent(t, addr, token)
	subscribe(t, p)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const firstInstruction = "第一机器人只回答一个词"
	const secondInstruction = "第二机器人只读 a.txt"
	if err := p.Call(ctx, "chat.send", map[string]any{
		"turn": 1, "text": "你好", "botId": 1, "botName": "第一机器人", "instruction": firstInstruction,
	}, nil); err != nil {
		t.Fatalf("chat.send#1: %v", err)
	}
	if err := p.Call(ctx, "chat.send", map[string]any{
		"turn": 2, "text": "再来", "botId": 2, "botName": "第二机器人", "instruction": secondInstruction,
	}, nil); err != nil {
		t.Fatalf("chat.send#2: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(prompts) < 2 {
		t.Fatalf("只捕获到 %d 个系统提示", len(prompts))
	}
	if !strings.Contains(prompts[0], "第一机器人") || !strings.Contains(prompts[0], firstInstruction) {
		t.Errorf("第一个机器人的身份不对: %q", prompts[0])
	}
	if !strings.Contains(prompts[1], "第二机器人") || !strings.Contains(prompts[1], secondInstruction) {
		t.Errorf("第二个机器人的身份不对: %q", prompts[1])
	}
	if strings.Contains(prompts[1], "第一机器人") || strings.Contains(prompts[1], firstInstruction) {
		t.Errorf("第二个机器人仍然看到进程级默认身份: %q", prompts[1])
	}
}
