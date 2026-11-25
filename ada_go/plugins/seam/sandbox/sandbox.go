// Package sandbox 是 `seam/sandbox` 的契约层:模式、操作、判定、事实与后端探测。
//
// 契约来源:docs/seam-api-security.md §2。实现属于 plugins/provider/*;
// 本包只提供「判定入口」的类型与后端注册表,不做任何策略判断。
package sandbox

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// Mode 是会话沙箱模式。
type Mode string

const (
	ModeReadOnly       Mode = "read-only"
	ModeWorkspaceWrite Mode = "workspace-write"
	ModeDanger         Mode = "danger-full-access"
)

// ValidMode 判断模式是否合法。
func ValidMode(m Mode) bool {
	switch m {
	case ModeReadOnly, ModeWorkspaceWrite, ModeDanger:
		return true
	}
	return false
}

// Op 是被判定的操作。
type Op string

const (
	OpFSRead    Op = "fs.read"
	OpFSWrite   Op = "fs.write"
	OpFSEdit    Op = "fs.edit"
	OpFSList    Op = "fs.list"
	OpShellExec Op = "shell.exec"
)

// Path 是虚拟路径(与 seam/fs 同一路径空间)。
type Path string

// Request 是一次判定请求。
type Request struct {
	SessionID string
	Op        Op
	Path      Path
	Program   string
	Args      []string
	Write     bool
	Network   bool
}

// Decision 是判定结果;Reasons 可直接展示给用户/模型。
type Decision struct {
	Allow       bool
	Mode        Mode
	Reasons     []string
	Escalatable bool
}

// Facts 是「这次调用实际被施加了什么约束」。
type Facts struct {
	Backend  string
	Mode     Mode
	Roots    []string
	Enforced bool
}

// BackendInfo 是后端能力探测结果。
type BackendInfo struct {
	Name  string
	Probe error
	Kind  string // "os-level" | "policy-only"
}

// Sandbox 是判定契约。
type Sandbox interface {
	Mode(ctx context.Context, sessionID string) Mode
	Resolve(ctx context.Context, req Request) (Decision, error)
	Backends() []BackendInfo
	Facts(ctx context.Context, req Request) Facts
}

// Registry 是 seam 插件提供的服务值:provider 注册后端,消费者按名选择。
//
// 选择规则与注册顺序无关:默认取名字排序后的第一个后端。
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Sandbox
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Sandbox)}
}

// Register 注册一个后端;dispose 时注销。
func (r *Registry) Register(name string, impl Sandbox) (kernel.Dispose, error) {
	if name == "" || impl == nil {
		return nil, seamerr.New(seamerr.ErrInvalid, "sandbox", "后端名与实现不能为空", false)
	}
	r.mu.Lock()
	if _, exists := r.backends[name]; exists {
		r.mu.Unlock()
		return nil, seamerr.New(seamerr.ErrInvalid, name, "同名后端已注册", false)
	}
	r.backends[name] = impl
	r.mu.Unlock()
	return func(context.Context) error {
		r.mu.Lock()
		if cur, ok := r.backends[name]; ok && cur == impl {
			delete(r.backends, name)
		}
		r.mu.Unlock()
		return nil
	}, nil
}

// Names 返回已注册后端名(排序)。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.backends))
	for name := range r.backends {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Get 按名取后端;name 为空时取默认(名字排序第一个)。
func (r *Registry) Get(name string) (Sandbox, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		names := make([]string, 0, len(r.backends))
		for n := range r.backends {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, seamerr.Unimplemented("sandbox", "没有注册任何沙箱后端")
		}
		name = names[0]
	}
	impl, ok := r.backends[name]
	if !ok {
		return nil, seamerr.New(seamerr.ErrUnimplemented, name, "沙箱后端未注册", false)
	}
	return impl, nil
}

const pluginID = "seam-sandbox"

// Plugin 返回 seam/sandbox 插件:只提供 sandbox 服务(Registry),不做判定。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		_, err := ctx.Provide("sandbox", NewRegistry(), kernel.WithVersion(1), kernel.WithKind("object"))
		return nil, err
	}).WithProvide("sandbox")
}

// Describe 便于诊断/日志:输出「后端(kind, probe=...)」。
func Describe(info BackendInfo) string {
	probe := "ok"
	if info.Probe != nil {
		probe = "probe=" + info.Probe.Error()
	}
	return fmt.Sprintf("%s(%s,%s)", info.Name, info.Kind, probe)
}
