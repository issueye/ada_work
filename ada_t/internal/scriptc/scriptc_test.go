package scriptc

import (
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

func TestTransformTS(t *testing.T) {
	src := `import { reactive } from 'ada:surface'
export enum Level { High = 3 }
type P = { name: string }
export default {
  props: {} as P,
  setup(props: P) {
    const s = reactive({ n: Level.High })
    return { s, label: props.name }
  },
}`
	res, err := TransformWith("C.aui.js", []byte(src), api.LoaderTS)
	if err != nil {
		t.Fatalf("Transform 失败: %v", err)
	}
	code := string(res.Code)
	if !strings.Contains(code, "reactive") {
		t.Fatal("import 应保留(未打包)")
	}
	if strings.Contains(code, ": P") || strings.Contains(code, "as P") {
		t.Fatalf("类型语法应被擦除:\n%s", code)
	}
	if !strings.Contains(code, "export") {
		t.Fatalf("应保持 ESM:\n%s", code)
	}
	if !strings.Contains(code, "3") {
		t.Fatalf("enum 值应内联:\n%s", code)
	}
}

func TestTransformSyntaxError(t *testing.T) {
	_, err := Transform("bad.ts", []byte("export const a = ;"))
	if err == nil {
		t.Fatal("语法错误应失败")
	}
	if !strings.Contains(err.Error(), "bad.ts") {
		t.Fatalf("错误应带文件名: %v", err)
	}
}

func TestLoaderFor(t *testing.T) {
	if LoaderFor("x.ts") != api.LoaderTS {
		t.Fatal(".ts 应选 TS loader")
	}
	if LoaderFor("x.js") != api.LoaderJS {
		t.Fatal(".js 应选 JS loader")
	}
	if LoaderFor("x.mjs") != api.LoaderJS {
		t.Fatal(".mjs 应选 JS loader")
	}
	if LoaderFor("C.aui.js") != api.LoaderJS {
		t.Fatal("组件脚本(.aui.js)应选 JS loader")
	}
}
