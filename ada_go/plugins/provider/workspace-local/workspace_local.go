// Package workspacelocal 是 workspace seam 的本地目录实现:多工作区根、
// 虚拟路径 /workspace 解析、只读授权与符号链接越界检查。
//
// 它不打开文件(打开属于 fs-local),只负责把虚拟路径换成「已验证仍在工作区根内」
// 的宿主路径;因此 Host 字段只允许 fs/shell provider 使用。
package workspacelocal

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// ProviderName 是 provider 名与插件 id。
const ProviderName = "workspace-local"

// idPattern 限制工作区 ID 为 kebab-case,避免它变成路径或事件名的一部分。
var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Workspace 是一条工作区配置。
type Workspace struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Root     string   `json:"root"`
	ReadOnly bool     `json:"readOnly,omitempty"`
	Ignore   []string `json:"ignore,omitempty"`
}

// Options 是 provider 配置。
type Options struct {
	Active     string      `json:"active,omitempty"`
	Workspaces []Workspace `json:"workspaces"`
}

// Emit 是总线事件回调;为 nil 时只做状态变更(测试/无总线场景)。
type Emit func(ctx context.Context, name string, payload any)

// entry 是校验后的内部记录;root 已 Abspath + EvalSymlinks。
type entry struct {
	info workspace.Info
	root string
}

// Provider 实现 workspace.Service。
type Provider struct {
	mu     sync.RWMutex
	active workspace.ID
	items  map[workspace.ID]*entry
	order  []workspace.ID
	emit   Emit
}

// New 校验并构造 provider;任何配置问题都在装配期报错,不半启动。
func New(opts Options, emit Emit) (*Provider, error) {
	if len(opts.Workspaces) == 0 {
		return nil, workspace.NoWorkspace()
	}
	p := &Provider{items: make(map[workspace.ID]*entry, len(opts.Workspaces)), emit: emit}
	for _, w := range opts.Workspaces {
		e, err := newEntry(w)
		if err != nil {
			return nil, err
		}
		if _, ok := p.items[e.info.ID]; ok {
			return nil, workspace.Invalid("workspace-local: 工作区 ID 重复: " + string(e.info.ID))
		}
		p.items[e.info.ID] = e
		p.order = append(p.order, e.info.ID)
	}
	sort.Slice(p.order, func(i, j int) bool { return p.order[i] < p.order[j] })

	p.active = workspace.ID(strings.TrimSpace(opts.Active))
	if p.active == "" {
		// 缺省用 ID 字典序第一个,而不是配置数组顺序;装配顺序不该改变行为。
		p.active = p.order[0]
	}
	if _, ok := p.items[p.active]; !ok {
		return nil, workspace.Invalid("workspace-local: active 指向不存在的工作区: " + string(p.active))
	}
	return p, nil
}

// newEntry 校验单条配置并规范化根。
func newEntry(w Workspace) (*entry, error) {
	id := workspace.ID(strings.TrimSpace(w.ID))
	if id == "" {
		return nil, workspace.Invalid("workspace-local: 工作区 id 不能为空")
	}
	if !idPattern.MatchString(string(id)) {
		return nil, workspace.Invalid("workspace-local: 工作区 id 必须是 kebab-case: " + string(id))
	}
	if strings.TrimSpace(w.Root) == "" {
		return nil, workspace.Invalid("workspace-local: 工作区根不能为空: " + string(id))
	}
	root, err := filepath.Abs(w.Root)
	if err != nil {
		return nil, workspace.Invalid(fmt.Sprintf("workspace-local: 工作区根非法(%s): %v", id, err))
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, workspace.Invalid(fmt.Sprintf("workspace-local: 工作区根不可用(%s): %v", id, err))
	}
	if !st.IsDir() {
		return nil, workspace.Invalid("workspace-local: 工作区根不是目录: " + root)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, workspace.Invalid(fmt.Sprintf("workspace-local: 解析工作区根失败(%s): %v", id, err))
	}
	name := strings.TrimSpace(w.Name)
	if name == "" {
		name = string(id)
	}
	return &entry{
		info: workspace.Info{
			ID: id, Name: name, Mount: workspace.MountRoot,
			ReadOnly: w.ReadOnly, Ignore: append([]string(nil), w.Ignore...),
		},
		root: resolved,
	}, nil
}

// Current 返回当前工作区。
func (p *Provider) Current(context.Context) (workspace.Info, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.items[p.active]
	if !ok {
		return workspace.Info{}, workspace.NoWorkspace()
	}
	return cloneInfo(e.info), nil
}

// Get 按 ID 取工作区;未知 ID 不静默回退。
func (p *Provider) Get(_ context.Context, id workspace.ID) (workspace.Info, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.items[id]
	if !ok {
		return workspace.Info{}, workspace.NotFound(id)
	}
	return cloneInfo(e.info), nil
}

