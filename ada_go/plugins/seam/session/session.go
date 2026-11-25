// Package session 是 `seam/session` 的契约层:只有类型、接口、事件名与选择规则,
// 没有实现(实现属于 plugins/provider/*,策略属于 plugins/policy/*)。
//
// 契约来源:docs/seam-api.md §4。不变量(比接口更重要):
//  1. append-only:会话是事件日志,只追加,不改写;
//  2. 唯一事实来源:模型看到的内容必须能从事件重建(fork / resume / replay);
//  3. 未知事件必须可忽略:插件自有事件写 plugin:<id>/... 且 Ignorable=true;
//  4. 单写者:一个 session 同时只有一个写入方,读者可任意多;
//  5. 顺序即因果:seq 单调递增,跨插件因果靠坐标而不是时间。
package session

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// 标准事件类型(seam-api §4.3)。
const (
	TypeUserMessage      = "user/message"
	TypeAssistantMessage = "assistant/message"
	TypeToolCall         = "tool/call"
	TypeToolResult       = "tool/result"
	TypeTurnStart        = "turn/start"
	TypeTurnEnd          = "turn/end"
	TypeStepStart        = "step/start"
	TypeStepEnd          = "step/end"
	TypeCompactSummary   = "compact/summary"
	TypeError            = "error"
)

// ModelVisible 判断某事件类型是否进入模型可见上下文。
//
// 「控制面事件」与「模型可见事件」必须分开:否则把 turn/start 拼进请求,
// 模型会开始猜测编排细节。
func ModelVisible(eventType string) bool {
	switch eventType {
	case TypeUserMessage, TypeAssistantMessage, TypeToolResult, TypeCompactSummary:
		return true
	}
	return false
}

// TurnState 是本轮的状态。
type TurnState string

const (
	TurnRunning TurnState = "running"
	TurnDone    TurnState = "done"
	TurnAborted TurnState = "aborted"
)

// Event 是会话里的一条事实。
type Event struct {
	Seq       uint64          `json:"seq"`
	TS        time.Time       `json:"ts"`
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	StepID    string          `json:"stepId,omitempty"`
	Source    string          `json:"source"`
	Data      json.RawMessage `json:"data,omitempty"`
	Ignorable bool            `json:"ignorable,omitempty"`
}

// TurnInfo 是本轮的坐标与状态。
type TurnInfo struct {
	ID      string    `json:"id"`
	Index   int       `json:"index"`
	State   TurnState `json:"state"`
	Started time.Time `json:"started"`
}

// Meta 是会话的元信息。
type Meta struct {
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Model     string `json:"model,omitempty"`
	Parent    string `json:"parent,omitempty"`
}

// Session 是会话的读写契约。
//
// 每个方法都收 context 吗?接口里只有 Compact 收 —— 其余是纯内存读写,
// 真正的取消语义由调用方通过 Compact 与 Subscribe 的 dispose 表达。
type Session interface {
	ID() string
	Meta() Meta

	// Append 追加事件并返回分配的 seq(从 1 开始、连续)。
	Append(events ...Event) ([]uint64, error)
	// Events 返回 seq >= from 的事件;from=0 表示从头;limit<=0 表示不限。
	Events(from uint64, limit int) ([]Event, error)
	// Subscribe 订阅事件(先回放既有事件,再推送新事件);dispose 后停止推送。
	Subscribe(from uint64) (<-chan Event, kernel.Dispose)

	Turn() TurnInfo
	// Fork 复制 at 为止的历史成为一个新会话;父子互不影响。
	Fork(at uint64) (Session, kernel.Dispose, error)
	// Compact 把 upTo 之前的范围标记为已压缩,并追加一条摘要事件。
	Compact(ctx context.Context, upTo uint64, summary Event) error
	// Project 返回命名投影的当前视图(见 projection.go)。
	Project(name string) (any, error)
}

