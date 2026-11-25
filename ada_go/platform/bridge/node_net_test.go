package bridge_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/bridge"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_jsrt/jsrt"
)

// echoServer 起一个本地 HTTP 服务,证明 node:http 走的是真实网络路径。
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 64)
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		w.Header().Set("X-Echo", "yes")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "method=%s body=%s", r.Method, string(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// echoTCP 起一个本地 TCP 服务,收到 PING 回 PONG。
func echoTCP(t *testing.T) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4)
				if _, rerr := c.Read(buf); rerr == nil {
					_, _ = c.Write([]byte("PONG"))
				}
			}(conn)
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), fmt.Sprint(addr.Port)
}

// numOf 把 JS 数值统一成 int64:moejs 对整数会给出 int64,而不是 float64。
func numOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return -1
	}
}

// TestNodeHTTPDNSNet 用真实本地服务验证 node:http / node:dns / node:net 的可用形状。
func TestNodeHTTPDNSNet(t *testing.T) {
	srv := echoServer(t)
	tcpHost, tcpPort := echoTCP(t)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	script := fmt.Sprintf(`import http from "node:http"
import dns from "node:dns"
import net from "node:net"

type Ctx = { provide: (name: string, value: unknown) => void }

export async function apply(ctx: Ctx) {
  const out = {}
  let body = ""
  http.get(%q, (res) => {
    out.status = res.statusCode
    out.echo = res.headers["x-echo"]
    res.setEncoding("utf8")
    res.on("data", (chunk: string) => { body += chunk })
    res.on("end", () => { out.body = body })
  })

  const req = http.request(%q, { method: "POST", headers: { "content-type": "text/plain" } }, (res) => {
    out.postStatus = res.statusCode
    let text = ""
    res.on("data", (chunk) => { text += chunk.toString() })
    res.on("end", () => { out.postBody = text })
  })
  req.write("hello ")
  req.end("bridge")

  out.lookup = dns.lookupSync("localhost")
  out.lookupCb = ""
  await new Promise<void>((resolve) => {
    dns.lookup("localhost", (err, address, family) => {
      out.lookupCb = err ? "err" : address + "/" + family
      resolve()
    })
  })

  out.ip4 = net.isIP("127.0.0.1")
  out.ip6 = net.isIP("::1")
  out.ipBad = net.isIP("999.1.1.1")
  out.ipv4 = net.isIPv4("10.0.0.1")

  out.reply = ""
  await new Promise<void>((resolve, reject) => {
    const sock = net.connect(%q, %q)
    sock.on("connect", () => sock.write("PING"))
    sock.on("data", (chunk: any) => {
      out.reply = chunk.toString()
      sock.end()
      resolve()
    })
    sock.on("error", reject)
  })

  try {
    http.createServer()
  } catch (e) {
    out.httpServer = e.code
  }
  try {
    net.createServer()
  } catch (e) {
    out.netServer = e.code
  }
  ctx.provide("net.probe", out)
}`, srv.URL, srv.URL, tcpPort, tcpHost)

	art := buildArtifact(t, map[string]string{"plugin.ts": script})
	p, err := bridge.New(bridge.Options{Name: "node-net", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("net.probe")
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(*jsrt.Service).Data()

	if numOf(data["status"]) != 200 {
		t.Fatalf("http.get 状态码不对: %+v", data)
	}
	if data["echo"] != "yes" {
		t.Fatalf("http 响应头没取到: %+v", data["echo"])
	}
	if got, _ := data["body"].(string); !strings.Contains(got, "method=GET") {
		t.Fatalf("http.get 响应体不对: %q", data["body"])
	}
	if numOf(data["postStatus"]) != 200 {
		t.Fatalf("http.request 状态码不对: %+v", data["postStatus"])
	}
	if got, _ := data["postBody"].(string); !strings.Contains(got, "body=hello bridge") {
		t.Fatalf("http.request 请求体没送到: %q", data["postBody"])
	}

	if numOf(data["ip4"]) != 4 || numOf(data["ip6"]) != 6 || numOf(data["ipBad"]) != 0 || data["ipv4"] != true {
		t.Fatalf("net.isIP 判定不对: %+v", data)
	}
	if got, _ := data["reply"].(string); got != "PONG" {
		t.Fatalf("net socket 读取不对: %q", got)
	}
	if data["httpServer"] != "ERR_NOT_SUPPORTED" || data["netServer"] != "ERR_NOT_SUPPORTED" {
		t.Fatalf("服务端 API 必须拒绝: %+v", data)
	}

	lookup, _ := data["lookup"].(map[string]any)
	if lookup == nil || lookup["address"] == "" {
		t.Fatalf("dns.lookupSync 结果不对: %+v", data["lookup"])
	}
	if got, _ := data["lookupCb"].(string); got == "" || got == "err" {
		t.Fatalf("dns.lookup 回调结果不对: %q", got)
	}
}

// TestNodeHTTPError 固定失败可见性:连接不上时 get 抛错,而不是静默返回空响应。
func TestNodeHTTPError(t *testing.T) {
	// 先占一个端口再关掉,保证端口上没有监听者。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	script := fmt.Sprintf(`import http from "node:http"
export function apply(ctx: any) {
  let code = ""
  try {
    http.get("http://127.0.0.1:%d/boom", () => {})
  } catch (e) {
    code = e.code || "EUNKNOWN"
  }
  ctx.provide("net.error", { code })
}`, port)

	art := buildArtifact(t, map[string]string{"plugin.ts": script})
	p, err := bridge.New(bridge.Options{Name: "node-net-error", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("net.error")
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(*jsrt.Service).Data()
	if data["code"] != "EHTTP" {
		t.Fatalf("连接失败应抛 EHTTP: %+v", data)
	}
}

// TestNodeHTTPSInsecure 覆盖 node:https 与 rejectUnauthorized:false。
// 用 httptest 的自签证书,证明 TLS 路径真的走通了,而不是只导出函数。
func TestNodeHTTPSInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure"))
	}))
	t.Cleanup(srv.Close)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })

	script := fmt.Sprintf(`import https from "node:https"
export function apply(ctx: any) {
  const out: any = { text: "" }
  https.get(%q, { rejectUnauthorized: false }, (res: any) => {
    res.setEncoding("utf8")
    res.on("data", (chunk: string) => { out.text += chunk })
    res.on("end", () => { out.status = res.statusCode })
  })
  ctx.provide("https.probe", out)
}`, srv.URL)

	art := buildArtifact(t, map[string]string{"plugin.ts": script})
	p, err := bridge.New(bridge.Options{Name: "node-https", Artifact: art})
	if err != nil {
		t.Fatal(err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("https.probe")
	if err != nil {
		t.Fatal(err)
	}
	data := raw.(*jsrt.Service).Data()
	if data["text"] != "secure" || numOf(data["status"]) != 200 {
		t.Fatalf("https 请求不对: %+v", data)
	}
}