// List 返回全部工作区,按 ID 排序。
func (p *Provider) List(context.Context) ([]workspace.Info, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]workspace.Info, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, cloneInfo(p.items[id].info))
	}
	return out, nil
}

// Switch 切换当前工作区;已有会话绑定不受影响,事件只在真正变化时发。
func (p *Provider) Switch(ctx context.Context, id workspace.ID) (workspace.Info, error) {
	p.mu.Lock()
	e, ok := p.items[id]
	if !ok {
		p.mu.Unlock()
		return workspace.Info{}, workspace.NotFound(id)
	}
	prev := p.active
	p.active = id
	p.mu.Unlock()

	if prev != id && p.emit != nil {
		p.emit(ctx, workspace.EventChanged, map[string]any{
			"previous": string(prev), "current": string(id),
		})
	}
	return cloneInfo(e.info), nil
}

// ResolveHost 选定工作区、校验虚拟路径与只读授权,再返回根内宿主路径。
func (p *Provider) ResolveHost(ctx context.Context, op workspace.Op, virtual string) (workspace.Target, error) {
	if !op.Valid() {
		return workspace.Target{}, workspace.Invalid("workspace-local: 未知操作: " + string(op))
	}
	clean, err := normalizeVirtual(virtual)
	if err != nil {
		return workspace.Target{}, err
	}

	p.mu.RLock()
	id := p.active
	if b, ok := workspace.BindingFrom(ctx); ok {
		id = b.Workspace
	}
	e, ok := p.items[id]
	p.mu.RUnlock()
	if !ok {
		return workspace.Target{}, workspace.NotFound(id)
	}
	if e.info.ReadOnly && op.Mutating() {
		return workspace.Target{}, workspace.Denied("workspace", fmt.Sprintf("工作区 %s 只读,拒绝 %s", id, op))
	}

	rel := strings.TrimPrefix(clean, workspace.MountRoot)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		rel = "."
	}
	host := filepath.Join(e.root, filepath.FromSlash(rel))
	target, err := secureTarget(e.root, host)
	if err != nil {
		return workspace.Target{}, err
	}
	return workspace.Target{Workspace: id, Virtual: clean, Rel: rel, Host: target, Root: e.root}, nil
}

// normalizeVirtual 做虚拟路径的第一层拒绝:空、NUL、反斜杠、非 /workspace 根。
func normalizeVirtual(p string) (string, error) {
	if p == "" {
		return "", workspace.Invalid("workspace-local: 路径不能为空")
	}
	if strings.ContainsRune(p, 0) {
		return "", workspace.Invalid("workspace-local: 路径包含 NUL")
	}
	if strings.Contains(p, `\`) {
		return "", workspace.Invalid("workspace-local: 路径不能包含反斜杠")
	}
	if !strings.HasPrefix(p, "/") {
		return "", workspace.Invalid("workspace-local: 虚拟路径必须以 / 开头")
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", workspace.Denied("workspace", "路径越界: "+p)
	}
	if clean != workspace.MountRoot && !strings.HasPrefix(clean, workspace.MountRoot+"/") {
		return "", workspace.Denied("workspace", "不支持的虚拟根: "+clean)
	}
	return clean, nil
}

// secureTarget 解析符号链接,并确保最终路径仍在 root 内。
//
// 目标不存在时解析「最深的已存在祖先」,再把缺失段拼回;否则新文件的父目录
// 是指向根外的链接时会被漏掉。
func secureTarget(root, host string) (string, error) {
	if target, err := filepath.EvalSymlinks(host); err == nil {
		if !within(root, target) {
			return "", workspace.Denied("workspace", "路径越界或符号链接指向根外")
		}
		return target, nil
	}

	cur := host
	var missing []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		missing = append([]string{filepath.Base(cur)}, missing...)
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			target := resolved
			for _, part := range missing {
				target = filepath.Join(target, part)
			}
			if !within(root, target) {
				return "", workspace.Denied("workspace", "路径越界或符号链接指向根外")
			}
			return target, nil
		}
	}
	return "", workspace.Denied("workspace", "无法解析路径")
}

// within 判断 target 是否在 root 内(含 root 本身)。
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func cloneInfo(info workspace.Info) workspace.Info {
	info.Ignore = append([]string(nil), info.Ignore...)
	return info
}

// Plugin 返回 workspace-local 插件:inject workspace seam,安装本地实现。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*workspace.Chain](ctx, workspace.ServiceName)
		if err != nil {
			return nil, err
		}
		var opts Options
		if err := cfg.Decode(&opts); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "workspace-local: 解析配置失败: %v", err)
		}
		impl, err := New(opts, func(c context.Context, name string, payload any) {
			ctx.EmitContext(c, name, payload)
		})
		if err != nil {
			return nil, err
		}
		return chain.Install(impl)
	}).WithInject(workspace.ServiceName)
}
