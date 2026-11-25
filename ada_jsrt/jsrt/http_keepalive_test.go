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

// keepAliveConn 依次返回多条原始请求，随后阻塞到 Close。
type keepAliveConn struct {
	mu       sync.Mutex
	requests [][]byte
	written  []byte
	closed   chan struct{}
	once     sync.Once
}

func newKeepAliveConn(requests ...string) *keepAliveConn {
	c := &keepAliveConn{closed: make(chan struct{})}
	for _, r := range requests {
		c.requests = append(c.requests, []byte(r))
	}
	return c
}

func (c *keepAliveConn) Read(ctx context.Context, p []byte) (int, error) {
	c.mu.Lock()
	if len(c.requests) > 0 {
		req := c.requests[0]
		c.requests = c.requests[1:]
		c.mu.Unlock()
		return copy(p, req), nil
	}
	c.mu.Unlock()
	select {
	case <-c.closed:
		return 0, io.EOF
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (c *keepAliveConn) Write(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, p...)
	c.mu.Unlock()
	return len(p), nil
}

func (c *keepAliveConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
func (c *keepAliveConn) RemoteAddr() string { return "127.0.0.1:5555" }
func (c *keepAliveConn) LocalAddr() string  { return "127.0.0.1:12345" }
func (c *keepAliveConn) response() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func TestHTTPKeepAlive(t *testing.T) {
	conn := newKeepAliveConn(
		"GET /one HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET /two HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
	)
	ln := newFakeListener(conn)
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const seen: string[] = [];
    const srv = http.createServer((req: any, res: any) => {
      seen.push(req.url);
      res.end("hello " + req.url);
      if (seen.length === 2) {
        srv.close();
        resolve(seen.join(","));
      }
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
			Enabled: true, Inbound: true, AllowPrivateIPs: true,
			Protocols: []string{"tcp"}, Listen: []string{"127.0.0.1:0"},
		}},
	})

	got, err := rt.Call(context.Background(), "run")
	if err != nil || got != "/one,/two" {
		t.Fatalf("keep-alive 请求结果不符: got=%v err=%v", got, err)
	}
	resp := conn.response()
	if strings.Count(resp, "HTTP/1.1 200 OK") != 2 {
		t.Fatalf("应返回两个响应: %q", resp)
	}
	if !strings.Contains(resp, "hello /one") || !strings.Contains(resp, "hello /two") {
		t.Fatalf("响应体不符: %q", resp)
	}
	if !strings.Contains(resp, "Connection: keep-alive") || !strings.Contains(resp, "Connection: close") {
		t.Fatalf("keep-alive/close 头不符: %q", resp)
	}
}

func TestHTTPChunkedResponse(t *testing.T) {
	conn := newKeepAliveConn("GET /chunked HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	ln := newFakeListener(conn)
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = http.createServer((req: any, res: any) => {
      res.writeHead(200, { "content-type": "text/plain" });
      res.write("hello ");
      res.end("world");
      srv.close();
      resolve("done");
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
			Enabled: true, Inbound: true, AllowPrivateIPs: true,
			Protocols: []string{"tcp"}, Listen: []string{"127.0.0.1:0"},
		}},
	})
	if _, err := rt.Call(context.Background(), "run"); err != nil {
		t.Fatalf("chunked 服务端失败: %v", err)
	}
	resp := conn.response()
	if !strings.Contains(resp, "Transfer-Encoding: chunked") {
		t.Fatalf("缺少 chunked 头: %q", resp)
	}
	if !strings.Contains(resp, "6\r\nhello \r\n") || !strings.Contains(resp, "5\r\nworld\r\n") {
		t.Fatalf("chunk 编码不符: %q", resp)
	}
	if !strings.HasSuffix(resp, "0\r\n\r\n") {
		t.Fatalf("缺少终止 chunk: %q", resp)
	}
}

func TestHTTPChunkedRequest(t *testing.T) {
	conn := newKeepAliveConn(
		"POST /chunk HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n" +
			"4\r\nWiki\r\n5\r\npedia\r\n0\r\n\r\n",
	)
	ln := newFakeListener(conn)
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = http.createServer((req: any, res: any) => {
      let body = "";
      req.on("data", (chunk: any) => { body += chunk });
      req.on("end", () => {
        res.end("got:" + body);
        srv.close();
        resolve(body);
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
			Enabled: true, Inbound: true, AllowPrivateIPs: true,
			Protocols: []string{"tcp"}, Listen: []string{"127.0.0.1:0"},
		}},
	})
	got, err := rt.Call(context.Background(), "run")
	if err != nil || got != "Wikipedia" {
		t.Fatalf("chunked 请求体解析不符: got=%v err=%v", got, err)
	}
	if resp := conn.response(); !strings.Contains(resp, "got:Wikipedia") {
		t.Fatalf("chunked 请求响应不符: %q", resp)
	}
}

func TestHTTPMaxRequestBody(t *testing.T) {
	conn := newKeepAliveConn("POST /big HTTP/1.1\r\nHost: x\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello")
	ln := newFakeListener(conn)
	rt := newResident(t, `
import http from "node:http";
export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = http.createServer({ maxBodyBytes: 4 }, (req: any, res: any) => {
      res.end("unexpected");
      resolve("handled");
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
			Enabled: true, Inbound: true, AllowPrivateIPs: true,
			Protocols: []string{"tcp"}, Listen: []string{"127.0.0.1:0"},
		}},
	})
	// 请求在 maxBodyBytes 校验处被 413 拒绝，Promise 永远不会 resolve；
	// 用短超时确认调用没有挂住，并检查原始响应。
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = rt.Call(ctx, "run")
	resp := conn.response()
	if !strings.Contains(resp, "413 Payload Too Large") {
		t.Fatalf("超过 maxBodyBytes 应返回 413: %q", resp)
	}
}