// Provider 是 session seam 的实现方(例如 session-memory)。
type Provider interface {
	// Name 是提供者名,用于确定性选择与诊断。
	Name() string
	// Version 是契约主版本;消费者期望的版本更高时视为依赖未满足。
	Version() int
	// Open 打开一个会话。
	Open(ctx context.Context, meta Meta) (Session, error)
}

// Registry 是 seam 插件提供的服务值:provider 向它注册,消费者从它选择。
//
// 选择规则与注册顺序无关:默认取名字排序后的第一个 provider。
// 「后注册的覆盖先注册的」会让装配顺序决定行为,这是装配层最忌讳的随机性。
type Registry struct {
	mu          sync.RWMutex
	providers   map[string]Provider
	projections *ProjectionRegistry
}

// NewRegistry 构造一个空注册表。
func NewRegistry() *Registry {
	return &Registry{
		providers:   make(map[string]Provider),
		projections: NewProjectionRegistry(),
	}
}

// Projections 返回投影注册表;投影插件通过它注册派生状态。
func (r *Registry) Projections() *ProjectionRegistry { return r.projections }

// ProjectionBinder 由 provider 可选实现:Registry 注册时把投影表注入 provider,
// 使 provider 创建的 Session 能执行 Project。
type ProjectionBinder interface {
	BindProjections(*ProjectionRegistry)
}

// Register 注册一个 provider;dispose 时注销。
func (r *Registry) Register(p Provider) (kernel.Dispose, error) {
	if p == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "provider 不能为 nil", false)
	}
	name := p.Name()
	if name == "" {
		return nil, seamerr.New(seamerr.ErrInvalid, "session", "provider 的 Name 不能为空", false)
	}
	if b, ok := p.(ProjectionBinder); ok {
		b.BindProjections(r.projections)
	}
	r.mu.Lock()
	if _, exists := r.providers[name]; exists {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, name, "同名 provider 已注册", false)
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

// Names 返回已注册的 provider 名(排序)。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Provider 按名字取一个 provider;name 为空时取默认(名字排序第一个)。
func (r *Registry) Provider(name string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		names := make([]string, 0, len(r.providers))
		for n := range r.providers {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, seamerr.Unimplemented("session", "没有任何 session provider")
		}
		name = names[0]
	}
	p, ok := r.providers[name]
	if !ok {
		return nil, seamerr.New(seamerr.ErrUnimplemented, name, "session provider 未注册", false)
	}
	return p, nil
}

// Open 是 Provider(name).Open 的语法糖;name 为空时用默认 provider。
func (r *Registry) Open(ctx context.Context, name string, meta Meta) (Session, error) {
	p, err := r.Provider(name)
	if err != nil {
		return nil, err
	}
	s, err := p.Open(ctx, meta)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, p.Name(), "provider 返回了 nil session", false)
	}
	return s, nil
}

// pluginID 是 seam 插件的默认 id(也是它 id 的 kebab-case 形式)。
const pluginID = "seam-session"

// Plugin 返回 seam/session 插件:它只提供 session 服务(Registry),不提供实现。
//
// 为什么 seam 是插件:产品可以只装一部分 seam 而不装实现,未配置后端时
// 消费者拿到的是 EUNIMPLEMENTED,而不是「服务不存在」导致 fiber pending 卡死。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("session", NewRegistry(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("session")
}

// EventTypes 便于诊断:返回标准事件类型列表。
func EventTypes() []string {
	out := []string{
		TypeUserMessage, TypeAssistantMessage, TypeToolCall, TypeToolResult,
		TypeTurnStart, TypeTurnEnd, TypeStepStart, TypeStepEnd,
		TypeCompactSummary, TypeError,
	}
	sort.Strings(out)
	return out
}

// PluginEvent 构造插件自有事件名:plugin:<id>/<kind>。
//
// 约定(seam-api §4.3):插件自有事件必须 Ignorable=true,旧读取器才能继续打开会话。
func PluginEvent(id, kind string) string {
	return "plugin:" + id + "/" + kind
}

// FormatSeq 便于日志与报告。
func FormatSeq(seq uint64) string { return strconv.FormatUint(seq, 10) }
