// Package inspector 内置调试面板组件:一个原生窗口,实时展示
// 注册表统计与各窗口的组件树(数据由 App 以 __inspector 事件推送)。
package inspector

import (
	_ "embed"
	"sync"

	"github.com/issueye/ada_t/internal/compile"
)

//go:embed inspector.aui
var inspectorAUI string

var (
	once   sync.Once
	pkg    *compile.Package
	pkgErr error
)

// Package 返回 Inspector 组件的编译产物(单例;失败时返回错误)。
func Package() (*compile.Package, error) {
	once.Do(func() {
		pkg, pkgErr = compile.CompileFiles("Inspector.aui", map[string]string{
			"Inspector.aui": inspectorAUI,
		}, compile.Options{Entry: "Inspector"})
	})
	return pkg, pkgErr
}
