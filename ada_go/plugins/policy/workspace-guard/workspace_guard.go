// Package workspaceguard 是 workspace 的路径策略装饰器:按工作区相对路径拒绝
// 指定读/写/执行。它只做确定性拒绝,不拥有根、不打开文件。
//
// 模式语法 v1:不含通配符时按路径段前缀匹配;含通配符时按 path.Match 单段匹配。
// 不支持 gitignore 的 ! 取反、** 跨段与目录继承,需要完整语义时先扩契约。
package workspaceguard

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/workspace"
)

// PolicyName 是插件 id。
const PolicyName = "workspace-guard"

// Priority 是装饰器优先级:高者在外层(先判)。
const Priority = 100

// Config 是策略配置。
type Config struct {
	DenyWrite []string `json:"denyWrite,omitempty"`
	DenyRead  []string `json:"denyRead,omitempty"`
	DenyExec  []string `json:"denyExec,omitempty"`
}

// Emit 是 workspace/denied 观察事件回调;为 nil 时不发事件。
type Emit func(ctx context.Context, name string, payload any)

// New 构造装饰器。
func New(inner workspace.Service, conf Config, emit Emit) workspace.Service {
	return &guard{Service: inner, conf: conf, emit: emit}
}

// guard 内嵌 Service,只覆盖 ResolveHost;其余查询原样转发。
type guard struct {
	workspace.Service
	conf Config
	emit Emit
}

// ResolveHost 先让内层完成路径与只读授权,再按相对路径做策略拒绝。
func (g *guard) ResolveHost(ctx context.Context, op workspace.Op, p string) (workspace.Target, error) {
	target, err := g.Service.ResolveHost(ctx, op, p)
	if err != nil {
		return workspace.Target{}, err
	}
	patterns := g.patternsFor(op)
	for _, pattern := range patterns {
		if match(pattern, target.Rel) {
			if g.emit != nil {
				g.emit(ctx, workspace.EventDenied, map[string]any{
					"op": string(op), "workspace": string(target.Workspace),
					"path": target.Virtual, "source": "policy",
				})
			}
			return workspace.Target{}, workspace.Denied("policy", fmt.Sprintf("路径 %s 被策略拒绝(%s)", target.Virtual, op))
		}
	}
	return target, nil
}

// Locate 把按宿主路径定位工作区的能力转发给内层;内层不支持时返回 found=false,
// 而不是假装定位成功。装饰器不改变「根属于宿主」的边界。
func (g *guard) Locate(ctx context.Context, hostPath string) (workspace.Info, bool, error) {
	if l, ok := g.Service.(workspace.Locator); ok {
		return l.Locate(ctx, hostPath)
	}
	return workspace.Info{}, false, nil
}

func (g *guard) patternsFor(op workspace.Op) []string {
	switch op {
	case workspace.OpWrite, workspace.OpEdit:
		return g.conf.DenyWrite
	case workspace.OpExec:
		return g.conf.DenyExec
	default:
		return g.conf.DenyRead
	}
}

// match 做路径段前缀或单段 glob 匹配;pattern 已按 slash 归一化。
func match(pattern, rel string) bool {
	pattern = strings.Trim(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return false
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return rel == pattern || strings.HasPrefix(rel, pattern+"/")
	}
	ok, err := path.Match(pattern, rel)
	return err == nil && ok
}

// Plugin 返回 workspace-guard 插件:inject workspace,注册路径拒绝装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*workspace.Chain](ctx, workspace.ServiceName)
		if err != nil {
			return nil, err
		}
		var conf Config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "workspace-guard: 解析配置失败: %v", err)
		}
		return chain.Decorate(func(inner workspace.Service) workspace.Service {
			return New(inner, conf, func(c context.Context, name string, payload any) {
				ctx.EmitContext(c, name, payload)
			})
		}, Priority)
	}).WithInject(workspace.ServiceName)
}
