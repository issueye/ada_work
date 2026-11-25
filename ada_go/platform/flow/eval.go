package flow

import (
	"github.com/issueye/ada_go_v2/platform/expr"
)

// Eval 是受限表达式求值的稳定入口(实现位于 expr 包,flow/bundle 共用)。
func Eval(s string, values map[string]any) (bool, error) {
	return expr.Eval(s, values)
}

// lookupPath 让 flow 的参数模板复用 expr 的路径语义。
func lookupPath(path string, values map[string]any) (any, error) {
	return expr.Lookup(path, values)
}
