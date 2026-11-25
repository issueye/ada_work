// Package compile 是执行层的编译步:把 TS 降级成 moejs 能执行的 JS。
//
// 它只做「逐文件 Transform」:不解析路径、不打包、不落盘、不启动进程。
// 模块解析是宿主的活(见 Artifact.Resolve),这是本包写死的责任边界:
// esbuild 补 moejs 的 TS 运行时语义,不承担打包器职责。
package compile

import (
	"fmt"
	"path"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Options 是单文件转换参数。零值即默认:按扩展名选 loader、不产 sourcemap。
type Options struct {
	SourceMap bool
	// Loader 为零值时按文件扩展名推断。
	Loader api.Loader
	// TsconfigRaw 透传部分 tsconfig 选项(如 experimentalDecorators),
	// 因为包外的插件作者常用装饰器,而 esbuild 默认不开。
	TsconfigRaw string
}

// Result 是一次转换的产物。
type Result struct {
	Code     []byte
	Map      []byte
	Warnings []string
}

// 错误类别。跨边界要能按类别处置,所以错误带 Kind 而不是只有字符串。
const (
	KindSyntax   = "syntax"
	KindInternal = "internal"
)

// Error 是编译错误:带文件名、类别与逐条诊断(含行列)。
type Error struct {
	File     string
	Kind     string
	Messages []api.Message
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "compile: %s: %s 错误", e.File, e.Kind)
	for _, m := range e.Messages {
		if m.Location != nil {
			fmt.Fprintf(&b, "\n  %s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text)
		} else {
			fmt.Fprintf(&b, "\n  %s", m.Text)
		}
	}
	return b.String()
}

// Transform 把单个文件降级为 ES2022 模块。
//
// 目标固定 ES2022:比这更新的语法交给 esbuild 降级,比这更旧的运行时(moejs)
// 也能吃;不做类型检查——类型检查是编辑器/ada check 的事,不是加载热路径的事。
func Transform(name string, src []byte, opts Options) (Result, error) {
	if opts.Loader == api.LoaderNone {
		opts.Loader = LoaderFor(name)
	}
	sourcemap := api.SourceMapNone
	if opts.SourceMap {
		sourcemap = api.SourceMapExternal
	}
	res := api.Transform(string(src), api.TransformOptions{
		Loader:        opts.Loader,
		Sourcefile:    name,
		Target:        api.ES2022,
		Format:        api.FormatESModule,
		Sourcemap:     sourcemap,
		LegalComments: api.LegalCommentsNone,
		TsconfigRaw:   opts.TsconfigRaw,
	})
	if len(res.Errors) > 0 {
		return Result{}, &Error{File: name, Kind: KindSyntax, Messages: res.Errors}
	}
	out := Result{Code: res.Code, Map: res.Map}
	for _, w := range res.Warnings {
		out.Warnings = append(out.Warnings, messageText(w))
	}
	return out, nil
}

// LoaderFor 按扩展名选 loader。未知扩展名按 TS 处理:
// Artifact 里的文件本来就该是插件源码,按 TS 解析比按 JS 解析更不容易误伤。
func LoaderFor(name string) api.Loader {
	switch strings.ToLower(path.Ext(name)) {
	case ".ts":
		return api.LoaderTS
	case ".tsx":
		return api.LoaderTSX
	case ".jsx":
		return api.LoaderJSX
	case ".js", ".mjs":
		return api.LoaderJS
	default:
		return api.LoaderTS
	}
}

func messageText(m api.Message) string {
	if m.Location != nil {
		return fmt.Sprintf("%s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text)
	}
	return m.Text
}
