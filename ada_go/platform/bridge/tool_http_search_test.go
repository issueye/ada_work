package bridge_test

import (
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

// ddgFixture 是 html.duckduckgo.com 的简化形态:两个结果、跳转链接包装、
// 标题里的 &amp; 与摘要里的 <b> 都需要被解析掉。
const ddgFixture = `<html><body>
<div class="result results_links results_links_deep web-result">
  <div class="links_main links_deep result__body">
    <h2 class="result__title">
      <a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.org%2Fpage%3Fv%3D1&amp;rut=xyz">Example &amp; Title</a>
    </h2>
    <a class="result__snippet" href="https://example.org/page">Snippet <b>text</b> here with &lt;tags&gt;</a>
  </div>
</div>
<div class="result results_links web-result">
  <div class="links_main result__body">
    <h2 class="result__title">
      <a class="result__a" href="https://second.example/start">Second result</a>
    </h2>
    <div class="result__snippet">另一个结果 &amp; 摘要</div>
  </div>
</div>
</body></html>`

// mountHTTPHelper 以给定配置装载 http-helper(TS 现场编译)。
func mountHTTPHelper(t *testing.T, app *kernel.App, cfg map[string]any) *tools.Registry {
	t.Helper()
	manifest, err := plugin.Load("../../plugins/tool/http-helper")
	if err != nil {
		t.Fatalf("读取 http-helper 清单失败: %v", err)
	}
	lookup := loader.Lookup(loader.Options{Go: builtins.Plugins()})
	p, err := lookup(manifest)
	if err != nil {
		t.Fatalf("构造 http-helper 失败: %v", err)
	}
	f, err := app.Plugin(p, kernel.NewConfig(cfg))
	if err != nil {
		t.Fatalf("挂载 http-helper 失败: %v", err)
	}
	if err := f.Ready(context.Background()); err != nil {
		t.Fatalf("http-helper 未就绪: %v", err)
	}
	raw, err := app.Root().Service("tools")
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*tools.Registry)
}

func callTool(t *testing.T, reg *tools.Registry, name string, args map[string]any) tools.Result {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Invoke(context.Background(), tools.CallInfo{Name: name, Args: data})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	return res
}

// TestSearchDeepSeek 覆盖 DeepSeek 网页搜索:key 从 secret 取、请求形状正确、
// 结果归一化,并且 secret 不会出现在工具输出里。
func TestSearchDeepSeek(t *testing.T) {
	const secret = "sk-e2e-deepseek"
	var authHeader string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"rerank_score":2.35,"title":"Ada Go 插件","url":"https://example.com/ada","snippet":"一段短摘要","summary":"一段长摘要","site_name":"Example","date":"2026-01-01T00:00:00Z","authority":3},
			{"rerank_score":1.2,"title":"第二条","url":"https://example.com/2","snippet":"第二段","summary":"","site_name":null,"date":null,"authority":0}
		]}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ADA_SECRET_DEEPSEEK_API_KEY", secret)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	mountBuiltin(t, app, "seam-secrets", nil)
	mountBuiltin(t, app, "secrets-env", map[string]any{"allow": []any{"DEEPSEEK_API_KEY"}})

	reg := mountHTTPHelper(t, app, map[string]any{
		"deepseekEndpoint":  srv.URL + "/v1/tools/search",
		"searchProvider":    "deepseek",
		"deepseekKeySecret": "DEEPSEEK_API_KEY",
	})

	res := callTool(t, reg, "http_search", map[string]any{"query": "ada go 插件"})
	if res.Status != tools.StatusOK {
		t.Fatalf("搜索失败: %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "Ada Go 插件") || !strings.Contains(text, "https://example.com/ada") || !strings.Contains(text, "一段短摘要") {
		t.Fatalf("DeepSeek 结果没有归一化: %q", text)
	}
	if strings.Contains(text, secret) {
		t.Fatalf("工具结果泄露了 secret: %q", text)
	}
	if authHeader != "Bearer "+secret {
		t.Fatalf("Authorization 不对: %q", authHeader)
	}
	if body["query"] != "ada go 插件" || body["count"] != float64(5) || body["need_content"] != false {
		t.Fatalf("DeepSeek 请求体不对: %+v", body)
	}
	var details map[string]any
	raw, _ := json.Marshal(res.Details)
	_ = json.Unmarshal(raw, &details)
	if details["provider"] != "deepseek" || details["count"] != float64(2) {
		t.Fatalf("details 不对: %+v", details)
	}
}

