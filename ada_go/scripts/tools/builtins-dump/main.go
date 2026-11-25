// 命令 builtins-dump 把当前编译进宿主的 Go 插件注册表导出为 plugin.json。
//
// 它是发布打包工具,不是产品插件:清单的 provide/inject 直接来自 kernel.Plugin,
// 因此打包后的 plugins/ 与二进制内置插件不会因手写清单而过期。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/plugin"
)

func main() {
	out := flag.String("out", "", "输出 plugins 目录")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "必须给出 -out <plugins 目录>")
		os.Exit(2)
	}
	if err := dump(*out); err != nil {
		fmt.Fprintln(os.Stderr, "导出内置插件失败:", err)
		os.Exit(1)
	}
}

func dump(root string) error {
	reg := builtins.Plugins()
	ids := make([]string, 0, len(reg))
	for id := range reg {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		mk := reg[id]
		if mk == nil {
			return fmt.Errorf("内置插件 %s 的构造函数为 nil", id)
		}
		p := mk()
		if p == nil {
			return fmt.Errorf("内置插件 %s 返回 nil", id)
		}
		inject := p.Inject()
		m := plugin.Manifest{
			ID: id, Name: p.Name(), Version: "0.1.0", Kind: "go",
			Provide: p.Provide(),
			Inject: plugin.Inject{
				Required: inject.Names,
				Optional: inject.Optional,
			},
		}
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("builtins-dump: %d 个内置 Go 插件 -> %s\n", len(ids), root)
	return nil
}
