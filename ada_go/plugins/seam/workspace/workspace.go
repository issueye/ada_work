// Package workspace 是 `seam/workspace` 的契约层:工作区标识、解析请求/结果、
// 绑定上下文、装饰器链与错误码;没有实现(实现属于 plugins/provider/*)。
//
// 契约来源:docs/workspace-plugin.md。不变量:
//  1. 宿主绝对路径不出 seam:Info 不含根,Target.Host 只给 fs/shell provider;
//  2. 绑定不静默回退:未知工作区 → ENOTFOUND;
//  3. 根选择属于宿主,模型工具不得切换工作区;
//  4. 授权检查与路径解析同一次完成,fail-closed。
package workspace

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// 服务名、插件 id 与固定挂载点。
const (
	ServiceName = "workspace"
	PluginID    = "seam-workspace"
	MountRoot   = "/workspace"
)

// 总线事件名。
const (
	// EventChanged 在当前工作区实际变化时发出,payload 为 {"previous":ID,"current":ID}。
	EventChanged = "workspace/changed"
	// EventDenied 由策略可选发出,只用于观察,不是门禁。
	EventDenied = "workspace/denied"
)

// 错误码。
const (
	CodeNoWorkspace seamerr.Code = "ENOWORKSPACE"
	CodeNotFound    seamerr.Code = "ENOTFOUND"
	CodeDenied      seamerr.Code = "EDENIED"
	CodeInvalid     seamerr.Code = "EINVALID"
	CodeConflict    seamerr.Code = "ECONFLICT"
)

// ID 是工作区稳定标识;只允许 kebab-case,并出现在 session.Meta 与工具结果里。
type ID string

// Op 是解析用途;同一路径在读/写/执行下可能有不同授权。
type Op string

const (
	OpRead   Op = "read"
	OpWrite  Op = "write"
	OpEdit   Op = "edit"
	OpList   Op = "list"
	OpSearch Op = "search"
	OpExec   Op = "exec"
)

// Valid 判断 op 是否是已定义的用途。
func (o Op) Valid() bool {
	switch o {
	case OpRead, OpWrite, OpEdit, OpList, OpSearch, OpExec:
		return true
	}
	return false
}

// Mutating 判断 op 是否可能改变宿主状态;只读工作区必须拒绝这些操作。
func (o Op) Mutating() bool {
	return o == OpWrite || o == OpEdit || o == OpExec
}

// Info 是插件可见的工作区描述。它刻意不含宿主根路径。
type Info struct {
	ID       ID       `json:"id"`
	Name     string   `json:"name"`
	Mount    string   `json:"mount"`
	ReadOnly bool     `json:"readOnly,omitempty"`
	Ignore   []string `json:"ignore,omitempty"`
}

// Target 是一次解析结果。Host 只允许 fs/shell provider 内部使用:
// 不得写进事件、日志、工具结果或 prompt,因此没有 JSON 字段。
type Target struct {
	Workspace ID     `json:"workspace"`
	Virtual   string `json:"virtual"`
	Rel       string `json:"rel"`
	Host      string `json:"-"`
	Root      string `json:"-"` // 工作区根(已解析),同样只允许 fs/shell provider 使用
}

// Binding 是会话选定的工作区;由 session.Meta.Workspace 派生,随调用 ctx 传播。
type Binding struct {
	SessionID string
	Workspace ID
}

// Service 是工作区契约。ResolveHost 同时完成授权与路径解析,调用方不得先打开再检查。
type Service interface {
	Current(ctx context.Context) (Info, error)
	Get(ctx context.Context, id ID) (Info, error)
	List(ctx context.Context) ([]Info, error)

	// ResolveHost 只给 fs/shell provider 使用。
	ResolveHost(ctx context.Context, op Op, p string) (Target, error)

	// Switch 只给宿主/用户调用,不注册成模型工具;已有会话的绑定不变。
	Switch(ctx context.Context, id ID) (Info, error)
}

// NoWorkspace 构造 ENOWORKSPACE:provider 在位但没有任何已配置工作区。
func NoWorkspace() error {
	return &seamerr.Error{Code: CodeNoWorkspace, Provider: "workspace", Message: "没有可用的工作区"}
}

