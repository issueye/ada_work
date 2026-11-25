// 插件宿主示例:宿主自己的原生窗口 + 插件槽位内容。
//
// 运行:go run ./examples/plugin-host
//
// 演示的最小闭环:
//   - 宿主用 mygo 画自己的界面(这里是一个极简的侧栏 + 动作区);
//   - 插件(examples/plugins/hello)只声明 slot + component,不决定位置与主题;
//   - 宿主把槽位交给 backend/embed 的 Draw 画进自己的 ui.Context;
//   - 插件能力白名单(host.greet)、事件前缀(plugin.hello.command)与主题令牌。
//
// 真实宿主(ada_chat)的接入方式见 docs/plugins.md。
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/embed"
	"github.com/issueye/ada_t/plugin"
)

// themeTokens 模拟宿主的 Lobe MyGo / 设计令牌;插件用 ctx.theme() 读取。
var themeTokens = map[string]any{
	"bg":     "#10151d",
	"panel":  "#1d2430",
	"fg":     "#e8eef7",
	"muted":  "#9fb0c3",
	"accent": "#2f6fed",
	"radius": 8,
}

func main() {
	root := repoRoot()
	app, b, err := newHost(root)
	if err != nil {
		log.Fatalf("装配宿主失败: %v", err)
	}
	defer func() { _ = app.Stop() }()

	// App.Run 占后台(embed.Run 阻塞到 Stop);原生窗口占主线程
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run() }()

	mygo.App.WhenReady(func() {
		_ = mygo.NewWindow(mygo.WindowOptions{
			Title:           "Ada 插件宿主",
			Width:           860,
			Height:          560,
			BackgroundColor: "#10151d",
			Content:         ui.View(hostView(app, b)),
		})
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatalf("窗口失败: %v", err)
	}
	// 窗口关闭 → 停止 JS 世界
	_ = app.Stop()
	if err := <-runErr; err != nil {
		log.Printf("运行时退出: %v", err)
	}
}

// newHost 装配宿主:编译宿主包、创建 embed 后端、注册插槽、装载示例插件。
// 单独抽出来便于无显示器测试(main_test.go)。
func newHost(root string) (*adat.App, *embed.Backend, error) {
	hostPkg, err := adat.CompileDir(filepath.Join(root, "examples/plugin-host/host"), "")
	if err != nil {
		return nil, nil, fmt.Errorf("编译宿主组件: %w", err)
	}
	b := embed.New(embed.Options{Theme: themeTokens})
	app, err := adat.NewApp(hostPkg, adat.AppOptions{
		Backend:  b,
		SlotMode: true, // 装载插件时按贡献自动挂载槽位视图
		Theme:    themeTokens,
		Logf:     func(f string, a ...any) { log.Printf("[js] "+f, a...) },
	})
	if err != nil {
		return nil, nil, fmt.Errorf("创建应用: %w", err)
	}
	if err := app.ExposeHost("host", "greet", func() string { return "你好,我是宿主" }); err != nil {
		return nil, nil, fmt.Errorf("ExposeHost: %w", err)
	}
	app.OnPluginEvent(func(pluginID, event string, payload any) {
		fmt.Printf("[plugin:%s] %s %v\n", pluginID, event, payload)
	})
	for _, s := range []string{
		plugin.SlotSidebarItem,
		plugin.SlotComposerAction,
		plugin.SlotChatHeader,
		plugin.SlotCommand,
	} {
		if err := app.RegisterSlot(s); err != nil {
			return nil, nil, fmt.Errorf("RegisterSlot(%s): %w", s, err)
		}
	}
	info, err := app.LoadPluginDir(filepath.Join(root, "examples/plugins/hello"))
	if err != nil {
		return nil, nil, fmt.Errorf("装载插件: %w", err)
	}
	log.Printf("已装载插件 %s v%s,槽位贡献 %d 个", info.ID, info.Version, len(info.Slots))
	return app, b, nil
}

// hostView 是宿主的原生界面:左边侧栏、右边动作区,插件内容由 embed Draw 画进去。
func hostView(app *adat.App, b *embed.Backend) func(c *ui.Context) {
	return func(c *ui.Context) {
		ui.Text(c, "Ada 插件宿主").Bold().FontSize(18)
		ui.Row(c).Gap(10).Fill().Children(func() {
			ui.Column(c).Width(240).Gap(8).Padding(8).
				Background(ui.RGB(29, 36, 48)).Children(func() {
				ui.Text(c, "会话").Bold().TextColor(ui.RGB(159, 176, 195))
				drawSlot(c, app, b, plugin.SlotSidebarItem)
			})
			ui.Column(c).Grow(1).Gap(10).Padding(8).Children(func() {
				ui.Text(c, "插件动作 · composer.action").TextColor(ui.RGB(159, 176, 195))
				drawSlot(c, app, b, plugin.SlotComposerAction)
				ui.Text(c, "事件与能力:插件只能调 plugin.json 里声明的 host.*").TextColor(ui.RGB(130, 140, 155))
			})
		})
	}
}

// drawSlot 按宿主决定的位置/顺序画一个槽位里的全部插件视图。
func drawSlot(c *ui.Context, app *adat.App, b *embed.Backend, slot string) {
	views := app.SlotViews(slot)
	if len(views) == 0 {
		ui.Text(c, "(空槽位 "+slot+")").TextColor(ui.RGB(120, 130, 145))
		return
	}
	for _, v := range views {
		b.Draw(c, v.ID)
	}
}

// repoRoot 向上查找 go.mod,让示例与测试都能从任意工作目录定位仓库根。
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}
