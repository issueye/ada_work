// Package capsgate 是 fs/shell 的能力授权装饰器(三闸中的第一闸)。
//
// 它从 context 读取调用者插件 id(内核提供的 principal),向 caps.Broker 查授权;
// 缺少身份、没有授权、能力名非法一律拒绝(fail-closed)。
// 放在 priority=200,先于 sandbox(100)判定。
package capsgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamshell "github.com/issueye/ada_go_v2/plugins/seam/shell"
)

// PolicyName 是插件 id。
const PolicyName = "caps-gate"

// Priority 高于 sandbox(100):能力授权先于沙箱判定。
const Priority = 200

// NewFS 构造 fs 能力装饰器(测试可直接调用)。
func NewFS(inner seamfs.FS, broker *caps.Broker) seamfs.FS {
	return &fsGate{inner: inner, broker: broker}
}

// NewShell 构造 shell 能力装饰器。
func NewShell(inner seamshell.Shell, broker *caps.Broker) seamshell.Shell {
	return &shellGate{inner: inner, broker: broker}
}

type fsGate struct {
	inner  seamfs.FS
	broker *caps.Broker
}

func (g *fsGate) judge(ctx context.Context, spec, target string) error {
	id, ok := kernel.Principal(ctx)
	if !ok {
		return seamfs.Denied("cap", "缺少调用者身份:请使用 ctx.Context() 携带 principal")
	}
	if err := g.broker.Authorize(ctx, id, spec, target); err != nil {
		return seamfs.Denied("cap", err.Error())
	}
	return nil
}

func (g *fsGate) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	if err := g.judge(ctx, "fs.read", string(p)); err != nil {
		return seamfs.FileInfo{}, err
	}
	return g.inner.Stat(ctx, p)
}

func (g *fsGate) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	if err := g.judge(ctx, "fs.read", string(req.Path)); err != nil {
		return nil, err
	}
	return g.inner.List(ctx, req)
}

func (g *fsGate) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	if err := g.judge(ctx, "fs.read", string(req.Path)); err != nil {
		return seamfs.ReadResult{}, err
	}
	return g.inner.Read(ctx, req)
}

func (g *fsGate) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	if err := g.judge(ctx, "fs.read", string(req.Path)); err != nil {
		return seamfs.SearchResult{}, err
	}
	return g.inner.Search(ctx, req)
}

func (g *fsGate) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	if err := g.judge(ctx, "fs.write", string(req.Path)); err != nil {
		return seamfs.WriteResult{}, err
	}
	return g.inner.Write(ctx, req)
}

func (g *fsGate) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	if err := g.judge(ctx, "fs.write", string(req.Path)); err != nil {
		return seamfs.EditResult{}, err
	}
	return g.inner.Edit(ctx, req)
}

type shellGate struct {
	inner  seamshell.Shell
	broker *caps.Broker
}

func (g *shellGate) judge(ctx context.Context, program string) error {
	id, ok := kernel.Principal(ctx)
	if !ok {
		return seamshell.Denied("cap", "缺少调用者身份:请使用 ctx.Context() 携带 principal")
	}
	if err := g.broker.Authorize(ctx, id, "exec", program); err != nil {
		return seamshell.Denied("cap", err.Error())
	}
	return nil
}

func (g *shellGate) Run(ctx context.Context, cmd seamshell.Command) (seamshell.Result, error) {
	if err := g.judge(ctx, cmd.Program); err != nil {
		return seamshell.Result{}, err
	}
	return g.inner.Run(ctx, cmd)
}

func (g *shellGate) Stream(ctx context.Context, cmd seamshell.Command, onChunk func(seamshell.Chunk) error) (seamshell.Result, error) {
	if err := g.judge(ctx, cmd.Program); err != nil {
		return seamshell.Result{}, err
	}
	return g.inner.Stream(ctx, cmd, onChunk)
}

func (g *shellGate) Env() seamshell.Env { return g.inner.Env() }

type grantsConfig struct {
	Grants map[string][]string `json:"grants"`
}

// Plugin 返回 caps-gate 插件:可选 inject fs / shell,按配置的 grants 强制能力授权。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		var conf grantsConfig
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "caps-gate: 解析 grants 失败: %v", err)
		}
		// 优先使用注入的 caps 服务(caps-static 提供),这样宿主可以把清单 caps
		// 与 install 的 trust 灌进同一个 Broker;没有服务时才退回 config grants。
		broker := caps.NewBroker(func(d caps.Deny) { ctx.Emit(caps.EventDeny, d) })
		fromService := false
		if v, ok := ctx.TryService("caps"); ok {
			if b, ok := v.(*caps.Broker); ok {
				broker = b
				fromService = true
			}
		}
		if !fromService {
			for plugin, list := range conf.Grants {
				if err := broker.Grant(plugin, list...); err != nil {
					return nil, kernel.Fail(kernel.CodeInvalid, "caps-gate: 授予 %q 失败: %v", plugin, err)
				}
			}
		}

		var disposers []kernel.Dispose
		if v, ok := ctx.TryService("fs"); ok {
			if chain, ok := v.(*seamfs.Chain); ok {
				d, err := chain.Decorate(func(inner seamfs.FS) seamfs.FS {
					return NewFS(inner, broker)
				}, Priority)
				if err != nil {
					return nil, err
				}
				disposers = append(disposers, d)
			}
		}
		if v, ok := ctx.TryService("shell"); ok {
			if chain, ok := v.(*seamshell.Chain); ok {
				d, err := chain.Decorate(func(inner seamshell.Shell) seamshell.Shell {
					return NewShell(inner, broker)
				}, Priority)
				if err != nil {
					return nil, err
				}
				disposers = append(disposers, d)
			}
		}
		if len(disposers) == 0 {
			return nil, fmt.Errorf("caps-gate: 需要 fs 或 shell 服务(当前链上都没有)")
		}
		return func(ctx context.Context) error {
			var errs []error
			for i := len(disposers) - 1; i >= 0; i-- {
				if err := disposers[i](ctx); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}, nil
	}).WithOptional("caps", "fs", "shell")
}
