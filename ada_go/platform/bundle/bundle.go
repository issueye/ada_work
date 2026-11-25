// Package bundle 是 M4 的组合层(bundle / profile):数据,不是代码。
//
// 契约来源:docs/plugin-kernel-design.md §12.4。规则:
//  1. 同 id 的 config **整体替换**(不做深合并——深合并是配置事故高发区);
//  2. insert_before/after 只表达提示顺序,最终顺序由依赖图决定(本实现只告警并忽略);
//  3. profile 在 bundle 之上叠加,只允许按 id 打补丁。
package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/issueye/ada_go_v2/platform/expr"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/platform/version"
)

// Bundle 是一组插件条目。
type Bundle struct {
	Name    string  `json:"name"`
	Version string  `json:"version,omitempty"`
	Engine  string  `json:"engine,omitempty"`
	Entries []Entry `json:"entries"`

	Dir string `json:"-"`
}

// Entry 是 bundle 里的一个插件。
type Entry struct {
	ID       string         `json:"id"`
	Plugin   string         `json:"plugin"` // 插件目录(相对 bundle 文件或绝对路径)
	Config   map[string]any `json:"config,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`
	// When 是受限表达式;为假时该 entry 不参与装配(语法在解析期校验)。
	When string `json:"when,omitempty"`
}

// Profile 是在 bundle 之上按 id 叠加的补丁。
type Profile struct {
	Name    string       `json:"name"`
	Bundles []string     `json:"bundles"`
	Patch   []PatchEntry `json:"patch,omitempty"`

	Dir string `json:"-"`
}

// PatchEntry 是 profile 对某个 id 的覆盖。
type PatchEntry struct {
	ID           string         `json:"id"`
	Config       map[string]any `json:"config,omitempty"`
	Disabled     *bool          `json:"disabled,omitempty"`
	InsertBefore []string       `json:"insertBefore,omitempty"`
	InsertAfter  []string       `json:"insertAfter,omitempty"`
	// When 为假时整个 patch 不生效。
	When string `json:"when,omitempty"`
}

// Load 读取 bundle。
func Load(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bundle: 读取 %s 失败: %w", path, err)
	}
	b, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	b.Dir = filepath.Dir(path)
	return b, nil
}

// Parse 解析 bundle;未知字段直接报错。
func Parse(data []byte, dir string) (*Bundle, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("bundle: 解析失败: %w", err)
	}
	b.Dir = dir
	if b.Name == "" {
		return nil, fmt.Errorf("bundle: 缺少 name")
	}
	if len(b.Entries) == 0 {
		return nil, fmt.Errorf("bundle %q: entries 不能为空", b.Name)
	}
	if b.Engine != "" {
		if _, err := version.ParseConstraint(b.Engine); err != nil {
			return nil, fmt.Errorf("bundle %q: engine 约束非法: %w", b.Name, err)
		}
	}
	seen := map[string]bool{}
	for _, e := range b.Entries {
		if e.ID == "" {
			return nil, fmt.Errorf("bundle %q: entry 缺少 id", b.Name)
		}
		if e.Plugin == "" {
			return nil, fmt.Errorf("bundle %q: entry %q 缺少 plugin", b.Name, e.ID)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("bundle %q: entry id %q 重复", b.Name, e.ID)
		}
		seen[e.ID] = true
		if err := checkPluginPath(e.Plugin); err != nil {
			return nil, fmt.Errorf("bundle %q: entry %q: %w", b.Name, e.ID, err)
		}
		if e.When != "" {
			if err := expr.Validate(e.When); err != nil {
				return nil, fmt.Errorf("bundle %q: entry %q 的 when 非法: %w", b.Name, e.ID, err)
			}
		}
	}
	return &b, nil
}

// LoadProfile 读取 profile。
func LoadProfile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bundle: 读取 profile %s 失败: %w", path, err)
	}
	p, err := ParseProfile(data, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	p.Dir = filepath.Dir(path)
	return p, nil
}

// ParseProfile 解析 profile;未知字段直接报错。
func ParseProfile(data []byte, dir string) (*Profile, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("bundle: profile 解析失败: %w", err)
	}
	p.Dir = dir
	if p.Name == "" {
		return nil, fmt.Errorf("bundle: profile 缺少 name")
	}
	if len(p.Bundles) == 0 {
		return nil, fmt.Errorf("bundle: profile %q: bundles 不能为空", p.Name)
	}
	seen := map[string]bool{}
	for _, patch := range p.Patch {
		if patch.ID == "" {
			return nil, fmt.Errorf("bundle: profile %q: patch 缺少 id", p.Name)
		}
		if seen[patch.ID] {
			return nil, fmt.Errorf("bundle: profile %q: patch id %q 重复", p.Name, patch.ID)
		}
		seen[patch.ID] = true
		if patch.When != "" {
			if err := expr.Validate(patch.When); err != nil {
				return nil, fmt.Errorf("bundle: profile %q: patch %q 的 when 非法: %w", p.Name, patch.ID, err)
			}
		}
	}
	return &p, nil
}