// NotFound 构造 ENOTFOUND:工作区 ID 不存在(不静默回退到 Current)。
func NotFound(id ID) error {
	return &seamerr.Error{Code: CodeNotFound, Provider: "workspace", Message: fmt.Sprintf("工作区不存在: %s", id)}
}

// Invalid 构造 EINVALID。
func Invalid(message string) error {
	return &seamerr.Error{Code: CodeInvalid, Provider: "workspace", Message: message}
}

// Conflict 构造 ECONFLICT。
func Conflict(message string) error {
	return &seamerr.Error{Code: CodeConflict, Provider: "workspace", Message: message}
}

// Denied 构造 EDENIED;source 区分 workspace 与 policy。
func Denied(source, message string) error {
	return &seamerr.Error{Code: CodeDenied, Provider: "workspace", Message: fmt.Sprintf("[source=%s] %s", source, message)}
}

// bindingKey 是 ctx 中保存 Binding 的键类型;避免与其它包的字符串键冲突。
type bindingKey struct{}

// WithBinding 把会话工作区绑定写入 ctx;空工作区视为「无绑定」,原样返回。
func WithBinding(ctx context.Context, b Binding) context.Context {
	if b.Workspace == "" {
		return ctx
	}
	return context.WithValue(ctx, bindingKey{}, b)
}

// BindingFrom 读取会话绑定;没有绑定或工作区为空时返回 false。
func BindingFrom(ctx context.Context) (Binding, bool) {
	b, ok := ctx.Value(bindingKey{}).(Binding)
	if !ok || b.Workspace == "" {
		return Binding{}, false
	}
	return b, true
}

// Chain 是 seam 插件提供的服务值:provider 安装底座,策略注册装饰器。
//
// 为什么用装饰器而不是事件:工作区授权可能拒绝写/执行,必须确定性、可排序。
type Chain struct {
	mu    sync.Mutex
	base  Service
	items []decoration
	seq   uint64
}

type decoration struct {
	id       uint64
	fn       func(Service) Service
	priority int
}

// NewChain 创建空链。
func NewChain() *Chain { return &Chain{} }

// Install 安装底座实现;一个 workspace 只能有一个实现。
func (c *Chain) Install(base Service) (kernel.Dispose, error) {
	if base == nil {
		return nil, Invalid("workspace: 底座实现不能为 nil")
	}
	c.mu.Lock()
	if c.base != nil {
		c.mu.Unlock()
		return nil, Invalid("workspace: 已安装底座实现")
	}
	c.base = base
	c.mu.Unlock()

	var once bool
	return func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !once && c.base == base {
			c.base = nil
			once = true
		}
		return nil
	}, nil
}

// Decorate 注册装饰器;priority 高者在外层(先判),同级后注册者在外层。
func (c *Chain) Decorate(fn func(Service) Service, priority int) (kernel.Dispose, error) {
	if fn == nil {
		return nil, Invalid("workspace: 装饰器不能为 nil")
	}
	c.mu.Lock()
	c.seq++
	d := decoration{id: c.seq, fn: fn, priority: priority}
	c.items = append(c.items, d)
	c.mu.Unlock()

	var once bool
	return func(context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if once {
			return nil
		}
		once = true
		for i := range c.items {
			if c.items[i].id == d.id {
				c.items = append(c.items[:i], c.items[i+1:]...)
				break
			}
		}
		return nil
	}, nil
}

// Get 组装当前链;未安装底座返回 EUNIMPLEMENTED。
func (c *Chain) Get() (Service, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base == nil {
		return nil, seamerr.Unimplemented("workspace", "没有安装 workspace 实现")
	}
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority < items[j].priority
		}
		return items[i].id < items[j].id
	})
	cur := c.base
	for _, d := range items {
		cur = d.fn(cur)
	}
	return cur, nil
}

// Applied 返回装饰器优先级序列(外层到内层,诊断用)。
func (c *Chain) Applied() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]decoration, len(c.items))
	copy(items, c.items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].priority != items[j].priority {
			return items[i].priority > items[j].priority
		}
		return items[i].id > items[j].id
	})
	out := make([]int, 0, len(items))
	for _, d := range items {
		out = append(out, d.priority)
	}
	return out
}

// Plugin 返回 seam/workspace 插件:只提供 workspace 服务(Chain),不提供实现。
func Plugin() kernel.Plugin {
	return kernel.Func(PluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide(ServiceName, NewChain(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide(ServiceName)
}
