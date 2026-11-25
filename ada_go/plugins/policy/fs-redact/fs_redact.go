// Package fsredact 是 fs 的脱敏策略装饰器:读结果按正则替换敏感信息。
//
// 它只观察/改写读结果,不做门禁(门禁在 caps-gate/sandbox)。放在 priority=150:
// 在 sandbox(100)之外、caps-gate(200)之内。
//
// 模式表与编译逻辑在 platform/redact:同一个语义也被 shell-redact 使用,
// 两边各存一份会让新增密钥模式变成两处改动。
package fsredact

import (
	"context"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/redact"
	seamfs "github.com/issueye/ada_go_v2/plugins/seam/fs"
)

// PolicyName 是插件 id。
const PolicyName = "fs-redact"

// Priority 是装饰器优先级。
const Priority = 150

// DefaultReplacement 是默认替换文本(与 platform/redact 同源)。
const DefaultReplacement = redact.DefaultReplacement

// New 构造脱敏装饰器;patterns 为空时用默认模式。
func New(inner seamfs.FS, patterns []string, replacement string) (seamfs.FS, error) {
	rd, err := redact.New(patterns, replacement)
	if err != nil {
		return nil, fmt.Errorf("fs-redact: %w", err)
	}
	return &redactor{inner: inner, rd: rd}, nil
}

type redactor struct {
	inner seamfs.FS
	rd    *redact.Redactor
}

func (r *redactor) Read(ctx context.Context, req seamfs.ReadRequest) (seamfs.ReadResult, error) {
	res, err := r.inner.Read(ctx, req)
	if err != nil {
		return res, err
	}
	res.Content = r.rd.Apply(res.Content)
	return res, nil
}

func (r *redactor) Stat(ctx context.Context, p seamfs.Path) (seamfs.FileInfo, error) {
	return r.inner.Stat(ctx, p)
}
func (r *redactor) List(ctx context.Context, req seamfs.ListRequest) ([]seamfs.DirEntry, error) {
	return r.inner.List(ctx, req)
}
func (r *redactor) Write(ctx context.Context, req seamfs.WriteRequest) (seamfs.WriteResult, error) {
	return r.inner.Write(ctx, req)
}
func (r *redactor) Edit(ctx context.Context, req seamfs.EditRequest) (seamfs.EditResult, error) {
	return r.inner.Edit(ctx, req)
}
func (r *redactor) Search(ctx context.Context, req seamfs.SearchRequest) (seamfs.SearchResult, error) {
	return r.inner.Search(ctx, req)
}

type config struct {
	Patterns    []string `json:"patterns,omitempty"`
	Replacement string   `json:"replacement,omitempty"`
}

// Plugin 返回 fs-redact 插件:inject fs seam,注册脱敏装饰器。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamfs.Chain](ctx, "fs")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "fs-redact: 解析配置失败: %v", err)
		}
		// 配置期先编译,避免把错误藏到第一次读。
		rd, err := redact.New(conf.Patterns, conf.Replacement)
		if err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "fs-redact: %v", err)
		}
		return chain.Decorate(func(inner seamfs.FS) seamfs.FS {
			return &redactor{inner: inner, rd: rd}
		}, Priority)
	}).WithInject("fs")
}
