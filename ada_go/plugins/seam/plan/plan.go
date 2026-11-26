// Package plan 是会话级计划的契约:步骤、状态、优先级、事件名与文本渲染。
//
// 计划不是内存状态:每次变更写成 session 事件(EventUpdated),因此
// fork/resume/replay 都能恢复;agent-loop 在请求里注入最新计划,ACP 适配器
// 把它映射成 session/update 的 plan。
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EventUpdated 是计划变更的 session 事件类型。
//
// 插件自有事件必须 Ignorable=true(seam-api §4.3),旧读取器才能继续打开会话。
const EventUpdated = "plugin:plan/updated"

// Status 是步骤状态,取值与 ACP PlanEntryStatus 对齐。
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// Priority 是步骤优先级,取值与 ACP PlanEntryPriority 对齐。
type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityMedium Priority = "medium"
	PriorityLow    Priority = "low"
)

// 计划边界:过大的计划既挤占上下文,也无法稳定展示。
const (
	MaxSteps       = 50
	MaxContentRune = 200
	MaxTitleRune   = 100
)

// Step 是计划中的一步。
type Step struct {
	Content  string   `json:"content"`
	Status   Status   `json:"status,omitempty"`
	Priority Priority `json:"priority,omitempty"`
}

// Plan 是一个会话的计划。
type Plan struct {
	Title string `json:"title,omitempty"`
	Steps []Step `json:"steps"`
}

// Valid 判断状态是否是已定义值。
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusInProgress, StatusCompleted:
		return true
	}
	return false
}

// Valid 判断优先级是否是已定义值。
func (p Priority) Valid() bool {
	switch p {
	case PriorityHigh, PriorityMedium, PriorityLow:
		return true
	}
	return false
}

// Decode 解析事件载荷;不做补默认值。
func Decode(raw json.RawMessage) (Plan, error) {
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return Plan{}, fmt.Errorf("plan: 解析失败: %w", err)
	}
	return p, nil
}

// Encode 编码事件载荷。
func Encode(p Plan) json.RawMessage {
	b, _ := json.Marshal(p)
	return b
}

// Normalize 校验并补默认值:空状态 → pending,空优先级 → medium。
func Normalize(p Plan) (Plan, error) {
	p.Title = strings.TrimSpace(p.Title)
	if len([]rune(p.Title)) > MaxTitleRune {
		return Plan{}, fmt.Errorf("计划标题不能超过 %d 个字符", MaxTitleRune)
	}
	if len(p.Steps) == 0 {
		return Plan{}, fmt.Errorf("计划至少需要一个步骤")
	}
	if len(p.Steps) > MaxSteps {
		return Plan{}, fmt.Errorf("计划最多 %d 步", MaxSteps)
	}
	for i := range p.Steps {
		s := &p.Steps[i]
		s.Content = strings.TrimSpace(s.Content)
		if s.Content == "" {
			return Plan{}, fmt.Errorf("第 %d 步内容为空", i+1)
		}
		if len([]rune(s.Content)) > MaxContentRune {
			return Plan{}, fmt.Errorf("第 %d 步内容不能超过 %d 个字符", i+1, MaxContentRune)
		}
		if s.Status == "" {
			s.Status = StatusPending
		}
		if !s.Status.Valid() {
			return Plan{}, fmt.Errorf("第 %d 步状态非法: %s", i+1, s.Status)
		}
		if s.Priority == "" {
			s.Priority = PriorityMedium
		}
		if !s.Priority.Valid() {
			return Plan{}, fmt.Errorf("第 %d 步优先级非法: %s", i+1, s.Priority)
		}
	}
	return p, nil
}

// Text 把计划渲染成模型可读文本:状态用 [ ]/[~]/[x] 标记,高优先级额外标注。
func Text(p Plan) string {
	var b strings.Builder
	b.WriteString("# 当前计划")
	if p.Title != "" {
		b.WriteString(": ")
		b.WriteString(p.Title)
	}
	b.WriteString("\n")
	for i, s := range p.Steps {
		marker := "[ ]"
		switch s.Status {
		case StatusInProgress:
			marker = "[~]"
		case StatusCompleted:
			marker = "[x]"
		}
		fmt.Fprintf(&b, "%d. %s %s", i+1, marker, s.Content)
		if s.Priority == PriorityHigh {
			b.WriteString(" (high)")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
