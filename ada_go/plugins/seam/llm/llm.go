// Package llm 是 `seam/llm` 的契约层:请求/响应类型、provider 注册表与事件名。
//
// 它只定义接口与数据形状;具体模型调用属于 plugins/provider/*,
// 重试/路由/裁剪属于 plugins/policy/*(seam-api §2)。
package llm

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// Role 是消息角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// BlockKind 是消息块类型。
type BlockKind string

const (
	BlockText       BlockKind = "text"
	BlockImage      BlockKind = "image"
	BlockToolResult BlockKind = "tool_result"
	BlockReasoning  BlockKind = "reasoning"
)

// FinishReason 是模型结束原因。
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishError         FinishReason = "error"
)

// DeltaKind 是流式增量类型。
type DeltaKind string

const (
	DeltaText      DeltaKind = "text"
	DeltaReasoning DeltaKind = "reasoning"
	DeltaToolArgs  DeltaKind = "tool_args"
	DeltaUsage     DeltaKind = "usage"
	DeltaDone      DeltaKind = "done"
)

// Block 是消息内容块。
type Block struct {
	Kind     BlockKind         `json:"kind"`
	Text     string            `json:"text,omitempty"`
	Resource string            `json:"resource,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// Message 是一条模型消息。
type Message struct {
	Role      Role       `json:"role"`
	Content   []Block    `json:"content"`
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
}

// Text 返回消息里所有文本块的拼接(不含图片/推理)。
func (m Message) Text() string {
	out := ""
	for _, b := range m.Content {
		if b.Kind == BlockText {
			out += b.Text
		}
	}
	return out
}

// ToolCall 是模型请求的一次工具调用。
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Args  json.RawMessage `json:"args"`
	Index int             `json:"index,omitempty"`
}

// Usage 是 token 用量。
type Usage struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
	CacheRead    int `json:"cacheRead,omitempty"`
	CacheWrite   int `json:"cacheWrite,omitempty"`
}

// ModelInfo 描述一个模型。
type ModelInfo struct {
	ID            string   `json:"id"`
	ContextWindow int      `json:"contextWindow,omitempty"`
	MaxOutput     int      `json:"maxOutput,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

// ToolSchema 是给模型的工具描述。
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
}

// ToolChoice 控制模型是否必须调用工具。
type ToolChoice struct {
	Mode string `json:"mode,omitempty"` // auto | none | required | name
	Name string `json:"name,omitempty"`
}

// Request 是一次补全请求。
type Request struct {
	Model       string            `json:"model"`
	Messages    []Message         `json:"messages"`
	Tools       []ToolSchema      `json:"tools,omitempty"`
	ToolChoice  ToolChoice        `json:"toolChoice,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"topP,omitempty"`
	MaxTokens   int               `json:"maxTokens,omitempty"`
	Stop        []string          `json:"stop,omitempty"`
	SessionID   string            `json:"sessionId,omitempty"`
	TurnID      string            `json:"turnId,omitempty"`
	StepID      string            `json:"stepId,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Response 是一次补全结果。
type Response struct {
	Message   Message         `json:"message"`
	ToolCalls []ToolCall      `json:"toolCalls,omitempty"`
	Usage     Usage           `json:"usage"`
	Finish    FinishReason    `json:"finish,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// Delta 是一个流式增量。
type Delta struct {
	Kind      DeltaKind `json:"kind"`
	Text      string    `json:"text,omitempty"`
	ToolIndex int       `json:"toolIndex,omitempty"`
	ToolID    string    `json:"toolId,omitempty"`
	Usage     *Usage    `json:"usage,omitempty"`
}

// LLM 是一次模型补全的契约。
type LLM interface {
	Models(ctx context.Context) ([]ModelInfo, error)
	Complete(ctx context.Context, req Request) (Response, error)
	Stream(ctx context.Context, req Request, onDelta func(Delta) error) (Response, error)
}

// Provider 是 seam 的实现方:模型实现必须能自我标识,便于确定性选择。
type Provider interface {
	Name() string
	Version() int
	LLM
}

// EventRequest 允许策略在调用前改写请求(waterfall,输入 *Request)。
const EventRequest = "llm/request"

// EventStream 允许策略过滤/改写增量(waterfall,输入 *Delta)。
const EventStream = "llm/stream"

// EventResponse 是调用后的观察点(emit,输入 *Response)。
const EventResponse = "llm/response"

// EventError 是失败观察点(emit,输入 error)。
const EventError = "llm/error"

// Registry 是 seam 插件提供的服务值:provider 注册,消费者按名选择。
//
// 选择规则与注册顺序无关:名字为空时取排序后的第一个。
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	items     []decoration
	seq       uint64
}

type decoration struct {
	id       uint64
	fn       func(Provider) Provider
	priority int
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register 注册 provider;返回 disposer。
func (r *Registry) Register(p Provider) (kernel.Dispose, error) {
	if p == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "llm", "provider 不能为 nil", false)
	}
	name := p.Name()
	if name == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "llm", "provider 的 Name 不能为空", false)
	}
	r.mu.Lock()
	if _, ok := r.providers[name]; ok {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, name, "同名 llm provider 已注册", false)
	}
	r.providers[name] = p
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		if cur, ok := r.providers[name]; ok && cur == p {
			delete(r.providers, name)
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// Names 返回已注册 provider 名(排序)。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Decorate 注册 provider 装饰器;priority 大者在外层。返回 disposer。
func (r *Registry) Decorate(fn func(Provider) Provider, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "llm", "装饰器不能为 nil", false)
	}
	r.mu.Lock()
	r.seq++
	d := decoration{id: r.seq, fn: fn, priority: priority}
	r.items = append(r.items, d)
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		for i := range r.items {
			if r.items[i].id == d.id {
				r.items = append(r.items[:i], r.items[i+1:]...)
				break
			}
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// Get 按名取 provider;name 为空时取排序后的第一个。
func (r *Registry) Get(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.providers) == 0 {
		return nil, seamerr.Unimplemented("llm", "没有任何 llm provider")
	}
	if name == "" {
		names := make([]string, 0, len(r.providers))
		for n := range r.providers {
			names = append(names, n)
		}
		sort.Strings(names)
		name = names[0]
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, seamerr.New(seamerr.ErrUnimplemented, name, "llm provider 未注册", false)
	}
	items := make([]decoration, len(r.items))
	copy(items, r.items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		return items[i].id < items[j].id
	})
	for _, d := range items {
		p = d.fn(p)
	}
	return p, nil
}

// Models 是默认 provider 的模型列表语法糖。
func (r *Registry) Models(ctx context.Context, name string) ([]ModelInfo, error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	return p.Models(ctx)
}

const pluginID = "seam-llm"

// Plugin 返回 seam/llm 插件:只提供 llm 服务(Registry),不提供实现。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("llm", NewRegistry(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("llm")
}
