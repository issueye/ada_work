// Package redact 是 L2 基础设施里的脱敏工具:把常见密钥模式编译成可复用的替换器。
//
// 为什么集中在一处:`policy/fs-redact` 与 `policy/shell-redact` 原本各自抄了一份
// 模式表与编译逻辑,结果是新增一类密钥要改两个地方,且两份实现会悄悄漂移。
// 这里只做「正则 → 替换」,不认识任何 seam 类型,因此两个策略包都能依赖它
// (策略 → 基础设施,与 policy 依赖 caps/kernel 的方向一致)。
package redact

import (
	"fmt"
	"regexp"
)

// DefaultReplacement 是没有配置时的替换文本。
const DefaultReplacement = "[redacted]"

// DefaultPatterns 是默认的常见密钥模式。
//
// 只覆盖「一眼能认出来」的高置信度格式;宽泛匹配会误伤正常文本,
// 需要更多模式时由 presets 的 patterns 字段扩展。
var DefaultPatterns = []string{
	`gh[pousr]_[A-Za-z0-9]{20,}`, // GitHub token
	`AKIA[0-9A-Z]{16}`,           // AWS access key
	`sk-[A-Za-z0-9]{20,}`,        // OpenAI key
}

// Redactor 是一组编译好的模式;创建后不可变,可并发使用。
type Redactor struct {
	patterns    []*regexp.Regexp
	replacement string
}

// New 编译模式;patterns 为空时使用 DefaultPatterns,replacement 为空时使用 DefaultReplacement。
//
// 编译发生在构造期而不是第一次调用时:配置错误应当让插件装配失败,
// 而不是等到某次读文件/跑命令才炸,那时错误已经离配置很远。
func New(patterns []string, replacement string) (*Redactor, error) {
	list := patterns
	if len(list) == 0 {
		list = DefaultPatterns
	}
	compiled := make([]*regexp.Regexp, 0, len(list))
	for _, p := range list {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("非法正则 %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	if replacement == "" {
		replacement = DefaultReplacement
	}
	return &Redactor{patterns: compiled, replacement: replacement}, nil
}

// Apply 按顺序把每个模式替换成配置的文本。
func (r *Redactor) Apply(s string) string {
	if r == nil {
		return s
	}
	for _, re := range r.patterns {
		s = re.ReplaceAllString(s, r.replacement)
	}
	return s
}

// ApplyBytes 是 Apply 的字节版本,便于 shell 输出等 []byte 场景。
func (r *Redactor) ApplyBytes(b []byte) []byte {
	if r == nil || len(b) == 0 {
		return b
	}
	return []byte(r.Apply(string(b)))
}
