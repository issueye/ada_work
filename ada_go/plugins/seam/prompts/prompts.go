// Package prompts 是 `seam/prompts` 的契约层:Section、Assemble 输入输出与组装引擎。
//
// seam-api §5 要求「加提示用 Section,不要监听 assemble 往里塞」;因此引擎本身
// 负责稳定排序、预算裁剪与 waterfall 整体替换点,core/system-prompt 只注册 section。
package prompts

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/llm"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// Scope 是 section 的生命周期范围。
type Scope string

const (
	ScopeSession Scope = "session"
	ScopeTurn    Scope = "turn"
	ScopeStep    Scope = "step"
)

// Section 是一段系统提示。
type Section struct {
	Key   string      `json:"key"`
	Title string      `json:"title,omitempty"`
	Body  string      `json:"body"`
	Order int         `json:"order,omitempty"`
	Scope Scope       `json:"scope,omitempty"`
	Media []llm.Block `json:"media,omitempty"`
}

// AssembleInput 是组装一次的输入。
type AssembleInput struct {
	SessionID string        `json:"sessionId,omitempty"`
	TurnID    string        `json:"turnId,omitempty"`
	StepID    string        `json:"stepId,omitempty"`
	Model     llm.ModelInfo `json:"model"`
	Budget    int           `json:"budget,omitempty"`
}

// Assembly 是组装结果。
type Assembly struct {
	Sections []Section `json:"sections"`
	Text     string    `json:"text"`
	Tokens   int       `json:"tokens"`
	Dropped  []string  `json:"dropped,omitempty"`
}

// Prompts 是系统提示契约。
type Prompts interface {
	Section(s Section) (kernel.Dispose, error)
	Assemble(ctx context.Context, in AssembleInput) (Assembly, error)
	Budget(ctx context.Context) (int, error)
}

// EventAssemble 是整体替换点(waterfall,输入 *Assembly);默认必须调 next。
const EventAssemble = "prompts/assemble"

// EventAssembled 是组装后的观察点(emit,输入 *Assembly)。
const EventAssembled = "prompts/assembled"

// Engine 是 seam 自带的组装引擎。它不调用模型,只做 section 注册/排序/裁剪/事件。
type Engine struct {
	mu       sync.RWMutex
	sections map[string]Section
	budget   int
	ctx      *kernel.Context
}

// NewEngine 构造组装引擎;budget<=0 时取 4096。
func NewEngine(budget int) *Engine {
	if budget <= 0 {
		budget = 4096
	}
	return &Engine{sections: make(map[string]Section), budget: budget}
}

// Attach 绑定插件作用域,用于发事件;必须在插件 apply 内调用一次。
func (e *Engine) Attach(ctx *kernel.Context) {
	e.mu.Lock()
	e.ctx = ctx
	e.mu.Unlock()
}

// Section 注册一段提示;key 全局唯一,重复注册返回错误。
func (e *Engine) Section(s Section) (kernel.Dispose, error) {
	if strings.TrimSpace(s.Key) == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "prompts", "section key 不能为空", false)
	}
	if s.Scope == "" {
		s.Scope = ScopeSession
	}
	switch s.Scope {
	case ScopeSession, ScopeTurn, ScopeStep:
	default:
		return nil, seamerr.New(seamerr.ErrInvalid, "prompts", "未知 scope: "+string(s.Scope), false)
	}
	e.mu.Lock()
	if _, ok := e.sections[s.Key]; ok {
		e.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, "prompts", "section key 已存在: "+s.Key, false)
	}
	e.sections[s.Key] = s
	e.mu.Unlock()
	return func(context.Context) error {
		e.mu.Lock()
		delete(e.sections, s.Key)
		e.mu.Unlock()
		return nil
	}, nil
}

// Assemble 按 Order,Key 排序并渲染;超预算时从尾部裁剪。
func (e *Engine) Assemble(ctx context.Context, in AssembleInput) (Assembly, error) {
	if err := ctx.Err(); err != nil {
		return Assembly{}, seamerr.Wrap(seamerr.ErrCanceled, "prompts", "组装前已取消", err)
	}
	budget := in.Budget
	if budget <= 0 {
		budget = e.budget
	}

	e.mu.RLock()
	list := make([]Section, 0, len(e.sections))
	for _, s := range e.sections {
		list = append(list, s)
	}
	bus := e.ctx
	e.mu.RUnlock()

	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Order != list[j].Order {
			return list[i].Order < list[j].Order
		}
		return list[i].Key < list[j].Key
	})

	asm := Assembly{Sections: list}
	asm.Text, asm.Tokens = render(list)

	// 先给策略整体替换点;替换后的结果仍要经过预算裁剪,避免策略绕过预算。
	if bus != nil {
		if v, err := bus.Waterfall(ctx, EventAssemble, &asm); err != nil {
			return Assembly{}, err
		} else if repl, ok := v.(*Assembly); ok && repl != nil {
			asm = *repl
		}
	}

	if budget > 0 && asm.Tokens > budget {
		asm = trim(asm, budget)
	}
	if bus != nil {
		bus.EmitContext(ctx, EventAssembled, &asm)
	}
	return asm, nil
}

// Budget 返回当前提示预算(tokens)。
func (e *Engine) Budget(context.Context) (int, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.budget, nil
}

func render(list []Section) (string, int) {
	var b strings.Builder
	total := 0
	for i, s := range list {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if s.Title != "" {
			b.WriteString("## ")
			b.WriteString(s.Title)
			b.WriteString("\n")
		}
		b.WriteString(s.Body)
		total += estimateTokens(s.Title) + estimateTokens(s.Body) + 1
	}
	return b.String(), total
}

// estimateTokens 用「4 个 UTF-8 字符 ≈ 1 token」的保守估算;真实计数由 provider 负责。
func estimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

func trim(asm Assembly, budget int) Assembly {
	for len(asm.Sections) > 0 && asm.Tokens > budget {
		last := asm.Sections[len(asm.Sections)-1]
		asm.Sections = asm.Sections[:len(asm.Sections)-1]
		asm.Dropped = append([]string{last.Key}, asm.Dropped...)
		asm.Text, asm.Tokens = render(asm.Sections)
	}
	asm.Text, asm.Tokens = render(asm.Sections)
	return asm
}

const pluginID = "seam-prompts"

// Plugin 返回 seam/prompts 插件:提供 prompts 服务(Engine)。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		engine := NewEngine(cfg.Int("budget"))
		engine.Attach(ctx)
		_, err := ctx.Provide("prompts", engine, kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("prompts")
}
