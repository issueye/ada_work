package jsrt_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

//  8. ctx.on:宿主 → JS 的反向调用。注册后,宿主可以在任意 goroutine 上触发;
//     调用会被投递到 worker,并复用同一套中断/超时语义。
func TestHostOnInvokesJSHandler(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { log: (m: string) => void; on: (e: string, h: (p: unknown) => void) => void }
export function run(ctx: Ctx) {
  ctx.on("ev", (p) => ctx.log("handler:" + String(p)))
}`,
	})

	var (
		logs         []string
		invokedEvent string
		invoke       func(ctx context.Context, payload any) (any, error)
	)
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Logf:     func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Host: jsrt.Host{
			On: func(event string, inv func(context.Context, any) (any, error)) error {
				invokedEvent, invoke = event, inv
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
	if invokedEvent != "ev" {
		t.Fatalf("ctx.on 未注册: event=%q", invokedEvent)
	}
	if invoke == nil {
		t.Fatalf("ctx.on 没有提供可调用的回调")
	}
	if _, err := invoke(context.Background(), "x"); err != nil {
		t.Fatalf("触发处理器失败: %v", err)
	}
	if len(logs) != 1 || logs[0] != "handler:x" {
		t.Fatalf("处理器日志不正确: %v", logs)
	}
}

// 9. ctx.on 的第二个参数不是函数时必须报错(否则会埋一个永远不响的监听器)。
func TestHostOnRejectsNonFunction(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { on: (e: string, h: unknown) => void }
export function run(ctx: Ctx): void { ctx.on("ev", 42) }`,
	})
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			On: func(string, func(context.Context, any) (any, error)) error { return nil },
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	if _, err := rt.CallWithHost(context.Background(), "run"); err == nil {
		t.Fatalf("非函数处理器应报错")
	}
}
