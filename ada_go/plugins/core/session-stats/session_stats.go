// Package sessionstats 是 `core/session-stats`:把会话事件投影成轮次/消息/工具/token 统计。
//
// 它不写 session,只消费事件;`agent-loop`、compaction 与 UI 都通过
// `Session.Project("turn-stats")` 读同一份派生状态。
package sessionstats

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/session"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// ProjectionName 是投影的稳定名字。
const ProjectionName = session.ProjectionTurnStats

// Stats 是投影视图;Token 是保守估算,用于 compaction/预算判断。
type Stats struct {
	Turns           int    `json:"turns"`
	Messages        int    `json:"messages"`
	ToolCalls       int    `json:"toolCalls"`
	Tokens          int    `json:"tokens"`
	CompactedTokens int    `json:"compactedTokens,omitempty"`
	LastSeq         uint64 `json:"lastSeq"`
	CompactedTo     uint64 `json:"compactedTo,omitempty"`
}

// TokenCount 让消费者不必 import 本包就能读 token 估算。
func (s Stats) TokenCount() int {
	n := s.Tokens - s.CompactedTokens
	if n < 0 {
		return 0
	}
	return n
}

// CompactedToSeq 返回已压缩边界,供 render 跳过旧事件。
func (s Stats) CompactedToSeq() uint64 { return s.CompactedTo }

// Projection 把事件折叠成 Stats。
type Projection struct{}

// New 构造投影。
func New() *Projection { return &Projection{} }

// Name 返回投影名。
func (p *Projection) Name() string { return ProjectionName }

// Version 返回投影版本;规则变化时递增,旧缓存会整体重放。
func (p *Projection) Version() int { return 1 }

// Apply 是纯函数:不修改传入 state,返回新状态。
func (p *Projection) Apply(state any, ev session.Event) (any, bool) {
	cur, _ := state.(*Stats)
	if cur == nil {
		cur = &Stats{}
	}
	next := *cur
	changed := false
	switch ev.Type {
	case session.TypeTurnStart:
		next.Turns++
		changed = true
	case session.TypeUserMessage:
		var m llm.Message
		if json.Unmarshal(ev.Data, &m) == nil {
			next.Messages++
			next.Tokens += tokensMessage(m)
			changed = true
		}
	case session.TypeAssistantMessage:
		var payload struct {
			Message llm.Message `json:"message"`
			Usage   llm.Usage   `json:"usage"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil {
			next.Messages++
			next.Tokens += tokensMessage(payload.Message)
			if payload.Usage.InputTokens > 0 || payload.Usage.OutputTokens > 0 {
				next.Tokens += payload.Usage.InputTokens + payload.Usage.OutputTokens
			}
			changed = true
		}
	case session.TypeToolCall:
		next.ToolCalls++
		changed = true
	case session.TypeToolResult:
		var payload struct {
			Result tools.Result `json:"result"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil {
			next.Tokens += tokensBlocks(payload.Result.Content)
			changed = true
		}
	case session.TypeCompactSummary:
		var payload struct {
			UpTo   uint64 `json:"upTo"`
			Tokens int    `json:"tokens"`
		}
		if json.Unmarshal(ev.Data, &payload) == nil && payload.UpTo >= next.CompactedTo {
			next.CompactedTo = payload.UpTo
			next.CompactedTokens += payload.Tokens
			changed = true
		}
	}
	if !changed {
		return cur, false
	}
	if ev.Seq > next.LastSeq {
		next.LastSeq = ev.Seq
	}
	return &next, true
}

// View 返回投影视图;调用方拿到的是只读快照。
func (p *Projection) View(state any) any { return state }

func tokensMessage(m llm.Message) int { return tokensBlocks(m.Content) }

func tokensBlocks(blocks []llm.Block) int {
	total := 0
	for _, b := range blocks {
		if b.Kind == llm.BlockText {
			total += estimateTokens(b.Text)
		}
	}
	return total
}

// estimateTokens 用「4 个 UTF-8 字符 ≈ 1 token」的保守估算。
func estimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

// Plugin 返回 session-stats 插件:inject session,注册 turn-stats 投影。
func Plugin() kernel.Plugin {
	return kernel.Func("session-stats", func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		registry, err := kernel.Service[*session.Registry](ctx, "session")
		if err != nil {
			return nil, err
		}
		return registry.Projections().Register(New())
	}).WithInject("session")
}
