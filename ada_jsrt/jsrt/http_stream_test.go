package jsrt_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/jsrt"
)

type httpStreamConn struct {
	mu       sync.Mutex
	response []byte
	read     bool
	written  []byte
}

func newHTTPStreamConn(response string) *httpStreamConn {
	return &httpStreamConn{response: []byte(response)}
}

func (c *httpStreamConn) Read(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, c.response), nil
}

func (c *httpStreamConn) Write(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *httpStreamConn) Close() error       { return nil }
func (c *httpStreamConn) RemoteAddr() string { return "127.0.0.1:9" }
func (c *httpStreamConn) LocalAddr() string  { return "127.0.0.1:12345" }
func (c *httpStreamConn) request() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func TestHTTPStreamingClientContentLength(t *testing.T) {
	conn := newHTTPStreamConn("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello")
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: "127.0.0.1", port: 9, path: "/stream", method: "POST" }, (res: any) => {
      let out = "";
      res.on("data", (chunk: any) => { out += chunk });
      res.on("end", () => resolve(out));
    });
    req.on("error", reject);
    req.write("hello");
    req.end();
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
				return conn, nil
			},
		},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("流式 HTTP 失败: %v", err)
	}
	if got != "hello" {
		t.Fatalf("响应体不符: %v", got)
	}
	req := conn.request()
	if !strings.Contains(req, "POST /stream HTTP/1.1") || !strings.Contains(req, "Content-Length: 5") || !strings.HasSuffix(req, "hello") {
		t.Fatalf("请求内容不符: %q", req)
	}
}

func TestHTTPStreamingClientChunked(t *testing.T) {
	conn := newHTTPStreamConn("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: "127.0.0.1", port: 9, path: "/chunk" }, (res: any) => {
      let out = "";
      res.on("data", (chunk: any) => { out += chunk });
      res.on("end", () => resolve(out));
    });
    req.on("error", reject);
    req.end();
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
				return conn, nil
			},
		},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("chunked 流式 HTTP 失败: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("chunked 响应体不符: %v", got)
	}
}

func TestHTTPStreamingRequestQuota(t *testing.T) {
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve) => {
    const req = http.request({ host: "127.0.0.1", port: 9, path: "/", method: "POST" });
    req.on("error", (err: any) => resolve(String((err && err.code) || err)));
    req.write("12345");
    req.end();
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
				return newHTTPStreamConn("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Outbound:        true,
			AllowPrivateIPs: true,
			Allow:           []string{"http://127.0.0.1:9"},
			MaxRequestBytes: 4,
		}},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("Call 失败: %v", err)
	}
	if got != "ERR_HTTP_REQUEST_TOO_LARGE" {
		t.Fatalf("请求配额错误码不符: %v", got)
	}
}

func TestHTTPSStreamingClient(t *testing.T) {
	conn := newHTTPStreamConn("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	rt := newResident(t, `
import https from "node:https";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const req = https.get({ host: "example.test", port: 443, path: "/" }, (res: any) => {
      let out = "";
      res.on("data", (chunk: any) => { out += chunk });
      res.on("end", () => resolve(out));
    });
    req.on("error", reject);
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			TLSDial: func(context.Context, string, string, jsrt.TLSOptions) (jsrt.NetConn, error) {
				return conn, nil
			},
		},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("HTTPS 流式客户端失败: %v", err)
	}
	if got != "ok" {
		t.Fatalf("HTTPS 响应不符: %v", got)
	}
}
