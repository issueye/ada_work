package replay_test

import (
	"context"
	"encoding/json"
	"testing"

	replay "github.com/issueye/ada_go_v2/plugins/provider/llm-replay"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

func TestReplayCompleteStreamAndToolCall(t *testing.T) {
	steps := []replay.Step{
		{Response: llm.Response{
			Message:   llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "你好"}}},
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Args: json.RawMessage(`{"path":"a.txt"}`)}},
			Finish:    llm.FinishToolCalls,
			Usage:     llm.Usage{InputTokens: 10, OutputTokens: 5},
		}},
		{Response: llm.Response{
			Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "文件内容是 hello"}}},
			Finish:  llm.FinishStop,
		}},
	}
	p := replay.New(steps, []llm.ModelInfo{{ID: "replay", ContextWindow: 8192}}, "replay")

	models, err := p.Models(context.Background())
	if err != nil || len(models) != 1 {
		t.Fatalf("Models: %v %d", err, len(models))
	}
	resp, err := p.Complete(context.Background(), llm.Request{})
	if err != nil || resp.Message.Text() != "你好" || len(resp.ToolCalls) != 1 {
		t.Fatalf("Complete: %+v %v", resp, err)
	}
	if !json.Valid(resp.ToolCalls[0].Args) {
		t.Fatalf("tool args 不是合法 JSON")
	}

	var deltas []llm.Delta
	resp, err = p.Stream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil || resp.Message.Text() != "文件内容是 hello" || len(deltas) < 3 || deltas[len(deltas)-1].Kind != llm.DeltaDone {
		t.Fatalf("Stream: %+v %v deltas=%d", resp, err, len(deltas))
	}
}

func TestReplayCancelAndExhausted(t *testing.T) {
	p := replay.New(nil, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Complete(ctx, llm.Request{}); !seamerr.Is(err, seamerr.ErrCanceled) {
		t.Fatalf("取消应 ECANCELED, 实际 %v", err)
	}
	if _, err := p.Complete(context.Background(), llm.Request{}); !seamerr.Is(err, seamerr.ErrUnavailable) {
		t.Fatalf("脚本用完应 EUNAVAILABLE, 实际 %v", err)
	}
}
