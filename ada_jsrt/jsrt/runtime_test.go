package jsrt_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func newArtifact(t *testing.T, files map[string]string) *compile.Artifact {
	t.Helper()
	art, err := compile.Build("test", "plugin.ts", files, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	return art
}

// 1. 多模块加载 + JSON 参数与返回值。
func TestCallMultiModule(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `import { add } from "./util"
export function sum(a: number, b: number): number { return add(a, b) }`,
		"util.ts": `export function add(a: number, b: number): number { return a + b }`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	got, err := rt.Call(context.Background(), "sum", 2, 3)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if n := asInt(t, got); n != 5 {
		t.Fatalf("返回值应为 5,实际 %v", got)
	}
}

// 2. JS 抛错要带上堆栈信息,不能只是一句 "error"。
func TestCallReportsJSException(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `export function boom(): void { throw new Error("kaboom") }`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err = rt.Call(context.Background(), "boom")
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("错误应包含 JS 异常信息,实际 %v", err)
	}
}

// 3. 超时必须能中断死循环,且中断后运行时还能继续用(清掉中断标志)。
func TestTimeoutInterruptsAndRuntimeRecovers(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `export function spin(): void { for (;;) {} }
export function echo(v: number): number { return v }`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := rt.Call(ctx, "spin"); err == nil {
		t.Fatalf("死循环应在超时后失败")
	}

	got, err := rt.Call(context.Background(), "echo", 7)
	if err != nil {
		t.Fatalf("中断后运行时应可继续使用: %v", err)
	}
	if n := asInt(t, got); n != 7 {
		t.Fatalf("恢复后返回值应为 7,实际 %v", got)
	}
}

// 4. 缺导出、坏导入、关闭后调用都要报明确的错误。
func TestErrorsAreExplicit(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `export function ok(): number { return 1 }`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	if _, err := rt.Call(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "未导出") {
		t.Fatalf("缺导出应报错,实际 %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, err := rt.Call(context.Background(), "ok"); !errors.Is(err, jsrt.ErrClosed) {
		t.Fatalf("关闭后调用应返回 ErrClosed,实际 %v", err)
	}

	bad := newArtifact(t, map[string]string{
		"plugin.ts": `import { x } from "./nope"; export const y = x`,
	})
	if _, err := jsrt.New(context.Background(), jsrt.Options{Artifact: bad}); err == nil || !strings.Contains(err.Error(), "无法解析") {
		t.Fatalf("无法解析的导入应在装配期报错,实际 %v", err)
	}
}
