package jsrt_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

type fakeConn struct{}

func (fakeConn) Read(context.Context, []byte) (int, error)  { return 0, io.EOF }
func (fakeConn) Write(context.Context, []byte) (int, error) { return 0, io.EOF }
func (fakeConn) Close() error                               { return nil }
func (fakeConn) RemoteAddr() string                         { return "127.0.0.1:9" }
func (fakeConn) LocalAddr() string                          { return "127.0.0.1:1" }

func TestNetGuardEndToEnd(t *testing.T) {
	art, err := compile.Build("netguard", "plugin.ts", map[string]string{
		"plugin.ts": `
import net from "node:net";
import http from "node:http";
import dns from "node:dns";

export function dial(host: string, port: number): Promise<string> {
  return new Promise((resolve) => {
    const s = net.connect({ host, port });
    s.once("connect", () => { s.end(); resolve("ok") });
    s.once("error", (e: any) => resolve(String((e && e.code) || (e && e.message) || e)));
  });
}
export function get(url: string): string {
  try { http.get(url, () => {}); return "ok" } catch (e: any) { return String((e && e.message) || e) }
}
export function lookup(host: string): string {
  try { dns.lookup(host); return "ok" } catch (e: any) { return String((e && e.message) || e) }
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}

	var dials, httpCalls, dnsCalls atomic.Int64
	host := jsrt.Host{
		NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
			dials.Add(1)
			return fakeConn{}, nil
		},
		HTTPDo: func(context.Context, string) (string, error) {
			httpCalls.Add(1)
			return `{"status":200,"statusText":"OK","headers":{},"body":""}`, nil
		},
		DNSLookup: func(context.Context, string, string, string) (string, error) {
			dnsCalls.Add(1)
			return `{"address":"127.0.0.1","family":4}`, nil
		},
	}
	// 这里用 time.Duration 需要 import；见下方修正。
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host:     host,
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Outbound:        true,
			DNS:             true,
			AllowPrivateIPs: true,
			Allow:           []string{"tcp://127.0.0.1:9", "http://127.0.0.1:9"},
			Deny:            []string{"blocked.example.com"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	if got, err := rt.Call(ctx, "dial", "127.0.0.1", 9); err != nil || got != "ok" {
		t.Fatalf("允许的 dial 失败: got=%v err=%v", got, err)
	}
	if dials.Load() != 1 {
		t.Fatalf("允许的 dial 应调用 Host.NetDial 一次，实际 %d", dials.Load())
	}
	if got, err := rt.Call(ctx, "dial", "127.0.0.1", 10); err != nil {
		t.Fatalf("被拒的 dial 不应变成宿主错误: %v", err)
	} else if got == "ok" {
		t.Fatal("越权 dial 应被拒绝")
	}
	if dials.Load() != 1 {
		t.Fatal("越权 dial 不应调用 Host.NetDial")
	}

	if got, err := rt.Call(ctx, "get", "http://127.0.0.1:9/x"); err != nil || got != "ok" {
		t.Fatalf("允许的 HTTP 失败: got=%v err=%v", got, err)
	}
	if httpCalls.Load() != 1 {
		t.Fatalf("允许的 HTTP 应调用 Host.HTTPDo 一次，实际 %d", httpCalls.Load())
	}
	if got, err := rt.Call(ctx, "get", "http://127.0.0.1:10/x"); err != nil {
		t.Fatalf("被拒的 HTTP 不应变成宿主错误: %v", err)
	} else if got == "ok" {
		t.Fatal("越权 HTTP 应被拒绝")
	}
	if httpCalls.Load() != 1 {
		t.Fatal("越权 HTTP 不应调用 Host.HTTPDo")
	}

	if got, err := rt.Call(ctx, "lookup", "example.com"); err != nil || got != "ok" {
		t.Fatalf("允许的 DNS 失败: got=%v err=%v", got, err)
	}
	if dnsCalls.Load() != 1 {
		t.Fatalf("允许的 DNS 应调用 Host.DNSLookup 一次，实际 %d", dnsCalls.Load())
	}
	if got, err := rt.Call(ctx, "lookup", "blocked.example.com"); err != nil {
		t.Fatalf("被拒的 DNS 不应变成宿主错误: %v", err)
	} else if got == "ok" {
		t.Fatal("被拒的 DNS 应返回错误")
	}
	if dnsCalls.Load() != 1 {
		t.Fatal("越权 DNS 不应调用 Host.DNSLookup")
	}
}
