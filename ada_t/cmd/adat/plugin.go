package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/plugin"
)

// cmdPlugin 处理插件子命令:
//
//	adat plugin build <dir> [-write]   编译插件目录,打印摘要(可选写回 plugin.json)
//	adat plugin show  <dir>           打印清单与槽位贡献
//
// 第三方签名:用 plugin.Sign(manifest, privateKey)(见 docs/plugins.md)。
func cmdPlugin(args []string) error {
	if len(args) == 0 {
		return usageErrorf("用法: adat plugin <build|show> <dir>")
	}
	switch args[0] {
	case "build":
		return pluginBuild(args[1:])
	case "show":
		return pluginShow(args[1:])
	default:
		return usageErrorf("未知 plugin 子命令 %q(可用 build/show)", args[0])
	}
}

func pluginBuild(args []string) error {
	fs := flag.NewFlagSet("plugin build", flag.ExitOnError)
	write := fs.Bool("write", false, "把 sha256 摘要写回插件目录的 plugin.json")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat plugin build <dir> [-write]")
	}
	dir := fs.Arg(0)
	pkg, m, err := adat.CompilePlugin(dir)
	if err != nil {
		return err
	}
	for _, w := range pkg.Warnings {
		fmt.Printf("[warn] %s\n", w)
	}
	digest := adat.PluginDigest(pkg)
	fmt.Printf("plugin: %s v%s (entry %s)\n", m.ID, m.Version, m.Entry)
	fmt.Printf("components: %d\n", len(pkg.Components))
	for _, c := range m.Slots {
		fmt.Printf("  slot %-18s → %s (order %d)\n", c.Slot, c.Component, c.Order)
	}
	if len(m.Capabilities) > 0 {
		fmt.Printf("capabilities: %v\n", m.Capabilities)
	}
	fmt.Printf("digest: %s\n", digest)
	if !*write {
		fmt.Println("(未写回;加 -write 可写入 plugin.json)")
		return nil
	}
	m.SHA256 = digest
	m.Signature = "" // 摘要变了,旧签名失效
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, plugin.ManifestName)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("写回 %s 失败: %w", path, err)
	}
	fmt.Printf("已写回 %s\n", path)
	return nil
}

func pluginShow(args []string) error {
	fs := flag.NewFlagSet("plugin show", flag.ExitOnError)
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat plugin show <dir>")
	}
	m, err := plugin.Load(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", b)
	return nil
}
