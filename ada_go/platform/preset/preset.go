// Package preset 是 M3 的组合数据层:一份 preset 声明「装哪些目录、装哪些插件
// (enable)、哪些 disabled、每个插件的配置怎么覆盖」。
//
// 它是**数据不是代码**(AGENTS §2.2 的组合数据类):产品差异靠换 preset 表达,
// 不允许在代码里写 `if product == "..."`。patch 只允许覆盖清单的 config,
// 不能改 id/kind/inject/provide——那些改的是插件身份与依赖图,不是参数。
package preset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/issueye/ada_go_v2/platform/plugin"
)

// CodePreset 是 preset 层的错误码。
const CodePreset = "EPRESET"

// Error 是 preset 层的错误。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Msg) }

// Preset 是一份组合声明。路径相对 preset 文件所在目录解析。
type Preset struct {
	Name    string                    `json:"name"`
	Plugins []string                  `json:"plugins"`
	Enable  []string                  `json:"enable,omitempty"`
	Disable []string                  `json:"disable,omitempty"`
	Patch   map[string]map[string]any `json:"patch,omitempty"`

	// Dir 是 preset 文件所在目录,由加载器填写。
	Dir string `json:"-"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load 读取并校验一份 preset。
func Load(path string) (*Preset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("读取 %s 失败: %v", path, err)}
	}
	p, err := Parse(data, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Parse 解析 preset。未知字段一律报错:数据契约的拼写错误必须暴露出来。
func Parse(data []byte, dir string) (*Preset, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Preset
	if err := dec.Decode(&p); err != nil {
		return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("解析失败: %v", err)}
	}
	p.Dir = dir
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate 校验 preset 自身的一致性。
func (p *Preset) Validate() error {
	fail := func(format string, args ...any) error {
		return &Error{Code: CodePreset, Msg: fmt.Sprintf(format, args...)}
	}
	if !namePattern.MatchString(p.Name) {
		return fail("name 必须是 kebab-case,得到 %q", p.Name)
	}
	if len(p.Plugins) == 0 {
		return fail("plugins 不能为空")
	}
	for _, dir := range p.Plugins {
		if strings.TrimSpace(dir) == "" {
			return fail("plugins 里不能有空路径")
		}
	}
	seen := map[string]bool{}
	for _, id := range p.Disable {
		if !namePattern.MatchString(id) {
			return fail("disable 里的 id 不合法: %q", id)
		}
		if seen[id] {
			return fail("disable 重复声明了 %q", id)
		}
		seen[id] = true
	}
	// enable 是候选白名单:不在名单里的插件不进入装配。
	// 同一个插件可以同时出现在 enable 与 disable:enable 表示「目录里收下它」,
	// disable 表示「默认不启用」——宿主随后可以在运行期决定是否打开(如切换 LLM 供应商)。
	enabled := map[string]bool{}
	for _, id := range p.Enable {
		if !namePattern.MatchString(id) {
			return fail("enable 里的 id 不合法: %q", id)
		}
		if enabled[id] {
			return fail("enable 重复声明了 %q", id)
		}
		enabled[id] = true
	}
	for id := range p.Patch {
		if !namePattern.MatchString(id) {
			return fail("patch 里的 id 不合法: %q", id)
		}
	}
	return nil
}

// Expand 加载 preset 引用的全部插件目录,应用 enable 白名单、disable 与 patch。
//
// 未知的 enable/disable/patch id 一律报错:静默忽略会让作者以为配置生效了。
func (p *Preset) Expand() ([]*plugin.Manifest, error) {
	var manifests []*plugin.Manifest
	for _, dir := range p.Plugins {
		resolved := dir
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(p.Dir, resolved)
		}
		ms, err := plugin.LoadDir(resolved)
		if err != nil {
			return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("加载 %s 失败: %v", resolved, err)}
		}
		manifests = append(manifests, ms...)
	}

	byID := make(map[string]*plugin.Manifest, len(manifests))
	for _, m := range manifests {
		if _, ok := byID[m.ID]; ok {
			return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("插件 id %q 在多个目录里重复出现", m.ID)}
		}
		byID[m.ID] = m
	}

	// enable 是白名单:不在名单里的清单一律不进入装配,也不标记 Disabled——
	// 标记会让 plan 输出几十条「已被 disabled」噪声,而这里表达的是「根本不在候选集」。
	// 目录可以扫得很宽(整个内置插件目录),装配集合由 preset 显式收口。
	if len(p.Enable) > 0 {
		allow := make(map[string]bool, len(p.Enable))
		for _, id := range p.Enable {
			if _, ok := byID[id]; !ok {
				return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("enable 指向不存在的插件 %q", id)}
			}
			allow[id] = true
		}
		kept := make([]*plugin.Manifest, 0, len(allow))
		for _, m := range manifests {
			if allow[m.ID] {
				kept = append(kept, m)
			}
		}
		manifests = kept
		byID = make(map[string]*plugin.Manifest, len(kept))
		for _, m := range kept {
			byID[m.ID] = m
		}
	}

	for _, id := range p.Disable {
		m, ok := byID[id]
		if !ok {
			return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("disable 指向不存在的插件 %q", id)}
		}
		m.Disabled = true
	}

	ids := make([]string, 0, len(p.Patch))
	for id := range p.Patch {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		m, ok := byID[id]
		if !ok {
			return nil, &Error{Code: CodePreset, Msg: fmt.Sprintf("patch 指向不存在的插件 %q", id)}
		}
		m.Config = mergeConfig(m.Config, p.Patch[id])
	}
	return manifests, nil
}

// mergeConfig 深合并:对象递归合并,其余值直接覆盖。
//
// 为什么递归而不是整体替换:patch 应当只写「与默认不同的那个键」,
// 整体替换会逼作者复制全部默认值,默认值一改就漂移。
func mergeConfig(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := out[k].(map[string]any); ok {
				out[k] = mergeConfig(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}
