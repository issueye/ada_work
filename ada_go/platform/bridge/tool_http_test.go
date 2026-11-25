package bridge_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/issueye/ada_go_v2/platform/host/builtins"
	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/platform/loader"
	"github.com/issueye/ada_go_v2/platform/plugin"
	"github.com/issueye/ada_go_v2/plugins/seam/tools"
)

// httpProbeServer 覆盖 http-helper 需要证明的几条路径:
// JSON 响应、请求体回显、重定向、gzip 压缩、超大响应。
func httpProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Probe", r.Header.Get("X-Test"))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": "/json"})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 0, 64)
		buf := make([]byte, 64)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "method=%s type=%s query=%s clen=%d body=%s", r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery, r.ContentLength, string(body))
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/json", http.StatusFound)
	})
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte("gzip 正文:压缩后由 node:zlib 解开"))
		_ = zw.Close()
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>测试标题</title>
<meta name="description" content="不该出现在结果里">
<style>.a{color:red}</style>
<script>var secret = 1</script>
</head><body>
<h1>正文标题</h1>
<p>第一段 &amp; 实体</p>
<div>第二段</div>
</body></html>`))
	})
	mux.HandleFunc("/html-nocontent", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>无类型</title><script>var x=1</script></head><body><p>正文</p></body></html>`))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestJSToolHTTPHelper 端到端验证 http-helper:装配 → 工具注册 → 真实请求。
