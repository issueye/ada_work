// Package fssandbox 是 fs 的沙箱策略装饰器:每次调用先问 sandbox 后端,再走实现。
//
// 三闸串联(seam-api-security §1.1):能力 → sandbox.Resolve → 装饰器执行。
// 本包只做第二闸的接线与失败关闭;模式与路径规则由 sandbox 后端统一给出,
// 避免 fs/shell 各自维护一套模式逻辑。
package fssandbox

import (
	"context"
	"strings"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
	seamsandbox "github.com/issueye/ada_go_v2/plugins/seam/sandbox"
)

// PolicyName 是插件 id。
const PolicyName = "fs-sandbox"

// Priority 是装饰器优先级:高者在外层(先判)。
const Priority = 100

// New 构造装饰器。registry 与 backend 懒取:装配顺序不影响正确性。
// approval 为 nil 表示没有审批服务:可升级的拒绝一律按拒绝处理(fail-closed)。
func New(inner seamfs.FS, registry *seamsandbox.Registry, backend string, approvalRegistry *seamapproval.Registry, approvalBackend string, emit func(string, any)) seamfs.FS {
	return &sandbox{inner: inner, registry: registry, backend: backend, approval: approvalRegistry, approvalBackend: approvalBackend, emit: emit}
}

type sandbox struct {
	inner           seamfs.FS
	registry        *seamsandbox.Registry
	backend         string
	approval        *seamapproval.Registry
	approvalBackend string
	emit            func(string, any)
}

func (s *sandbox) emitDecision(op seamsandbox.Op, p seamfs.Path, dec seamapproval.Decision, callErr error) {
	if s.emit == nil {
		return
	}
	allow := dec.Allow
	reason := dec.Reason
	if callErr != nil {
		reason = callErr.Error()
	}
	s.emit("approval/decided", map[string]any{
		"source": "fs", "op": string(op), "target": string(p),
		"allow": allow, "approver": dec.Approver, "reason": reason,
	})
}

func (s *sandbox) judge(ctx context.Context, op seamsandbox.Op, p seamfs.Path, write bool) error {
	if s.registry == nil {
		return seamfs.Denied("sandbox", "sandbox 服务不可用")
	}
	sb, err := s.registry.Get(s.backend)
	if err != nil {
		return seamfs.Denied("sandbox", "没有可用的沙箱后端: "+err.Error())
	}
	decision, err := sb.Resolve(ctx, seamsandbox.Request{Op: op, Path: seamsandbox.Path(p), Write: write})
	if err != nil {
		return err
	}
	if decision.Allow {
		return nil
	}
	reason := strings.Join(decision.Reasons, "; ")
	if reason == "" {
		reason = "被沙箱拒绝"
	}
	// 可升级的拒绝:问审批;没有审批服务/审批人报错/拒绝 → 仍然拒绝(fail-closed)。
	if decision.Escalatable && s.approval != nil {
		dec, err := seamapproval.Ask(ctx, s.approval, s.approvalBackend, seamapproval.Request{
			Source: "fs", Op: string(op), Target: string(p), Reason: reason,
		})
		s.emitDecision(op, p, dec, err)
		if err != nil {
			return seamfs.Denied("approval", "审批失败: "+err.Error())
		}
		if dec.Allow {
			return nil
		}
		return seamfs.Denied("approval", dec.Reason)
	}
	return seamfs.Denied("sandbox", reason)
}

func (s *sandbox) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	if err := s.judge(ctx, seamsandbox.OpFSRead, p, false); err != nil {
		return seamfs.FileInfo{}, err
	}
	return s.inner.Stat(ctx, p)
}

func (s *sandbox) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	if err := s.judge(ctx, seamsandbox.OpFSList, req.Path, false); err != nil {
		return nil, err
	}
	return s.inner.List(ctx, req)
}

func (s *sandbox) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	if err := s.judge(ctx, seamsandbox.OpFSRead, req.Path, false); err != nil {
		return seamfs.ReadResult{}, err
	}
	return s.inner.Read(ctx, req)
}

func (s *sandbox) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	if err := s.judge(ctx, seamsandbox.OpFSRead, req.Path, false); err != nil {
		return seamfs.SearchResult{}, err
	}
	return s.inner.Search(ctx, req)
}

func (s *sandbox) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	if err := s.judge(ctx, seamsandbox.OpFSWrite, req.Path, true); err != nil {
		return seamfs.WriteResult{}, err
	}
	return s.inner.Write(ctx, req)
}

func (s *sandbox) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	if err := s.judge(ctx, seamsandbox.OpFSEdit, req.Path, true); err != nil {
		return seamfs.EditResult{}, err
	}
	return s.inner.Edit(ctx, req)
}

// Plugin 返回 fs-sandbox 插件:inject fs 与 sandbox,注册沙箱装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		registry, err := kernel.Service[*seamsandbox.Registry](ctx, "sandbox")
		if err != nil {
			return nil, err
		}
		backend := cfg.String("backend")
		var approvalRegistry *seamapproval.Registry
		if v, ok := ctx.TryService("approval"); ok {
			if reg, ok := v.(*seamapproval.Registry); ok {
				approvalRegistry = reg
			}
		}
		return chain.Decorate(func(inner seamfs.FS) seamfs.FS {
			return New(inner, registry, backend, approvalRegistry, cfg.String("approvalBackend"), func(name string, payload any) {
				ctx.EmitContext(context.Background(), name, payload)
			})
		}, Priority)
	}).WithInject("fs", "sandbox").WithOptional("approval")
}
