// 命令 adat:运行时编译层的 CLI。
//
//	adat lint   <dir>       只编译,输出诊断(结构/模板/表达式/样式/导入)
//	adat build  <dir> [-o dist/ui]   编译产物落盘 + ui.manifest.json(sha256)
//	adat render <dir>       headless 渲染首帧,打印 widget 树(无 GUI 验证闭环)
//	adat run    <dir>       编译并打开 mygo 原生窗口(GPU 绘制)
//	adat watch  <dir>       run 的热重载变体:文件变化即重建窗口组件(状态会重置)
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/backend/mygoui"
	"github.com/issueye/ada_t/internal/compile"
	"github.com/issueye/ada_t/widget"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "lint":
		err = cmdLint(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:], false)
	case "watch":
		err = cmdRun(os.Args[2:], true)
	case "plugin":
		err = cmdPlugin(os.Args[2:])
	case "version":
		fmt.Println("adat 0.1.0 (planVersion", widget.PlanVersion, ")")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		var ue usageErr
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, err)
			usage()
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "adat: %v\n", err)
		os.Exit(1)
	}
}

// usageErr 标记用法错误:main 以退出码 2 结束,与编译/运行错误(1)区分。
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return usageErr{fmt.Sprintf(format, args...)}
}

// reorderArgs 把「位置参数 + flag」混排重排成 flag 在前
// (Go 的 flag 包遇到首个非 flag 参数即停止解析)。布尔 flag 不吞后面的
// 位置参数——否则 `adat run -inspector examples/chat` 会把目录当成 bool 值。
func reorderArgs(args []string, fs *flag.FlagSet) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			rest = append(rest, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if f := fs.Lookup(name); f != nil && isBoolFlag(f) {
			flags = append(flags, a)
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags = append(flags, a, args[i+1])
			i++
			continue
		}
		flags = append(flags, a)
	}
	return append(flags, rest...)
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func usage() {
	fmt.Print(`adat — 前端式开发 golang native GUI 的运行时编译层

用法:
  adat lint    <dir>              编译并输出诊断
  adat build   <dir> [-o out]     产物落盘 + ui.manifest.json(sha256 摘要)
  adat render  <dir>              headless 渲染首帧并打印 widget 树
  adat inspect <dir> [-live 2s]   headless 打印 stats 与组件树(可周期刷新)
  adat run     <dir>              打开 mygo 原生窗口(GPU 绘制)
  adat watch   <dir>              run + 文件监听热重载(状态保留或重跑 setup)
  adat plugin  build <dir> [-write]  编译插件目录,打印/写回 sha256 摘要
  adat plugin  show  <dir>          打印插件清单
  adat version                    版本与 planVersion

.aui 组件 = <template>(Ada 指令) + <script lang="ts">(esbuild 降级)
         + <style scoped>(class→控件属性令牌) + <i18n>($t 查表) + <meta>
多组件目录加 -entry 指定入口组件(如 examples/chat -entry Chat)

退出码:0 成功 · 1 编译/运行错误 · 2 用法错误
`)
}

// compileFor 编译一个目录(入口 = 目录里唯一或名字匹配的 .aui)。
func compileFor(dir, entry string) (*compile.Package, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return compile.CompileDir(abs, compile.Options{Entry: entry})
}

func printWarnings(pkg *compile.Package) {
	for _, w := range pkg.Warnings {
		fmt.Printf("[warn] %s\n", w)
	}
}

func cmdLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ExitOnError)
	entry := fs.String("entry", "", "入口组件名(多组件时必填)")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat lint <dir>")
	}
	pkg, err := compileFor(fs.Arg(0), *entry)
	if err != nil {
		return err
	}
	printWarnings(pkg)
	fmt.Printf("OK: 入口组件 %s,共 %d 个组件\n", pkg.Entry, len(pkg.Components))
	return nil
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	entry := fs.String("entry", "", "入口组件名(多组件时必填)")
	out := fs.String("o", "dist/ui", "输出目录")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat build <dir> [-o out]")
	}
	pkg, err := compileFor(fs.Arg(0), *entry)
	if err != nil {
		return err
	}
	printWarnings(pkg)
	m, err := pkg.Build(*out)
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Printf("  %-32s %6d B  %s\n", f.Name, f.Bytes, f.SHA256[:12]+"…")
	}
	fmt.Printf("manifest: %s (digest %s…)\n", filepath.Join(*out, "ui.manifest.json"), m.Digest[:18])
	// 立刻 Verify 一次,证明产物与摘要一致
	if _, err := compile.Verify(*out); err != nil {
		return fmt.Errorf("产物校验失败: %w", err)
	}
	fmt.Println("verify: OK")
	return nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	entry := fs.String("entry", "", "入口组件名(多组件时必填)")
	locale := fs.String("locale", "zh-CN", "语言")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat render <dir>")
	}
	pkg, err := compileFor(fs.Arg(0), *entry)
	if err != nil {
		return err
	}
	printWarnings(pkg)
	b := headless.New()
	app, err := adat.NewApp(pkg, adat.AppOptions{
		Backend: b,
		Locale:  *locale,
		Logf:    func(f string, a ...any) { fmt.Printf("[js] "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	if err := app.Window(adat.WindowOptions{Title: "render"}); err != nil {
		return err
	}
	// 等首帧
	deadline := time.Now().Add(3 * time.Second)
	var tree *widget.Node
	for time.Now().Before(deadline) {
		if tree = b.Tree("win1"); tree != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = app.Stop()
	if tree == nil {
		return fmt.Errorf("3 秒内没有收到首帧")
	}
	fmt.Println(tree.Dump())
	return nil
}

func cmdRun(args []string, watch bool) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	entry := fs.String("entry", "", "入口组件名(多组件时必填)")
	title := fs.String("title", "", "窗口标题(默认组件名)")
	locale := fs.String("locale", "zh-CN", "语言")
	w := fs.Int("width", 360, "窗口宽(DIP)")
	h := fs.Int("height", 480, "窗口高(DIP)")
	interval := fs.Duration("interval", 600*time.Millisecond, "watch 轮询间隔")
	insp := fs.Bool("inspector", false, "开启内置调试面板窗口")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat run <dir>")
	}
	dir := fs.Arg(0)

	// 非监听模式:一次运行
	if !watch {
		app, err := buildApp(dir, *entry, *locale, *insp)
		if err != nil {
			return err
		}
		if *title == "" {
			*title = app.Pkg().Entry // 与 flag 描述一致:默认入口组件名
		}
		if err := app.Window(adat.WindowOptions{Title: *title, Width: *w, Height: *h}); err != nil {
			return err
		}
		return app.Run()
	}

	// 监听模式:组件级热重载——窗口不关、实例不重建:
	//   模板/样式/i18n 变更 → 只换数据,状态完整保留
	//   脚本变更           → 重跑受影响组件的 setup(组件内部状态重置)
	//   编译失败           → 旧世界继续服务,错误打在终端
	// 注意:app.Run 必须占用主 goroutine(mygo 约束),监听循环放后台。
	app, err := buildApp(dir, *entry, *locale, *insp)
	if err != nil {
		return err
	}
	if *title == "" {
		*title = app.Pkg().Entry
	}
	if err := app.Window(adat.WindowOptions{Title: *title, Width: *w, Height: *h}); err != nil {
		return err
	}
	stopWatch := make(chan struct{})
	go func() {
		base := compile.DirSignature(dir)
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(*interval):
			}
			if compile.DirSignature(dir) == base {
				continue
			}
			base = compile.DirSignature(dir)
			pkg, err := compileFor(dir, *entry)
			if err != nil {
				fmt.Printf("[watch] 编译失败(旧世界继续服务):\n%v\n", err)
				continue
			}
			printWarnings(pkg)
			if err := app.HotReload(pkg); err != nil {
				fmt.Printf("[watch] 热重载失败(旧世界继续服务): %v\n", err)
				continue
			}
			fmt.Println("[watch] 热重载完成")
		}
	}()
	runErr := app.Run()
	close(stopWatch)
	return runErr
}

func buildApp(dir, entry, locale string, inspector bool) (*adat.App, error) {
	pkg, err := compileFor(dir, entry)
	if err != nil {
		return nil, err
	}
	printWarnings(pkg)
	return adat.NewApp(pkg, adat.AppOptions{
		Backend:   mygoui.New(),
		Locale:    locale,
		Logf:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, "[js] "+f+"\n", a...) },
		Inspector: inspector,
	})
}
