package jsrt_test

import (
	"context"
	"errors"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// 15. 运行期 ctx.emit:宿主同步派发时若绕回本运行时的处理器,必须快速失败而不是死锁。
func TestRuntimeEmitReentrantGuard(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { on: (e: string, h: (p: any) => void) => void; emit: (e: string, p: any) => void }
export function run(ctx: Ctx) {
  ctx.on("e", (p) => ctx.emit("x", p))
}`,
	})

	var invoke func(ctx context.Context, payload any) (any, error)
	var emitErr error
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			On: func(event string, inv func(context.Context, any) (any, error)) error {
				invoke = inv
				return nil
			},
			Emit: func(event string, payload any) error {
				// 模拟宿主「同步派发」:直接回调本运行时的处理器。
				_, emitErr = invoke(context.Background(), payload)
				return emitErr
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
	if _, err := invoke(context.Background(), 1); err == nil {
		t.Fatalf("自回调应失败,实际成功")
	}
	if !errors.Is(emitErr, jsrt.ErrReentrant) {
		t.Fatalf("宿主派发应收到 ErrReentrant,实际 %v", emitErr)
	}
}
