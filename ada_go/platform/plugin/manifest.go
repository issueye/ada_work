// Package plugin 是 M3 的清单层:plugin.json 的解析与校验。
//
// 它只做「读清单 + 校验」:装载、签名、capability 分别属于 assemble 与 M5。
// 清单是数据契约(AGENTS P4「组合即产品」):产品差异靠它表达,不靠代码分支。
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/version"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 错误码:跨边界传递的错误必须带码,调用方才能区分「清单坏了」与「读不到文件」。
const (
	CodeIO       = "EPLUGINIO"
	CodeManifest = "EPLUGINMANIFEST"
)

// Error 是清单层的错误。
type Error struct {
	Code   string
	Plugin string // 已知插件 id 时填上,便于定位
	Msg    string
}

func (e *Error) Error() string {
	if e.Plugin != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Plugin, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Msg)
}

// Inject 声明依赖。必需缺失 → 不装配(plan 报错);可选缺失 → 照常装配(plan 只告警)。
type Inject struct {
	Required []string `json:"required,omitempty"`
	Optional []string `json:"optional,omitempty"`
}

// Manifest 是 plugin.json 的子集(design §9.2)。
//
// 未知字段一律报错:清单是契约,拼错的字段如果被静默忽略,作者会以为它生效了。
type Manifest struct {
	ID       string         `json:"id"`
	Name     string         `json:"name,omitempty"`
	Version  string         `json:"version"`
	Kind     string         `json:"kind"` // go | ts
	Entry    string         `json:"entry,omitempty"`
	Artifact string         `json:"artifact,omitempty"`
	Inject   Inject         `json:"inject"`
	Provide  []string       `json:"provide,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
	Disabled bool           `json:"disabled,omitempty"`

	// Caps 是插件声明的静态能力(design §13.2);Trust 是安装来源决定的信任等级。
	// 能力只在调用点由 caps.Broker 强制;清单声明本身不放行任何操作。
	Caps  []string `json:"caps,omitempty"`
	Trust string   `json:"trust,omitempty"`

	// Engines 是宿主版本约束(design §9.2),当前只认 "ada"。
	Engines map[string]string `json:"engines,omitempty"`
	// Activation 决定装配时机:startup(默认)或 on-demand(M4 延迟激活)。
	Activation string `json:"activation,omitempty"`

	// Dir 是清单所在的目录,由加载器填写,不来自 JSON。
	Dir string `json:"-"`
}

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
)

// Parse 解析一份清单。dir 用于回填来源目录(可为空)。
func Parse(data []byte, dir string) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, &Error{Code: CodeManifest, Msg: fmt.Sprintf("解析失败: %v", err)}
	}
	m.Dir = dir
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ApplySourceTrust 把「磁盘来源」的信任降到 third-party。
//
// 只对非 Go 插件生效:Go 插件代码已编译进宿主,其 trust 由内置注册表/宿主编译决定;
// TS 插件的 trust 不能由 plugin.json 自述提升(否则可以自称 builtin 拿 exec)。
func ApplySourceTrust(m *Manifest) {
	if m == nil {
		return
	}
	if m.Kind != "go" {
		m.Trust = string(caps.TrustThirdParty)
	}
}

// Load 读取 dir/plugin.json。
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: CodeIO, Msg: fmt.Sprintf("读取 %s 失败: %v", path, err)}
	}
	return Parse(data, dir)
}

// LoadDir 扫描 root 下的插件目录(每个子目录一份 plugin.json),按 id 排序返回。
//
// 若 root 自身就有 plugin.json,则当作单插件目录处理。
//
// 从磁盘加载的 TS 插件一律降为 third-party:清单里的 trust 是自述,
// 真正的 trust 只能由安装来源(签名/仓库/内置注册表)给出。
func LoadDir(root string) ([]*Manifest, error) {
	if _, err := os.Stat(filepath.Join(root, "plugin.json")); err == nil {
		m, err := Load(root)
		if err != nil {
			return nil, err
		}
		ApplySourceTrust(m)
		return []*Manifest{m}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, &Error{Code: CodeIO, Msg: fmt.Sprintf("读取目录 %s 失败: %v", root, err)}
	}
	var out []*Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err != nil {
			continue // 不是插件目录,跳过
		}
		m, err := Load(dir)
		if err != nil {
			return nil, err
		}
		ApplySourceTrust(m)
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, &Error{Code: CodeIO, Msg: fmt.Sprintf("目录 %s 下没有找到任何 plugin.json", root)}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Validate 校验清单自身的一致性。
func (m *Manifest) Validate() error {
	fail := func(format string, args ...any) error {
		return &Error{Code: CodeManifest, Plugin: m.ID, Msg: fmt.Sprintf(format, args...)}
	}
	if !idPattern.MatchString(m.ID) {
		return fail("id 必须是 kebab-case(例如 approval-gate),得到 %q", m.ID)
	}
	if !versionPattern.MatchString(m.Version) {
		return fail("version 必须是 semver(例如 0.1.0),得到 %q", m.Version)
	}
	switch m.Kind {
	case "go":
	case "ts":
		if m.Entry == "" && m.Artifact == "" {
			return fail("kind=ts 时必须声明 entry 或 artifact")
		}
	default:
		return fail("kind 必须是 go 或 ts,得到 %q", m.Kind)
	}

	seen := map[string]bool{}
	for _, name := range m.Provide {
		if strings.TrimSpace(name) == "" {
			return fail("provide 里不能有空服务名")
		}
		if seen[name] {
			return fail("provide 重复声明了 %q", name)
		}
		seen[name] = true
	}

	req := map[string]bool{}
	for _, name := range m.Inject.Required {
		if strings.TrimSpace(name) == "" {
			return fail("inject.required 里不能有空服务名")
		}
		if req[name] {
			return fail("inject.required 重复声明了 %q", name)
		}
		req[name] = true
		if seen[name] {
			return fail("同时 provide 和 require %q:这是自依赖,装配期必然成环", name)
		}
	}
	if m.Trust != "" && !caps.ValidTrust(caps.Trust(m.Trust)) {
		return fail("trust 非法(只允许 builtin/official/verified/third-party),得到 %q", m.Trust)
	}
	switch m.Activation {
	case "", "startup", "on-demand":
	default:
		return fail("activation 非法(只允许 startup/on-demand),得到 %q", m.Activation)
	}
	for name, constraint := range m.Engines {
		if name != "ada" {
			return fail("engines 目前只支持 ada,得到 %q", name)
		}
		if _, err := version.ParseConstraint(constraint); err != nil {
			return fail("engines.ada 约束非法: %v", err)
		}
	}
	for _, spec := range m.Caps {
		if _, err := caps.Parse(spec); err != nil {
			return fail("caps 非法: %v", err)
		}
	}
	opt := map[string]bool{}
	for _, name := range m.Inject.Optional {
		if strings.TrimSpace(name) == "" {
			return fail("inject.optional 里不能有空服务名")
		}
		if opt[name] {
			return fail("inject.optional 重复声明了 %q", name)
		}
		if req[name] {
			return fail("服务 %q 不能同时出现在 required 与 optional 里", name)
		}
		opt[name] = true
	}
	return nil
}

// String 便于日志与报告。
func (m *Manifest) String() string {
	return fmt.Sprintf("%s@%s(%s)", m.ID, m.Version, m.Kind)
}
