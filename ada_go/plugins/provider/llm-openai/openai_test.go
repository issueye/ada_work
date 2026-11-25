package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	openai "github.com/issueye/ada_go_v2/plugins/provider/llm-openai"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
)

// TestRealOpenAI 是显式的真实端点冒烟测试:没有 ADA_LLM_ENDPOINT/ADA_LLM_API_KEY 时跳过。
// 它不进入默认 CI;用于本地验证 OpenAI 兼容端点(例如 http://127.0.0.1:3050/v1)。
func TestRealOpenAI(t *testing.T) {
	endpoint := os.Getenv("ADA_LLM_ENDPOINT")
	key := os.Getenv("ADA_LLM_API_KEY")
	model := os.Getenv("ADA_LLM_MODEL")
	if endpoint == "" || key == "" {
		t.Skip("未设置 ADA_LLM_ENDPOINT / ADA_LLM_API_KEY,跳过真实端点测试")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}

	p, err := openai.New(openai.Options{Endpoint: endpoint, Model: model, APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req := llm.Request{
		Model:     model,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: "只回复 ok"}}}},
		MaxTokens: 128,
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete 失败: %v", err)
	}
	if resp.Message.Text() == "" && reasoningOf(resp.Message) == "" {
		t.Fatalf("Complete 返回空内容: %+v", resp.Message)
	}
	if resp.Usage.OutputTokens == 0 && resp.Usage.InputTokens == 0 {
		t.Logf("端点未返回 usage,可接受: %+v", resp)
	}

	var deltas int
	streamResp, err := p.Stream(ctx, req, func(d llm.Delta) error {
		deltas++
		if d.Kind == llm.DeltaText && d.Text != "" {
			t.Logf("delta text: %q", d.Text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	if deltas == 0 {
		t.Fatalf("Stream 未产生增量")
	}
	if streamResp.Message.Text() == "" {
		t.Fatalf("Stream 返回空文本: %+v", streamResp.Message)
	}
}

func reasoningOf(m llm.Message) string {
	for _, b := range m.Content {
		if b.Kind == llm.BlockReasoning {
			return b.Text
		}
	}
	return ""
}

// 用 httptest 验证 wire 编解码,不依赖真实端点。
func TestWireCompleteAndStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if !body.Stream {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`data: {"choices":[{"delta":{"content":"h"}}]}`,
			`data: {"choices":[{"delta":{"content":"i"},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`,
			`data: [DONE]`,
		} {
			_, _ = fmt.Fprintln(w, line)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	p, err := openai.New(openai.Options{Endpoint: srv.URL, Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Kind: llm.BlockText, Text: "hi"}}}},
	})
	if err != nil || resp.Message.Text() != "hi" || resp.Usage.InputTokens != 2 {
		t.Fatalf("Complete 失败: %+v %v", resp, err)
	}
	var deltas int
	streamResp, err := p.Stream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas++
		return nil
	})
	if err != nil || streamResp.Message.Text() != "hi" || deltas < 3 {
		t.Fatalf("Stream 失败: %+v %v deltas=%d", streamResp, err, deltas)
	}
}
