package jsrt_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

// 11. ctx.effect:清理函数登记到宿主,由宿主决定何时(逆序)调用。
func TestHostEffect(t *testing.T) {
	art := newArtifact(t, map[string]string{
		"plugin.ts": `type Ctx = { log: (m: string) => void; effect: (l: string, f: () => void) => void }
export function run(ctx: Ctx) {
  ctx.effect("first", () => ctx.log("cleanup:first"))
  ctx.effect("second", () => ctx.log("cleanup:second"))
}`,
	})

	var (
		logs    []string
		effects []struct {
			label   string
			dispose func(context.Context) error
		}
	)
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Logf:     func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Host: jsrt.Host{
			Effect: func(label string, dispose func(context.Context) error) error {
				effects = append(effects, struct {
					label   string
					dispose func(context.Context) error
				}{label, dispose})
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
	if len(effects) != 2 || effects[0].label != "first" || effects[1].label != "second" {
		t.Fatalf("effect 登记顺序不正确: %+v", effects)
	}
	// 逆序执行由桥接层负责,这里只验证入口本身可用。
	if err := effects[1].dispose(context.Background()); err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if err := effects[0].dispose(context.Background()); err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if got := strings.Join(logs, ","); got != "cleanup:second,cleanup:first" {
		t.Fatalf("清理顺序不正确: %q", got)
	}
}
