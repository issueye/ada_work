// Package fsobserve 是 fs 的观察策略:每次操作后广播 fs/observe 事件。
//
// 观察不是门禁:它不拒绝任何操作,失败的操作也会被记录(用于审计)。
// 放在 priority=250(最外层),因此能看到 caps-gate/sandbox 的拒绝结果。
package fsobserve

import (
	"context"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

// PolicyName 是插件 id。
const PolicyName = "fs-observe"

// Priority 高于 caps-gate(200),保证记录到被拒绝的调用。
const Priority = 250

// Record 是一次 fs 操作的观察记录。
type Record struct {
	Op     string
	Path   string
	Bytes  int64
	Err    string
	Plugin string
}

// Emitter 接收观察记录;插件实现里转成 fs/observe 事件。
type Emitter func(context.Context, Record)

// New 构造观察装饰器;emit 为 nil 时只透传。
func New(inner seamfs.FS, emit Emitter, skipOps []string) seamfs.FS {
	skip := make(map[string]bool, len(skipOps))
	for _, op := range skipOps {
		skip[op] = true
	}
	return &observer{inner: inner, emit: emit, skip: skip}
}

type observer struct {
	inner seamfs.FS
	emit  Emitter
	skip  map[string]bool
}

func (o *observer) record(ctx context.Context, op, path string, bytes int64, err error) {
	if o.emit == nil || o.skip[op] {
		return
	}
	plugin := ""
	if id, ok := kernel.Principal(ctx); ok {
		plugin = id
	}
	rec := Record{Op: op, Path: path, Bytes: bytes, Plugin: plugin}
	if err != nil {
		rec.Err = err.Error()
	}
	o.emit(ctx, rec)
}

func (o *observer) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	info, err := o.inner.Stat(ctx, p)
	o.record(ctx, "stat", string(p), info.Size, err)
	return info, err
}

func (o *observer) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	entries, err := o.inner.List(ctx, req)
	o.record(ctx, "list", string(req.Path), int64(len(entries)), err)
	return entries, err
}

func (o *observer) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	res, err := o.inner.Read(ctx, req)
	o.record(ctx, "read", string(req.Path), int64(len(res.Content)), err)
	return res, err
}

func (o *observer) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	res, err := o.inner.Write(ctx, req)
	o.record(ctx, "write", string(req.Path), int64(len(req.Content)), err)
	return res, err
}

func (o *observer) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	res, err := o.inner.Edit(ctx, req)
	o.record(ctx, "edit", string(req.Path), 0, err)
	return res, err
}

func (o *observer) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	res, err := o.inner.Search(ctx, req)
	o.record(ctx, "search", string(req.Path), 0, err)
	return res, err
}

type config struct {
	SkipOps []string `json:"skipOps,omitempty"`
}

// Plugin 返回 fs-observe 插件:inject fs seam,注册观察装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "fs-observe: 解析配置失败: %v", err)
		}
		emit := func(callCtx context.Context, rec Record) {
			payload := map[string]any{
				"op": rec.Op, "path": rec.Path, "bytes": rec.Bytes,
				"plugin": rec.Plugin,
			}
			if rec.Err != "" {
				payload["error"] = rec.Err
			}
			ctx.Emit(seamfs.EventObserve, payload)
		}
		return chain.Decorate(func(inner seamfs.FS) seamfs.FS {
			return New(inner, emit, conf.SkipOps)
		}, Priority)
	}).WithInject("fs")
}
