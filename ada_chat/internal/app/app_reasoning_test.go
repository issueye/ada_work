package app

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reasoningMessage() *Message {
	return &Message{
		ID: 2, Role: RoleAssistant, Streaming: true,
		Parts: []Part{
			{Kind: PartReasoning, Text: "先想一想步骤", Open: true},
		},
	}
}

// TestDeltaCollapsesReasoning 文字到达后,前面那段思考自动收起。
func TestDeltaCollapsesReasoning(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	m := reasoningMessage()
	a := &App{Sessions: []*Session{{ID: 1, BotID: 1, Messages: []*Message{m}}}}
	a.BotDelta(1, m.ID, "正文")

	if m.Parts[0].Kind != PartReasoning || m.Parts[0].Open {
		t.Fatalf("思考块应自动收起: %+v", m.Parts[0])
	}
	if len(m.Parts) != 2 || m.Parts[1].Kind != PartText || m.Parts[1].Text != "正文" {
		t.Fatalf("正文块没有追加: %+v", m.Parts)
	}
}

// TestToolCollapsesReasoning 工具事件到达时,思考块也自动收起。
func TestToolCollapsesReasoning(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	m := reasoningMessage()
	a := &App{Sessions: []*Session{{ID: 1, BotID: 1, Messages: []*Message{m}}}}
	a.BotTool(1, m.ID, ToolEvent{ID: "call-1", Name: "run_shell", Status: "running"})

	if m.Parts[0].Open {
		t.Fatalf("工具到达后思考块应收起: %+v", m.Parts[0])
	}
}

// TestTurnDoneCollapsesReasoning 一轮结束时收起所有思考块。
func TestTurnDoneCollapsesReasoning(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	m := reasoningMessage()
	a := &App{Sessions: []*Session{{ID: 1, BotID: 1, Messages: []*Message{m}}}}
	a.BotTurnDone(1, m.ID, "完成")

	if m.Parts[0].Open {
		t.Fatalf("结束时思考块应收起: %+v", m.Parts[0])
	}
}

// TestStopTurnCollapsesReasoning 用户停止生成时收起思考块。
func TestStopTurnCollapsesReasoning(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	m := reasoningMessage()
	a := &App{ActiveID: 1, Sessions: []*Session{{ID: 1, BotID: 1, Messages: []*Message{m}}}}
	if !a.StopTurn() {
		t.Fatalf("应能停止当前一轮")
	}
	if m.Parts[0].Open {
		t.Fatalf("停止后思考块应收起")
	}
	if m.Streaming {
		t.Fatalf("停止后不应再标记 streaming")
	}
	_ = time.Now
}

// TestTurnDoneCollapsesTools 一轮结束后把工具卡片一并收起:只留报告(正文),
// 过程折成一行,用户想看得点开。
func TestTurnDoneCollapsesTools(t *testing.T) {
	withStatePath(t, filepath.Join(t.TempDir(), "state.json"))
	call := &ToolCall{ID: "call-1", Name: "run_shell", Open: true, State: ToolDone}
	m := &Message{
		ID: 2, Role: RoleAssistant, Streaming: true,
		Tools: []*ToolCall{call},
		Parts: []Part{
			{Kind: PartText, Text: "先看看"},
			{Kind: PartTool, ToolID: call.ID},
			{Kind: PartText, Text: "报告：一切正常"},
		},
	}
	a := &App{Sessions: []*Session{{ID: 1, BotID: 1, Messages: []*Message{m}}}}
	a.BotTurnDone(1, m.ID, "报告：一切正常")

	if call.Open {
		t.Fatalf("结束时工具卡片应收起: %+v", call)
	}
	if m.Streaming {
		t.Fatalf("结束时不应再 streaming")
	}
	if !strings.Contains(m.Text, "报告：一切正常") {
		t.Fatalf("报告不应被折叠掉: %q", m.Text)
	}
}
