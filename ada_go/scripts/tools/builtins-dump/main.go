// 命令 builtins-dump 把当前编译进宿主的 Go 插件注册表导出为 plugin.json。
//
// 两种用法:
//
//	 -out <目录>:按发布包的扁平布局导出(plugins/<id>/plugin.json);
//	 -source <plugins 根>:写回源码目录(plugins/<分类>/<目录>/plugin.json),让仓库里的
//	进程内清单与注册表同步——清单是数据,不能靠手抄。
//
// 清单的 provide/inject 来自 kernel.Plugin,生成逻辑在 platform/host/builtins/catalog.go;
// 该包的清单一致性测试会拿同一份逻辑对比仓库里的清单,防止漂移。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/issueye/ada_go_v2/platform/catalog"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

func main() {
	out := flag.String("out", "", "输出扁平 plugins 目录(发布包布局)")
	src := flag.String("source", "", "把清单写回源码目录(plugins/<分类>/<目录>)")
	flag.Parse()
	if *out == "" && *src == "" {
		fmt.Fprintln(os.Stderr, "必须给出 -out <plugins 目录> 或 -source <plugins 根>")
		os.Exit(2)
	}
	ids, err := catalog.BuiltinIDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取内置插件注册表失败:", err)
		os.Exit(1)
	}
	if *out != "" {
		if err := dump(*out, ids); err != nil {
			fmt.Fprintln(os.Stderr, "导出内置插件失败:", err)
			os.Exit(1)
		}
	}
	if *src != "" {
		if err := writeSource(*src, ids); err != nil {
			fmt.Fprintln(os.Stderr, "写回源码清单失败:", err)
			os.Exit(1)
		}
	}
}

// dump 按发布包的扁平布局导出:plugins/<id>/plugin.json。
func dump(root string, ids []string) error {
	for _, id := range ids {
		m, err := catalog.BuiltinManifest(id)
		if err != nil {
			return err
		}
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := writeManifest(filepath.Join(dir, "plugin.json"), m); err != nil {
			return err
		}
	}
	fmt.Printf("builtins-dump: %d 个内置 Go 插件 -> %s\n", len(ids), root)
	return nil
}

// writeSource 把清单写回源码目录;目录必须已存在,找不到就报错——
// 清单不能凭空造出一个没有实现的插件目录。
func writeSource(root string, ids []string) error {
	for _, id := range ids {
		dir, ok := catalog.PluginSourceDir(root, id)
		if !ok {
			return fmt.Errorf("内置插件 %s 没有对应源码目录(%s/<分类>/<目录>)", id, root)
		}
		m, err := catalog.BuiltinManifest(id)
		if err != nil {
			return err
		}
		if err := writeManifest(filepath.Join(dir, "plugin.json"), m); err != nil {
			return err
		}
	}
	fmt.Printf("builtins-dump: %d 个内置 Go 插件清单已写回 %s\n", len(ids), root)
	return nil
}

func writeManifest(path string, m plugin.Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}
