package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestTLSConnectAsync(t *testing.T) {
	art, err := compile.Build("tls-async", "plugin.ts", map[string]string{
		"plugin.ts": `
import tls from "node:tls";

export function echo(host: string, port: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const s = tls.connect({ host, port, rejectUnauthorized: false, servername: "example.test" });
    s.on("secureConnect", () => s.write("PING"));
    s.on("data", (chunk: any) => { s.end(); resolve(chunk.toString()) });
    s.on("error", reject);
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}

	var gotOpts jsrt.TLSOptions
	var dials int
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			TLSDial: func(_ context.Context, _ string, _ string, opts jsrt.TLSOptions) (jsrt.NetConn, error) {
				dials++
				gotOpts = opts
				return &scriptedConn{}, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Outbound:        true,
			AllowPrivateIPs: true,
			Protocols:       []string{"tls"},
			Allow:           []string{"tls://127.0.0.1:9"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	got, err := rt.Call(context.Background(), "echo", "127.0.0.1", 9)
	if err != nil || got != "PONG" {
		t.Fatalf("TLS 异步结果不符: got=%v err=%v", got, err)
	}
	if dials != 1 {
		t.Fatalf("期望一次 TLS dial，实际 %d", dials)
	}
	if !gotOpts.Insecure || gotOpts.ServerName != "example.test" {
		t.Fatalf("TLSOptions 传递不符: %+v", gotOpts)
	}
}

func TestTLSProtocolPermission(t *testing.T) {
	art, err := compile.Build("tls-denied", "plugin.ts", map[string]string{
		"plugin.ts": `
import tls from "node:tls";
export function echo(host: string, port: number): Promise<string> {
  return new Promise((resolve) => {
    const s = tls.connect({ host, port });
    s.on("secureConnect", () => resolve("ok"));
    s.on("error", (e: any) => resolve(String((e && e.code) || e)));
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}
	var dials int
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			TLSDial: func(context.Context, string, string, jsrt.TLSOptions) (jsrt.NetConn, error) {
				dials++
				return &scriptedConn{}, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Outbound:        true,
			AllowPrivateIPs: true,
			Protocols:       []string{"tcp"},
			Allow:           []string{"tls://127.0.0.1:9"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	got, err := rt.Call(context.Background(), "echo", "127.0.0.1", 9)
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if dials != 0 {
		t.Fatal("协议未授权的 TLS 不应调用 Host.TLSDial")
	}
	if s, _ := got.(string); s == "ok" || !strings.Contains(s, "ERR_ACCESS_DENIED") {
		t.Fatalf("期望 ERR_ACCESS_DENIED，实际 %v", got)
	}
}