// TestSearchDeepSeekWithoutSecret 守住 fail-closed:显式选 deepseek 但没有 secret 时
// 必须报错并指出配置方式,而不是悄悄换成别的引擎。
func TestSearchDeepSeekWithoutSecret(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	reg := mountHTTPHelper(t, app, map[string]any{"searchProvider": "deepseek"})

	res := callTool(t, reg, "http_search", map[string]any{"query": "x"})
	if res.Status != tools.StatusError {
		t.Fatalf("缺 secret 时应返回 error: %+v", res)
	}
	if got := res.Content[0].Text; !strings.Contains(got, "DEEPSEEK_API_KEY") || !strings.Contains(got, "seam-secrets") {
		t.Fatalf("错误信息没有配置指引: %q", got)
	}
}

// TestSearchDuckDuckGo 覆盖无 key 的 DuckDuckGo HTML 解析(含实体与跳转链接还原)。
func TestSearchDuckDuckGo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			http.Error(w, "missing q", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ddgFixture))
	}))
	t.Cleanup(srv.Close)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	reg := mountHTTPHelper(t, app, map[string]any{
		"duckduckgoEndpoint": srv.URL + "/html/",
		"searchProvider":     "duckduckgo",
	})

	res := callTool(t, reg, "http_search", map[string]any{"query": "ada", "maxResults": 2})
	if res.Status != tools.StatusOK {
		t.Fatalf("DDG 搜索失败: %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "Example & Title") {
		t.Fatalf("标题实体没有解码: %q", text)
	}
	if !strings.Contains(text, "https://example.org/page?v=1") {
		t.Fatalf("跳转链接没有还原: %q", text)
	}
	if !strings.Contains(text, "Snippet text here with <tags>") {
		t.Fatalf("摘要标签没有清理: %q", text)
	}
	if !strings.Contains(text, "另一个结果 & 摘要") {
		t.Fatalf("第二条结果不对: %q", text)
	}
}

// TestSearchAutoFallsBackToDuckDuckGo 覆盖 provider=auto 且没有 secrets 服务时
// 自动用 DuckDuckGo(默认配置开箱可用)。
func TestSearchAutoFallsBackToDuckDuckGo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ddgFixture))
	}))
	t.Cleanup(srv.Close)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	reg := mountHTTPHelper(t, app, map[string]any{
		"duckduckgoEndpoint": srv.URL + "/html/",
	})

	res := callTool(t, reg, "http_search", map[string]any{"query": "ada"})
	if res.Status != tools.StatusOK {
		t.Fatalf("auto 搜索失败: %+v", res)
	}
	var details map[string]any
	raw, _ := json.Marshal(res.Details)
	_ = json.Unmarshal(raw, &details)
	if details["provider"] != "duckduckgo" {
		t.Fatalf("auto 应退到 duckduckgo: %+v", details)
	}
}

// TestSearchSearXNG 覆盖自建 SearXNG 的 JSON 接口(第三种引擎)。
func TestSearchSearXNG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"results":[{"title":"SearX 结果","url":"https://s.example/1","content":"来自 searxng 的摘要","engine":"duckduckgo"}]}`)
	}))
	t.Cleanup(srv.Close)

	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mountBuiltin(t, app, "seam-tools", nil)
	reg := mountHTTPHelper(t, app, map[string]any{
		"searxEndpoint":  srv.URL,
		"searchProvider": "searxng",
	})

	res := callTool(t, reg, "http_search", map[string]any{"query": "ada"})
	if res.Status != tools.StatusOK {
		t.Fatalf("searxng 搜索失败: %+v", res)
	}
	if got := res.Content[0].Text; !strings.Contains(got, "SearX 结果") || !strings.Contains(got, "https://s.example/1") {
		t.Fatalf("searxng 结果不对: %q", got)
	}
}
