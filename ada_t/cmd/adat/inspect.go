package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/issueye/ada_t"
	"github.com/issueye/ada_t/backend/headless"
)

// cmdInspect 以 headless 后端装载组件,打印注册表统计与每窗口 widget 树:
//
//	一次性:adat inspect <dir> -entry X
//	持续刷新:adat inspect <dir> -entry X -live 2s(Ctrl+C 退出)
func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	entry := fs.String("entry", "", "入口组件名(多组件时必填)")
	locale := fs.String("locale", "zh-CN", "语言")
	wait := fs.Duration("wait", 1200*time.Millisecond, "首帧等待时长")
	live := fs.Duration("live", 0, "持续刷新间隔(0 = 一次性)")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageErrorf("用法: adat inspect <dir> [-entry X] [-live 2s]")
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
	})
	if err != nil {
		return err
	}
	if err := app.Window(adat.WindowOptions{Title: "inspect"}); err != nil {
		return err
	}
	// headless 无事件循环:Run 需要后台起,Stop 收尾
	go func() { _ = app.Run() }()

	snapshot := func() {
		fmt.Println("==================================================")
		if m, err := app.Stats(); err == nil {
			fmt.Printf("stats: 实例=%v handler=%v 事件订阅=%v 窗口=%v\n",
				m["instances"], m["handlers"], m["eventSubs"], m["windows"])
		}
		for _, id := range app.WindowIDs() {
			fmt.Printf("-- 窗口 %s --\n%s", id, app.DumpWindow(id))
		}
	}
	time.Sleep(*wait)
	snapshot()

	if *live <= 0 {
		_ = app.Stop()
		return nil
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(*live)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			_ = app.Stop()
			return nil
		case <-tick.C:
			snapshot()
		}
	}
}
