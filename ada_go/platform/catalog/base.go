// 本文件解析「装配基目录」:preset 里的资源路径(replay 脚本、workspace 根、
// 临时目录)按包根书写,而宿主常在用户工作区目录里启动 agent;不把进程切到包根,
// 这些相对路径就会解析到用户的目录里,装配直接失败(ACP 表现为「连接已关闭」)。
package catalog

import (
	"os"
	"path/filepath"
	"strings"
)

// BaseMarker 是发布包的装配基目录锚点:发布包没有 go.mod,
// 靠这个文件让 preset 里 `data/`、`workspace/` 这类相对路径落在包根。
const BaseMarker = ".ada-base"

// FindBase 从 start 向上找到装配基目录(含 go.mod 的源码根,或含 .ada-base 的发布包根)。
//
// 找不到时返回空串——调用方据此决定用 preset 所在目录兜底,而不是猜一个路径。
func FindBase(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		for _, anchor := range []string{"go.mod", BaseMarker} {
			if info, err := os.Stat(filepath.Join(dir, anchor)); err == nil && !info.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// SameIndir 判断两个路径是否指向同一个目录(Windows 下大小写不敏感)。
func SameIndir(a, b string) bool {
	ca, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	cb, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb)) ||
		filepath.Clean(ca) == filepath.Clean(cb)
}
