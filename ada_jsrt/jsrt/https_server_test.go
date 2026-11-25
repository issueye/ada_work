package jsrt_test

import (
	"context"
	"strings"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

func TestHTTPSServerOverTLSListener(t *testing.T) {
	conn := &httpConn{}
	ln := newFakeListener(conn)
	var listenArgs []string
	rt := newResident(t, `
import https from "node:https";

export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = https.createServer((req: any, res: any) => {
      res.writeHead(200, "OK", { "content-type": "text/plain" });
      res.end("secure " + req.method + " " + req.url);
      resolve(req.url);
      srv.close();
    });
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1");
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			TLSListen: func(_ context.Context, _ string, address string, _ jsrt.TLSOptions) (jsrt.NetListener, error) {
				listenArgs = append(listenArgs, address)
				return ln, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Inbound:         true,
			AllowPrivateIPs: true,
			Protocols:       []string{"tls"},
			Listen:          []string{"127.0.0.1:0"},
		}},
	})

	got, err := rt.Call(context.Background(), "run")
	if err != nil {
		t.Fatalf("HTTPS server 失败: %v", err)
	}
	if got != "/x" {
		t.Fatalf("HTTPS 请求 URL 不符: %v", got)
	}
	if len(listenArgs) != 1 || listenArgs[0] != "127.0.0.1:0" {
		t.Fatalf("TLS 监听参数不符: %v", listenArgs)
	}
	resp := conn.response()
	if !strings.HasPrefix(resp, "HTTP/1.1 200 OK\r\n") || !strings.Contains(resp, "secure GET /x") {
		t.Fatalf("HTTPS 响应不符: %q", resp)
	}
}
