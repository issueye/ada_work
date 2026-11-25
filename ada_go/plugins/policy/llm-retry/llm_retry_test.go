package retry_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/plugins/policy/llm-retry"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type fake struct {
	completeCalls  int
	streamCalls    int
	failComplete   int
	failStream     int
	emitBeforeFail bool
}

func (f *fake) Name() string { return "fake" }
func (f *fake) Version() int { return 1 }
func (f *fake) Models(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "m"}}, nil
}

func (f *fake) Complete(context.Context, llm.Request) (llm.Response, error) {
	f.completeCalls++
	if f.completeCalls <= f.failComplete {
		return llm.Response{}, seamerr.New(seamerr.ErrUnavailable, "fake", "transient", true)
	}
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}}, nil
}

func (f *fake) Stream(ctx context.Context, req llm.Request, onDelta func(llm.Delta) error) (llm.Response, error) {
	f.streamCalls++
	if f.streamCalls <= f.failStream {
		if f.emitBeforeFail {
			if err := onDelta(llm.Delta{Kind: llm.DeltaText, Text: "partial"}); err != nil {
				return llm.Response{}, err
			}
		}
		return llm.Response{}, seamerr.New(seamerr.ErrUnavailable, "fake", "transient", true)
	}
	if err := onDelta(llm.Delta{Kind: llm.DeltaText, Text: "ok"}); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Kind: llm.BlockText, Text: "ok"}}}}, nil
}

func TestCompleteRetries(t *testing.T) {
	f := &fake{failComplete: 1}
	p := retry.New(f, 2, time.Millisecond, 2*time.Millisecond, nil)
	resp, err := p.Complete(context.Background(), llm.Request{})
	if err != nil || resp.Message.Text() != "ok" || f.completeCalls != 2 {
		t.Fatalf("重试失败: calls=%d resp=%+v err=%v", f.completeCalls, resp, err)
	}
}

func TestStreamDoesNotRetryAfterDelta(t *testing.T) {
	f := &fake{failStream: 1, emitBeforeFail: true}
	p := retry.New(f, 2, time.Millisecond, 2*time.Millisecond, nil)
	var deltas []string
	_, err := p.Stream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas = append(deltas, d.Text)
		return nil
	})
	if err == nil || f.streamCalls != 1 || len(deltas) != 1 || deltas[0] != "partial" {
		t.Fatalf("已有增量后不应重试: calls=%d deltas=%v err=%v", f.streamCalls, deltas, err)
	}
}

func TestStreamRetriesBeforeDelta(t *testing.T) {
	f := &fake{failStream: 1}
	p := retry.New(f, 2, time.Millisecond, 2*time.Millisecond, nil)
	var deltas []string
	resp, err := p.Stream(context.Background(), llm.Request{}, func(d llm.Delta) error {
		deltas = append(deltas, d.Text)
		return nil
	})
	if err != nil || f.streamCalls != 2 || resp.Message.Text() != "ok" || len(deltas) != 1 || deltas[0] != "ok" {
		t.Fatalf("无增量时应重试: calls=%d deltas=%v resp=%+v err=%v", f.streamCalls, deltas, resp, err)
	}
}
