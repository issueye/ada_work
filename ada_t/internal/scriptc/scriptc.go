// Package scriptc 把 .aui 的 <script>(TS/JS)降级为 ESM JS。
//
// 边界与 ada_go_v2 的 platform/compile 一致:只做逐文件 Transform,
// 不解析路径、不打包、不落盘;模块解析是 compile 包与 moejs.Link 的职责。
// 目标固定 ES2022;不做类型检查(类型错误由编辑器承担,见 ui-framework.md §12)。
package scriptc

import (
	"fmt"
	"path"

	"github.com/evanw/esbuild/pkg/api"
)

// Error 是降级错误:带文件名与逐条诊断(含行列)。
type Error struct {
	File     string
	Messages []api.Message
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("scriptc: %s: TS/JS 降级错误", e.File)
	for _, m := range e.Messages {
		if m.Location != nil {
			msg += fmt.Sprintf("\n  %s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text)
		} else {
			msg += "\n  " + m.Text
		}
	}
	return msg
}

// Result 是一次降级的产物。
type Result struct {
	Code     []byte
	Warnings []string
}

// Transform 把单个文件降级为 ES2022 ESM 模块(loader 按扩展名推断)。
func Transform(name string, src []byte) (Result, error) {
	return TransformWith(name, src, LoaderFor(name))
}

// TransformWith 用显式 loader 降级;组件的 <script lang="ts"> 虚拟模块
// 名是 "<File>.aui.js"(按 .js 解析导入),但源码是 TS,必须传 LoaderTS。
func TransformWith(name string, src []byte, loader api.Loader) (Result, error) {
	res := api.Transform(string(src), api.TransformOptions{
		Loader:        loader,
		Sourcefile:    name,
		Target:        api.ES2022,
		Format:        api.FormatESModule,
		LegalComments: api.LegalCommentsNone,
	})
	return resultOf(name, res)
}

// TransformCJS 降级为 CommonJS(HMR 用:imports 变 require,配 RunScript
// 进入已加载的运行时,surface 的 require 垫片提供 'ada:surface' 与相对模块)。
func TransformCJS(name string, src []byte) (Result, error) {
	res := api.Transform(string(src), api.TransformOptions{
		Loader:        api.LoaderJS,
		Sourcefile:    name,
		Target:        api.ES2022,
		Format:        api.FormatCommonJS,
		LegalComments: api.LegalCommentsNone,
	})
	return resultOf(name, res)
}

func resultOf(name string, res api.TransformResult) (Result, error) {
	if len(res.Errors) > 0 {
		return Result{}, &Error{File: name, Messages: res.Errors}
	}
	out := Result{Code: res.Code}
	for _, w := range res.Warnings {
		out.Warnings = append(out.Warnings, w.Text)
	}
	return out, nil
}

// LoaderFor 按扩展名选 loader;.aui 里的 script 块默认按 TS 处理。
func LoaderFor(name string) api.Loader {
	switch path.Ext(name) {
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
