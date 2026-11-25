package compile_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
)

// 1. TS 的运行时语义(enum、构造器参数属性)必须被真正降级。
func TestTransformLowersTSRuntimeSemantics(t *testing.T) {
	src := `enum Level { High = 3 }
class Box {
  constructor(private readonly label: string) {}
  describe(): string { return this.label + ":" + Level.High }
}
export const text = new Box("x").describe()`
	res, err := compile.Transform("plugin.ts", []byte(src), compile.Options{})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	code := string(res.Code)
	if strings.Contains(code, "private readonly") {
		t.Fatalf("参数属性没有被降级:\n%s", code)
	}
	if !strings.Contains(code, "Level") {
		t.Fatalf("enum 的产物丢失:\n%s", code)
	}
	if !strings.Contains(code, "describe") {
		t.Fatalf("类方法丢失:\n%s", code)
	}
}

// 2. TSX 必须能编译(moejs 的 CompileTS 做不到)。
func TestTransformTSX(t *testing.T) {
	res, err := compile.Transform("view.tsx", []byte(`export const el = <div id="a">hi</div>`), compile.Options{})
	if err != nil {
		t.Fatalf("TSX 转换失败: %v", err)
	}
	if !strings.Contains(string(res.Code), "createElement") {
		t.Fatalf("TSX 未被转换:\n%s", string(res.Code))
	}
}

// 3. 语法错误必须带文件名与类别,而不是一句无法定位的报错。
func TestTransformSyntaxError(t *testing.T) {
	_, err := compile.Transform("bad.ts", []byte("const x: = 1"), compile.Options{})
	var ce *compile.Error
	if !errors.As(err, &ce) {
		t.Fatalf("应返回 *compile.Error,实际 %T: %v", err, err)
	}
	if ce.Kind != compile.KindSyntax {
		t.Fatalf("类别应为 syntax,实际 %q", ce.Kind)
	}
	if !strings.Contains(err.Error(), "bad.ts") {
		t.Fatalf("错误应带文件名,实际: %v", err)
	}
}

// 4. 多文件产物:解析相对导入、拒绝裸说明符、摘要稳定。
func TestBuildResolveAndDigest(t *testing.T) {
	src := map[string]string{
		"plugin.ts": `import { v } from "./util"; export const out = v`,
		"util.ts":   `export const v = 1`,
	}
	art, err := compile.Build("demo", "plugin.ts", src, compile.Options{})
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	if err := art.Verify(); err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	for _, spec := range []string{"./util", "./util.ts", "./util.js"} {
		name, ok := art.Resolve("plugin.ts", spec)
		if !ok || name != "util.ts" {
			t.Fatalf("解析 %q 应为 util.ts,实际 %q(%v)", spec, name, ok)
		}
	}
	if _, ok := art.Resolve("plugin.ts", "lodash"); ok {
		t.Fatalf("裸说明符应被拒绝(没有 node_modules)")
	}

	art.Files["util.ts"] += "\n// tampered"
	if err := art.Verify(); err == nil {
		t.Fatalf("文件被改动后摘要应不匹配")
	}
}

// 5. Artifact 的 JSON 往返与拒绝空的/缺入口的产物。
func TestArtifactJSONRoundTrip(t *testing.T) {
	art, err := compile.Build("demo", "plugin.ts", map[string]string{
		"plugin.ts": `export const x = 1`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	b, err := art.JSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := compile.ParseArtifact(b)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back.Entry != art.Entry || back.Digest != art.Digest {
		t.Fatalf("往返后不一致: %+v vs %+v", back, art)
	}

	if _, err := compile.Build("x", "nope.ts", map[string]string{"a.ts": "export {}"}, compile.Options{}); err == nil {
		t.Fatalf("入口不存在时应报错")
	}
	if _, err := compile.ParseArtifact([]byte(`{"entry":"a.ts","files":{},"digest":"x"}`)); err == nil {
		t.Fatalf("files 为空时应报错")
	}
}
