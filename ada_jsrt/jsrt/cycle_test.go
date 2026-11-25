package jsrt_test

import (
	"context"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

// 5. ESM 循环依赖:两个模块互相 import,Link 与执行都要正常(S9 回归)。
func TestCircularImports(t *testing.T) {
	art, err := compile.Build("cycle", "a.ts", map[string]string{
		"a.ts": `import { b } from "./b"
export function a(): string { return "a" + b() }`,
		"b.ts": `import { a } from "./a"
export function b(): string { return "b" }`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("循环依赖应能装配: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	got, err := rt.Call(context.Background(), "a")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got != "ab" {
		t.Fatalf("循环依赖下的调用结果应为 ab,实际 %v", got)
	}
}
