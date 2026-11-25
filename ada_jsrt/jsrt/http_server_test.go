package jsrt_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

type httpConn struct {
	mu      sync.Mutex
	read    bool
	written []byte
}

func (c *httpConn) Read(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, "GET /x HTTP/1.1\r\nHost: example.test\r\n\r\n"), nil
}

func (c *httpConn) Write(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *httpConn) Close() error       { return nil }
func (c *httpConn) RemoteAddr() string { return "127.0.0.1:5555" }
func (c *httpConn) LocalAddr() string  { return "127.0.0.1:12345" }
func (c *httpConn) response() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func TestHTTPServerRequestResponse(t *testing.T) {
	conn := &httpConn{}
	ln := newFakeListener(conn)
	rt := newResident(t, `
import http from "node:http";

export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = http.createServer((req: any, res: any) => {
      let body = "";
      req.on("data", (chunk: any) => { body += chunk });
      req.on("end", () => {
        res.writeHead(200, "OK", { "content-type": "text/plain" });
        res.end("hello " + req.method + " " + req.url);
        resolve(req.url);
        srv.close();
      });
    });
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1");
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			NetListen: func(context.Context, string, string) (jsrt.NetListener, error) {
				return ln, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Inbound:         true,
			AllowPrivateIPs: true,
			Protocols:       []string{"tcp"},
			Listen:          []string{"127.0.0.1:0"},
		}},
	})

	got, err := rt.Call(context.Background(), "run")
	if err != nil || got != "/x" {
		t.Fatalf("HTTP server 请求结果不符: got=%v err=%v", got, err)
	}
	resp := conn.response()
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") {
		t.Fatalf("响应头不符: %q", resp)
	}
	if !strings.Contains(resp, "content-type: text/plain") {
		t.Fatalf("响应头缺少 content-type: %q", resp)
	}
	if !strings.Contains(resp, "hello GET /x") {
		t.Fatalf("响应体不符: %q", resp)
	}
}
