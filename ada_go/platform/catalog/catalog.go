// Package catalog 把编译进宿主的 Go 插件注册表导出为 plugin.json。
//
// 它是打包机制,不是宿主适配:生成逻辑只有这一份——发布打包
// (scripts/tools/builtins-dump)与仓库里的源码清单(一致性测试)都调这里,
// 清单因此不会因为手抄而过期。放在 platform/ 而不是 host/builtins,是因为
// 它服务于「插件目录的打包产物」,与宿主窗口/协议映射无关。
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

// CatalogCategories 是插件源码目录的分类,顺序即查找顺序(与 AGENTS §5.4 的约定一致)。
var CatalogCategories = []string{"seam", "provider", "policy", "core", "tool"}

// BuiltinIDs 返回内置 Go 插件 id,已排序(保证打包输出可重复)。
func BuiltinIDs() ([]string, error) {
	reg := builtins.Plugins()
	ids := make([]string, 0, len(reg))
	for id, mk := range reg {
		if mk == nil {
			return nil, fmt.Errorf("内置插件 %s 的构造函数为 nil", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// BuiltinManifest 从编译进宿主的插件定义生成清单。
//
// config 不写进清单:参数是 preset 的数据,清单只陈述「这个插件是谁、需要什么、提供什么」。
func BuiltinManifest(id string) (plugin.Manifest, error) {
	mk, ok := builtins.Plugins()[id]
	if !ok {
		return plugin.Manifest{}, fmt.Errorf("内置插件 %s 没有注册实现", id)
	}
	if mk == nil {
		return plugin.Manifest{}, fmt.Errorf("内置插件 %s 的构造函数为 nil", id)
	}
	p := mk()
	if p == nil {
		return plugin.Manifest{}, fmt.Errorf("内置插件 %s 返回 nil", id)
	}
	inject := p.Inject()
	return plugin.Manifest{
		ID: id, Name: p.Name(), Version: "0.1.0", Kind: "go",
		Provide: p.Provide(),
		Inject:  plugin.Inject{Required: inject.Names, Optional: inject.Optional},
	}, nil
}

// PluginSourceDir 在插件源码根(root)下定位 id 对应的目录。
//
// 目录名就是 id,唯一例外是 seam:seam 插件 id 带 `seam-` 前缀、目录去掉前缀
// (plugins/seam/acp → seam-acp)。seam 目录下只放契约,不参与无前缀 id 的查找——
// plugins/seam/plan 是 plan 的契约包,plugins/core/plan 才是插件实现。
func PluginSourceDir(root, id string) (string, bool) {
	if name, ok := strings.CutPrefix(id, "seam-"); ok {
		return existingDir(filepath.Join(root, "seam", name))
	}
	for _, cat := range CatalogCategories {
		if cat == "seam" {
			continue // seam 只承载契约,插件实现不在专门分类里按裸 id 找
		}
		if dir, ok := existingDir(filepath.Join(root, cat, id)); ok {
			return dir, true
		}
	}
	return "", false
}

// existingDir 判断 dir 是否存在且是目录。
func existingDir(dir string) (string, bool) {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, true
	}
	return "", false
}
