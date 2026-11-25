package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"

	"ada_chat/internal/app"
	"ada_chat/internal/bot"
	"ada_chat/internal/plugins"
	"ada_chat/internal/ui"
)

func main() {
	mygo.App.SetLocale("zh-Hans")

	state := app.New()
	supervisor, err := bot.NewSupervisor(state)
	if err != nil {
		log.Printf("机器人进程管理不可用: %v", err)
	} else {
		state.SetEngine(supervisor)
	}
	view := ui.New(state)

	var win *mygo.Window
	dispatch := func(fn func()) {
		if win != nil {
			win.Update(fn)
			return
		}
		fn()
	}
	state.SetDispatch(dispatch)
	view.SetDispatch(dispatch)

	// 插件宿主(可选):没有插件时界面不变;单个插件失败只记日志。
	host, err := plugins.New(state, dispatch)
	if err != nil {
		log.Printf("插件宿主不可用: %v", err)
	} else {
		view.SetPlugins(host)
		ids, loadErrs := host.LoadRoots(pluginRoots()...)
		if len(ids) > 0 {
			log.Printf("插件: 已装载 %v", ids)
		}
		for _, e := range loadErrs {
			log.Printf("插件装载失败: %v", e)
		}
		host.Start()
	}

	mygo.App.WhenReady(func() {
		win = mygo.NewWindow(ui.WindowOptions(view))
		view.SetWindow(win)
	})
	runErr := mygo.App.Run()
	if host != nil {
		host.Stop()
	}
	if supervisor != nil {
		_ = supervisor.Close()
	}
	// 关窗时把机器人/对话状态落盘，下一轮启动从这里恢复。
	_ = state.SaveState()
	if runErr != nil {
		log.Fatal(runErr)
	}
}

// pluginRoots 返回插件搜索目录:ADA_CHAT_PLUGINS(可多个,按系统分隔符)、
// 配置目录下的 plugins,以及当前目录/可执行文件旁的 plugins。
func pluginRoots() []string {
	var roots []string
	if p := strings.TrimSpace(os.Getenv("ADA_CHAT_PLUGINS")); p != "" {
		roots = append(roots, filepath.SplitList(p)...)
	}
	if dir, err := os.UserConfigDir(); err == nil {
		roots = append(roots, filepath.Join(dir, "ada_chat", "plugins"))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, filepath.Join(wd, "plugins"))
	}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Join(filepath.Dir(exe), "plugins"))
	}
	return roots
}
