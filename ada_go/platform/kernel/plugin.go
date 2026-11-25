package kernel

import "github.com/issueye/ada_go_v2/platform/kernel/schema"

// Inject 声明插件的依赖。
//
// 为什么必需与可选要分开：必需依赖缺失时插件必须停在 pending（不能半启动），
// 可选依赖缺失时仍要工作，但提供者上线后要能触发重载（epoch 会变）。
type Inject struct {
	Names      []string
	Optional   []string
	MinVersion map[string]int
}

func (i Inject) has(name string) bool {
	for _, n := range i.Names {
		if n == name {
			return true
		}
	}
	for _, n := range i.Optional {
		if n == name {
			return true
		}
	}
	return false
}

func (i Inject) minVersion(name string) int {
	if i.MinVersion == nil {
		return 0
	}
	return i.MinVersion[name]
}

// Plugin 是内核认得的唯一插件形态。Go 插件与未来的 JS 适配器都实现它。
type Plugin interface {
	// Name 是稳定标识，用于诊断与服务归属。
	Name() string
	// Inject 声明依赖。
	Inject() Inject
	// Provide 声明本插件将提供的服务名（用于装配期预检）。
	Provide() []string
	// Config 返回配置 schema；nil 表示不接受配置。
	Config() *schema.Schema
	// Apply 装配插件：注册服务、事件、副作用，返回清理函数（可为 nil）。
	Apply(ctx *Context, cfg Config) (Dispose, error)
}

// FuncPlugin 是函数式 Go 插件的构造器，便于内置插件与测试。
type FuncPlugin struct {
	name    string
	inject  Inject
	provide []string
	schema  *schema.Schema
	apply   func(ctx *Context, cfg Config) (Dispose, error)
}

// Func 构造一个 Go 插件。
func Func(name string, apply func(ctx *Context, cfg Config) (Dispose, error)) *FuncPlugin {
	return &FuncPlugin{name: name, apply: apply}
}

// WithInject 声明必需依赖，可链式调用。
func (p *FuncPlugin) WithInject(names ...string) *FuncPlugin {
	p.inject.Names = append(p.inject.Names, names...)
	return p
}

// WithOptional 声明可选依赖。
func (p *FuncPlugin) WithOptional(names ...string) *FuncPlugin {
	p.inject.Optional = append(p.inject.Optional, names...)
	return p
}

// WithProvide 声明将提供的服务名。
func (p *FuncPlugin) WithProvide(names ...string) *FuncPlugin {
	p.provide = append(p.provide, names...)
	return p
}

// WithSchema 设置配置 schema。
func (p *FuncPlugin) WithSchema(s *schema.Schema) *FuncPlugin {
	p.schema = s
	return p
}

func (p *FuncPlugin) Name() string           { return p.name }
func (p *FuncPlugin) Inject() Inject         { return p.inject }
func (p *FuncPlugin) Provide() []string      { return p.provide }
func (p *FuncPlugin) Config() *schema.Schema { return p.schema }
func (p *FuncPlugin) Apply(ctx *Context, cfg Config) (Dispose, error) {
	if p.apply == nil {
		return nil, nil
	}
	return p.apply(ctx, cfg)
}
