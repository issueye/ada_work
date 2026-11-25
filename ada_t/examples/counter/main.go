// 命令 adat-counter 演示:前端式开发 golang native GUI。
//
// Counter.aui 是一个 .aui 单文件组件(模板 + TS setup + scoped 样式 + i18n);
// 本 main 只负责装载与开窗——界面与交互全部在组件里,和写 Vue 一样,
// 但窗口是 mygo 的原生 UI(GPU 绘制,没有 webview)。
//
// 运行:go run ./examples/counter
package main

import (
	"errors"
	"log"
	"os"
	"time"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/mygoui"
	"github.com/issueye/ada_t/internal/compile"
)

func main() {
	pkg, err := compile.CompileDir("examples/counter", compile.Options{})
	if err != nil {
		log.Fatalf("编译组件失败: %v", err)
	}
	for _, w := range pkg.Warnings {
		log.Printf("[warn] %s", w)
	}

	app, err := adat.NewApp(pkg, adat.AppOptions{
		Backend: mygoui.New(),
		Locale:  "zh-CN",
		Logf:    func(f string, a ...any) { log.Printf("[js] "+f, a...) },
	})
	if err != nil {
		log.Fatalf("创建应用失败: %v", err)
	}

	// —— Go 服务桥:组件里 ctx.api.call('server', ...) 可调用 ——
	if err := app.Expose("server", "now", func() string {
		return time.Now().Format("15:04:05")
	}); err != nil {
		log.Fatalf("Expose now: %v", err)
	}
	if err := app.Expose("server", "greet", func(name string) (string, error) {
		if name == "" {
			return "", errors.New("名字是空的")
		}
		return "你好," + name + "(来自 Go)", nil
	}); err != nil {
		log.Fatalf("Expose greet: %v", err)
	}

	// —— Go → 组件事件:每 3 秒广播 tick,组件用 ctx.on('tick', ...) 接收 ——
	go func() {
		for n := 1; ; n++ {
			time.Sleep(3 * time.Second)
			app.Emit("tick", map[string]any{"n": n})
		}
	}()

	if err := app.Window(adat.WindowOptions{
		Title:  "Ada Counter",
		Width:  380,
		Height: 560,
	}); err != nil {
		log.Fatalf("开窗失败: %v", err)
	}

	// 演示驱动:ADAT_COUNTER_DEMO=1 时,几秒后自动切到「设置」页签
	// (组件 ctx.on('goto') 接收;截图/演示用)。
	if os.Getenv("ADAT_COUNTER_DEMO") == "1" {
		go func() {
			for i := 0; i < 3; i++ {
				app.Emit("demo-inc", nil)
				time.Sleep(400 * time.Millisecond)
			}
			time.Sleep(600 * time.Millisecond)
			app.Emit("goto", map[string]any{"tab": "设置"})
			time.Sleep(2500 * time.Millisecond)
			app.Emit("goto", map[string]any{"tab": "数据"})
		}()
	}

	if err := app.Run(); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}
