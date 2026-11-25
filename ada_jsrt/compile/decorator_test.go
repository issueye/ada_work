package compile_test

import (
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
)

const decoratorSrc = `function sealed(target: any) {}
@sealed
export class A { constructor(public readonly n: number) {} }`

//  6. 装饰器:esbuild 默认支持现代(TC39)装饰器;旧式实验装饰器需要 tsconfig 开关。
//     两者都必须被降级成普通 JS——moejs 不认识装饰器语法。
func TestTransformDecorator(t *testing.T) {
	modern, err := compile.Transform("dec.ts", []byte(decoratorSrc), compile.Options{})
	if err != nil {
		t.Fatalf("现代装饰器应能转换: %v", err)
	}
	if !strings.Contains(string(modern.Code), "__decorator") {
		t.Fatalf("现代装饰器未被降级:\n%s", string(modern.Code))
	}

	legacy, err := compile.Transform("dec.ts", []byte(decoratorSrc), compile.Options{
		TsconfigRaw: `{"compilerOptions":{"experimentalDecorators":true}}`,
	})
	if err != nil {
		t.Fatalf("实验装饰器应能转换: %v", err)
	}
	if !strings.Contains(string(legacy.Code), "__decorate") {
		t.Fatalf("实验装饰器未被降级:\n%s", string(legacy.Code))
	}
}