func TestJSToolHTTPHelper(t *testing.T) {
	srv := httpProbeServer(t)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)

	manifest, err := plugin.Load("../../plugins/tool/http-helper")
	if err != nil {
		t.Fatalf("读取 http-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 http-helper 失败: %v", err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)

	call := func(args map[string]any) (tools.Result, error) {
		t.Helper()
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return reg.Invoke(context.Background(), tools.CallInfo{Name: "http_request", Args: data})
	}

	text := func(res tools.Result) string {
		if len(res.Content) == 0 {
			return ""
		}
		return res.Content[0].Text
	}
	// got 在多个断言间复用;上面几处 `if got := ...` 是各自块内的影子变量。
	var got string

	// 1) GET JSON + 自定义请求头。
	res, err := call(map[string]any{
		"url":     srv.URL + "/json",
		"headers": map[string]any{"x-test": "ada"},
	})
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	if res.Status != tools.StatusOK {
		t.Fatalf("GET 状态应为 ok: %+v", res)
	}
	if got := text(res); !strings.Contains(got, "200") || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("GET 正文不对: %q", got)
	}

	// 2) POST 对象体 → JSON 编码,服务端回显方法与类型。
	res, err = call(map[string]any{
		"url":    srv.URL + "/echo",
		"method": "POST",
		"body":   map[string]any{"name": "ada", "n": 7},
	})
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	if got := text(res); !strings.Contains(got, "method=POST") || !strings.Contains(got, "application/json") {
		t.Fatalf("POST 回显不对: %q", got)
	}

	// 3) 表单体 + query 参数:数组值展开成重复键。
	res, err = call(map[string]any{
		"url":      srv.URL + "/echo",
		"method":   "POST",
		"body":     map[string]any{"q": "a b"},
		"bodyType": "form",
		"query":    map[string]any{"probe": 1, "tag": []any{"x", "y"}},
	})
	if err != nil {
		t.Fatalf("表单请求失败: %v", err)
	}
	got = text(res)
	if !strings.Contains(got, "application/x-www-form-urlencoded") || !strings.Contains(got, "probe=1") || !strings.Contains(got, "tag=x&tag=y") && !strings.Contains(got, "tag=x%2Ctag%3Dy") {
		t.Fatalf("表单体或 query 不对: %q", got)
	}

	// 3.1) 非 ASCII 请求体:Content-Length 必须是字节数(6),不是 UTF-16 码元数。
	res, err = call(map[string]any{
		"url":      srv.URL + "/echo",
		"method":   "POST",
		"body":     "中文",
		"bodyType": "text",
	})
	if err != nil {
		t.Fatalf("UTF-8 请求体失败: %v", err)
	}
	if got = text(res); !strings.Contains(got, "clen=6") {
		t.Fatalf("Content-Length 应按字节数计算: %q", got)
	}

	// 3.2) 未允许时 insecure 必须被拒(不能静默忽略证书校验开关)。
	res, err = call(map[string]any{"url": srv.URL + "/json", "insecure": true})
	if err != nil {
		t.Fatalf("insecure 请求调用失败: %v", err)
	}
	if res.Status != tools.StatusError {
		t.Fatalf("insecure 未配置时应返回 error: %+v", res)
	}

	// 4) 重定向:跟随一次,正文来自目标地址。
	res, err = call(map[string]any{"url": srv.URL + "/redirect", "followRedirects": 2})
	if err != nil {
		t.Fatalf("重定向失败: %v", err)
	}
	got = text(res)
	if !strings.Contains(got, "重定向:") || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("重定向正文不对: %q", got)
	}

	// 4.1) followRedirects=0:不跟随,原样把 302 交给模型。
	res, err = call(map[string]any{"url": srv.URL + "/redirect", "followRedirects": 0})
	if err != nil {
		t.Fatalf("零重定向请求失败: %v", err)
	}
	if got = text(res); !strings.Contains(got, "302") || strings.Contains(got, "重定向:") {
		t.Fatalf("followRedirects=0 应原样返回 3xx: %q", got)
	}

	// 5) gzip:响应头声明压缩,工具侧用 node:zlib 解开。
	res, err = call(map[string]any{"url": srv.URL + "/gzip"})
	if err != nil {
		t.Fatalf("gzip 失败: %v", err)
	}
	if got := text(res); !strings.Contains(got, "gzip 正文") {
		t.Fatalf("gzip 正文没有解开: %q", got)
	}

	// 5.1) HTML:默认清洗成纯文本(去脚本/样式/meta,保留标题与正文)。
	res, err = call(map[string]any{"url": srv.URL + "/html"})
	if err != nil {
		t.Fatalf("HTML 请求失败: %v", err)
	}
	got = text(res)
	for _, want := range []string{"标题: 测试标题", "正文标题", "第一段 & 实体", "第二段"} {
		if !strings.Contains(got, want) {
			t.Fatalf("清洗后的正文缺少 %q: %q", want, got)
		}
	}
	for _, bad := range []string{"<html", "<script", "<style", "meta name", "var secret", "<!DOCTYPE"} {
		if strings.Contains(got, bad) {
			t.Fatalf("清洗后仍残留 %q: %q", bad, got)
		}
	}
	var details map[string]any
	detailRaw, _ := json.Marshal(res.Details)
	_ = json.Unmarshal(detailRaw, &details)
	if details["cleaned"] != true || details["kind"] != "html" {
		t.Fatalf("details 应标明已清洗: %+v", details)
	}

	// 5.1b) 没有 content-type 时按开头嗅探成 HTML,仍然清洗。
	res, err = call(map[string]any{"url": srv.URL + "/html-nocontent"})
	if err != nil {
		t.Fatalf("无类型 HTML 请求失败: %v", err)
	}
	if got := text(res); !strings.Contains(got, "标题: 无类型") || strings.Contains(got, "var x=1") {
		t.Fatalf("无 content-type 的网页也应清洗: %q", got)
	}
	// 5.2) clean=false:返回原文。
	res, err = call(map[string]any{"url": srv.URL + "/html", "clean": false})
	if err != nil {
		t.Fatalf("原文请求失败: %v", err)
	}
	if got := text(res); !strings.Contains(got, "<html") || !strings.Contains(got, "var secret") {
		t.Fatalf("clean=false 应返回原文: %q", got)
	}
	// 6) 截断:maxBytes 小于响应体时,正文被宿主截断并告知模型。
	res, err = call(map[string]any{"url": srv.URL + "/big", "maxBytes": 100})
	if err != nil {
		t.Fatalf("截断请求失败: %v", err)
	}
	if got := text(res); !strings.Contains(got, "截断") {
		t.Fatalf("截断标记缺失: %q", got)
	}

	// 7) 连接失败:必须是工具 error,而不是空响应。
	res, err = call(map[string]any{"url": "http://127.0.0.1:1/none"})
	if err != nil {
		// 有些平台会在参数校验阶段就返回错误,两种形态都接受。
		return
	}
	if res.Status != tools.StatusError || !strings.Contains(text(res), "HTTP 请求失败") {
		t.Fatalf("连接失败应返回 error: %+v", res)
	}
}

// TestJSToolHTTPHelperRejectsBadArgs 守住 schema:缺 url 在调用点被拒。
func TestJSToolHTTPHelperRejectsBadArgs(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)

	manifest, err := plugin.Load("../../plugins/tool/http-helper")
	if err != nil {
		t.Fatalf("读取 http-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 http-helper 失败: %v", err)
	}
	mount(t, app, p)

	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	reg := raw.(*tools.Registry)
	// schema 失败在 tools.Registry 里被映射成 StatusError(不返回 error),
	// 与「工具执行失败」对模型是同一类可见结果。
	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: "http_request", Args: json.RawMessage(`{"method":"GET"}`)})
	if err != nil {
		t.Fatalf("调用本身不应失败: %v", err)
	}
	if res.Status != tools.StatusError {
		t.Fatalf("缺少 url 时状态应为 error: %+v", res)
	}
	// 错误码在 seam 边界保留,具体字段名在 wrapped 详情里(finish 只渲染摘要)。
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "ESCHEMA") {
		t.Fatalf("错误应带 ESCHEMA 码: %+v", res)
	}
}