// ExpandOptions 控制展开:表达式环境与宿主版本(用于 engine/engines 校验)。
//
// HostVersion 为空时跳过所有版本校验(测试/离线组装场景可用),但生产宿主应提供。
type ExpandOptions struct {
	Env         map[string]any
	HostVersion string
}

// Expand 加载 profile 引用的 bundle,按 id 合并并应用补丁;不提供表达式环境。
func (p *Profile) Expand() ([]*plugin.Manifest, []string, error) {
	return p.ExpandWithOptions(ExpandOptions{})
}

// ExpandWithEnv 是 Expand 的完整版本:env 提供表达式可读的值表。
//
// 约定命名空间(由调用方组装):env / os / profile / features;缺失路径视为 null。
func (p *Profile) ExpandWithEnv(env map[string]any) ([]*plugin.Manifest, []string, error) {
	return p.ExpandWithOptions(ExpandOptions{Env: env})
}

// ExpandWithOptions 是完整实现。
func (p *Profile) ExpandWithOptions(opts ExpandOptions) ([]*plugin.Manifest, []string, error) {
	env := opts.Env
	var (
		manifests []*plugin.Manifest
		warnings  []string
	)
	byID := map[string]*plugin.Manifest{}
	for _, ref := range p.Bundles {
		path := ref
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.Dir, path)
		}
		b, err := Load(path)
		if err != nil {
			return nil, nil, err
		}
		if b.Engine != "" && opts.HostVersion != "" {
			if err := version.Satisfy(b.Engine, opts.HostVersion); err != nil {
				return nil, nil, fmt.Errorf("bundle %q: %w", b.Name, err)
			}
		}
		for _, entry := range b.Entries {
			if entry.When != "" {
				ok, err := expr.Eval(entry.When, env)
				if err != nil {
					return nil, nil, fmt.Errorf("bundle: entry %q 的 when 求值失败: %w", entry.ID, err)
				}
				if !ok {
					warnings = append(warnings, fmt.Sprintf("bundle %q: entry %q 的 when 为假,已跳过", b.Name, entry.ID))
					continue
				}
			}
			if _, exists := byID[entry.ID]; exists {
				return nil, nil, fmt.Errorf("bundle: 多个 bundle 里出现重复 id %q", entry.ID)
			}
			m, err := loadEntry(b.Dir, entry, opts.HostVersion)
			if err != nil {
				return nil, nil, err
			}
			byID[entry.ID] = m
			manifests = append(manifests, m)
		}
	}

	for _, patch := range p.Patch {
		if patch.When != "" {
			ok, err := expr.Eval(patch.When, env)
			if err != nil {
				return nil, nil, fmt.Errorf("bundle: profile %q: patch %q 的 when 求值失败: %w", p.Name, patch.ID, err)
			}
			if !ok {
				warnings = append(warnings, fmt.Sprintf("profile %q: patch %q 的 when 为假,已跳过", p.Name, patch.ID))
				continue
			}
		}
		m, ok := byID[patch.ID]
		if !ok {
			return nil, nil, fmt.Errorf("bundle: profile %q 的 patch 指向不存在的 id %q", p.Name, patch.ID)
		}
		if patch.Config != nil {
			m.Config = patch.Config // 整体替换,不深合并
		}
		if patch.Disabled != nil {
			m.Disabled = *patch.Disabled
		}
		if len(patch.InsertBefore) > 0 || len(patch.InsertAfter) > 0 {
			warnings = append(warnings,
				fmt.Sprintf("profile %q: id %q 的 insert_before/after 只是提示顺序,已忽略(顺序由依赖图决定)", p.Name, patch.ID))
		}
	}
	return manifests, warnings, nil
}

func loadEntry(bundleDir string, entry Entry, hostVersion string) (*plugin.Manifest, error) {
	dir := entry.Plugin
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(bundleDir, dir)
	}
	manifests, err := plugin.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("bundle: 加载 entry %q(%s)失败: %w", entry.ID, entry.Plugin, err)
	}
	if len(manifests) != 1 {
		return nil, fmt.Errorf("bundle: entry %q 的目录必须恰好包含一个插件,实际 %d 个", entry.ID, len(manifests))
	}
	m := manifests[0]
	if m.ID != entry.ID {
		return nil, fmt.Errorf("bundle: entry id %q 与插件清单 id %q 不一致(暂不支持别名)", entry.ID, m.ID)
	}
	if hostVersion != "" {
		if constraint := m.Engines["ada"]; constraint != "" {
			if err := version.Satisfy(constraint, hostVersion); err != nil {
				return nil, fmt.Errorf("bundle: entry %q 的 engines 不满足: %w", entry.ID, err)
			}
		}
	}
	if entry.Config != nil {
		m.Config = entry.Config // 整体替换
	}
	m.Disabled = m.Disabled || entry.Disabled
	return m, nil
}

// checkPluginPath 拒绝相对路径逃逸(绝对路径允许,由宿主负责)。
func checkPluginPath(p string) error {
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("plugin 路径含 NUL")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) && (clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))) {
		return fmt.Errorf("plugin 路径不能逃逸 bundle 目录: %s", p)
	}
	return nil
}
