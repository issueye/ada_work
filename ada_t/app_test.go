package adat

// 公共入口回归:窗口 id、WindowIDs、Run/Stop 幂等与 Inspector 附加窗口。

import (
	"testing"
	"time"

	"github.com/issueye/ada_t/backend/headless"
	"github.com/issueye/ada_t/internal/compile"
)

func testPackage(t *testing.T) *compile.Package {
	t.Helper()
	pkg, err := compile.CompileFiles("A.aui", map[string]string{
		"A.aui": `<template><Column><Text>ok</Text></Column></template>`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("编译: %v", err)
	}
	return pkg
}

func TestAppWindowsAndStop(t *testing.T) {
	app, err := NewApp(testPackage(t), AppOptions{Backend: headless.New()})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Window(WindowOptions{Title: "a", Width: 200, Height: 200}); err != nil {
		t.Fatal(err)
	}
	if err := app.Window(WindowOptions{Title: "b", Width: 200, Height: 200}); err != nil {
		t.Fatal(err)
	}
	ids := app.WindowIDs()
	if len(ids) != 2 || ids[0] != "win1" || ids[1] != "win2" {
		t.Fatalf("窗口 id = %v, want [win1 win2]", ids)
	}

	done := make(chan error, 1)
	go func() { done <- app.Run() }()
	time.Sleep(50 * time.Millisecond) // 让 headless Run 进入 running 状态
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && app.DumpWindow("win1") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if app.DumpWindow("win1") == "" {
		t.Fatal("首帧未渲染")
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run 返回错误: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 之后 Run 未返回")
	}
	if err := app.Stop(); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
}

func TestAppInspectorWindow(t *testing.T) {
	app, err := NewApp(testPackage(t), AppOptions{Backend: headless.New(), Inspector: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Window(WindowOptions{Title: "main"}); err != nil {
		t.Fatal(err)
	}
	ids := app.WindowIDs()
	if len(ids) != 2 || ids[0] != "inspector" || ids[1] != "win1" {
		t.Fatalf("Inspector 下的窗口 id = %v", ids)
	}
	if err := app.Stop(); err != nil {
		t.Fatal(err)
	}
}
