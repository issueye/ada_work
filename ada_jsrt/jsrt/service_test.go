package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// 10. JS 服务对象上的函数成员变成 Go 可调用的方法,且 this 绑定到服务对象。
func TestServiceMethods(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { provide: (n: string, v: unknown) => void }
export function run(ctx: Ctx) {
  ctx.provide("calc", {
    base: 7,
    add(n: number): number { return this.base + n },
    label(): string { return "base=" + this.base },
  })
}`,
	})

	var svc *jsrt.Service
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			Provide: func(name string, s *jsrt.Service) error {
				svc = s
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	if _, err := rt.CallWithHost(context.Background(), "run"); err != nil {
		t.Fatalf("调用 run 失败: %v", err)
	}
	if svc == nil {
		t.Fatalf("ctx.provide 未产出服务")
	}
	if got := strings.Join(svc.Methods(), ","); got != "add,label" {
		t.Fatalf("方法列表不正确: %q", got)
	}
	if asInt(t, svc.Data()["base"]) != 7 {
		t.Fatalf("数据成员不正确: %v", svc.Data())
	}

	got, err := svc.Call(context.Background(), "add", 5)
	if err != nil {
		t.Fatalf("调用 add 失败: %v", err)
	}
	if asInt(t, got) != 12 {
		t.Fatalf("add(5) 应为 12(验证 this 绑定),实际 %v", got)
	}
	label, err := svc.Call(context.Background(), "label")
	if err != nil || label != "base=7" {
		t.Fatalf("label() 应为 base=7,实际 %v(%v)", label, err)
	}
	if _, err := svc.Call(context.Background(), "missing"); err == nil {
		t.Fatalf("不存在的方法应报错")
	}
}
