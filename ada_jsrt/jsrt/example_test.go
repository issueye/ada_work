package jsrt_test

import (
	"context"
	"fmt"
	"log"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

// Example 演示最小闭环:TS 降级 → 装配运行时 → 调用导出函数。
func Example() {
	art, err := compile.Build("example", "plugin.ts", map[string]string{
		"plugin.ts": `export function greet(name: string): string { return "hello, " + name }`,
	}, compile.Options{})
	if err != nil {
		log.Fatal(err)
	}

	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = rt.Close(context.Background()) }()

	got, err := rt.Call(context.Background(), "greet", "ada")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(got)
	// Output: hello, ada
}
