package jsrt_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestNetAbortSignal(t *testing.T) {
	art, err := compile.Build("net-abort", "plugin.ts", map[string]string{
		"plugin.ts": `
import net from "node:net";
export function run(host: string, port: number): Promise<string> {
  return new Promise((resolve) => {
    const controller = new AbortController();
    const sock = net.connect({ host, port, signal: controller.signal });
    sock.on("error", (err: any) => resolve(String((err && err.code) || err)));
    setTimeout(() => controller.abort(), 10);
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
				time.Sleep(50 * time.Millisecond)
				return &scriptedConn{}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	got, err := rt.Call(context.Background(), "run", "127.0.0.1", 9)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if got != "ABORT_ERR" {
		t.Fatalf("net abort 错误码不符: %v", got)
	}
}

func TestHTTPAbortBeforeDispatch(t *testing.T) {
	art, err := compile.Build("http-abort", "plugin.ts", map[string]string{
		"plugin.ts": `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve) => {
    const controller = new AbortController();
    controller.abort();
    const req = http.get({ host: "127.0.0.1", port: 9, path: "/", signal: controller.signal });
    req.on("error", (err: any) => resolve(String((err && err.code) || err)));
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	rt, err := jsrt.New(context.Background(), jsrt.Options{Artifact: art})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if got != "ABORT_ERR" {
		t.Fatalf("HTTP abort 错误码不符: %v", got)
	}
}
