package jsrt_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

//  6. ctx 的宿主能力:log/provide/emit 都要落在调用方给的回调上,
//     且回调在 worker goroutine 上执行(这里靠 Call 返回的 happens-before 读取结果)。
func TestHostCallbacks(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { log: (m: string) => void; provide: (n: string, v: unknown) => void; emit: (e: string, p: unknown) => void }
export function run(ctx: Ctx) {
  ctx.log("hi")
  ctx.provide("svc", { n: 1 })
  ctx.emit("ev", "payload")
}`,
	})

	var (
		logs           []string
		providedName   string
		providedValue  any
		emittedEvent   string
		emittedPayload any
	)
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Logf:     func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Host: jsrt.Host{
			Provide: func(name string, svc *jsrt.Service) error {
				providedName, providedValue = name, svc
				return nil
			},
			Emit: func(event string, payload any) error {
				emittedEvent, emittedPayload = event, payload
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	if _, err := rt.CallWithHost(context.Background(), "run"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(logs) != 1 || logs[0] != "hi" {
		t.Fatalf("ctx.log 未生效: %v", logs)
	}
	if providedName != "svc" {
		t.Fatalf("ctx.provide 的服务名不正确: %q", providedName)
	}
	svc, ok := providedValue.(*jsrt.Service)
	if !ok {
		t.Fatalf("ctx.provide 应产出 *jsrt.Service,实际 %T", providedValue)
	}
	if svc.Name() != "svc" || asInt(t, svc.Data()["n"]) != 1 {
		t.Fatalf("ctx.provide 的内容不正确: name=%q data=%v", svc.Name(), svc.Data())
	}
	if emittedEvent != "ev" || emittedPayload != "payload" {
		t.Fatalf("ctx.emit 不正确: %q %v", emittedEvent, emittedPayload)
	}
}

// 7. 未启用宿主能力时,JS 调用必须抛错(而不是静默丢弃)。
func TestHostCallbackErrorsSurfaceInJS(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `export function run(ctx: { provide: (n: string, v: unknown) => void }): void {
  try {
    ctx.provide("svc", 1)
  } catch (e) {
    throw new Error("caught: " + String(e))
  }
}`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err = rt.CallWithHost(context.Background(), "run")
	if err == nil {
		t.Fatalf("未启用 provide 时应抛错")
	}
}
