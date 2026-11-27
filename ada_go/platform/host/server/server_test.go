package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/host/server"
	"github.com/issueye/ada_go_v2/platform/install"
)

func newHandler(t *testing.T, root string) http.Handler {
	t.Helper()
	h, err := server.NewHandler(server.Options{Root: root})
	if err != nil {
		t.Fatalf("构造 handler 失败: %v", err)
	}
	return h
}

func get(t *testing.T, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// 1. plan:合法目录返回顺序。
func TestPlan(t *testing.T) {
	h := newHandler(t, "../../../examples")
	rec := get(t, h, "/api/plan?path=plugins")
	if rec.Code != http.StatusOK {
		t.Fatalf("plan 应 200,实际 %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		OK    bool     `json:"ok"`
		Order []string `json:"order"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !payload.OK || len(payload.Order) != 3 {
		t.Fatalf("plan 内容不正确: %+v", payload)
	}
}

// 2. caps:静态授权检查。
func TestCaps(t *testing.T) {
	h := newHandler(t, "../../../examples")
	rec := get(t, h, "/api/caps?path=plugins")
	if rec.Code != http.StatusOK {
		t.Fatalf("caps 应 200,实际 %d", rec.Code)
	}
	var payload struct {
		OK      bool `json:"ok"`
		Plugins []struct {
			ID string `json:"id"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !payload.OK || len(payload.Plugins) != 3 {
		t.Fatalf("caps 内容不正确: %+v", payload)
	}
}

// 3. 路径逃逸 → 400。
func TestEscape(t *testing.T) {
	h := newHandler(t, "../../../examples")
	for _, path := range []string{"..", "../..", "/etc"} {
		rec := get(t, h, "/api/plan?path="+path)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path=%q 应 400,实际 %d", path, rec.Code)
		}
	}
}

// 4. 非 GET → 405。
func TestMethod(t *testing.T) {
	h := newHandler(t, "../../../examples")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/plan?path=plugins", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 应 405,实际 %d", rec.Code)
	}
}

// 5. list:读取安装索引。
func TestList(t *testing.T) {
	root := t.TempDir()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "plugin.json"),
		[]byte(`{"id":"srv-demo","version":"0.1.0","kind":"go"}`), 0o644); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	if _, err := install.Install(src, filepath.Join(root, "dest"), install.Options{}); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	h := newHandler(t, root)
	rec := get(t, h, "/api/list?dest=dest")
	if rec.Code != http.StatusOK {
		t.Fatalf("list 应 200,实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "srv-demo") {
		t.Fatalf("list 输出缺少条目: %s", rec.Body.String())
	}
}

// 6. 根路由列出可用接口。
func TestIndex(t *testing.T) {
	h := newHandler(t, "../../../examples")
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index 应 200,实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/api/plan") {
		t.Fatalf("index 输出不正确: %s", rec.Body.String())
	}
}

// 7. inspect:临时挂载内置安全栈并返回快照。
func TestInspect(t *testing.T) {
	root := t.TempDir()
	stack := filepath.Join(root, "stack")
	workspace := filepath.Join(root, "workspace")
	write := func(id, body string) {
		t.Helper()
		dir := filepath.Join(stack, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("写清单失败: %v", err)
		}
	}
	write("seam-fs", `{"id":"seam-fs","version":"0.1.0","kind":"go","provide":["fs"]}`)
	write("fs-local", `{"id":"fs-local","version":"0.1.0","kind":"go","inject":{"required":["fs"]},"config":{"workspace":"`+strings.ReplaceAll(workspace, `\`, `\\`)+`"}}`)
	write("caps-static", `{"id":"caps-static","version":"0.1.0","kind":"go","provide":["caps"]}`)
	write("caps-gate", `{"id":"caps-gate","version":"0.1.0","kind":"go","inject":{"required":["fs"],"optional":["caps"]}}`)

	h := newHandler(t, root)
	rec := get(t, h, "/api/inspect?path=stack")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect 应 200,实际 %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Order  []string `json:"order"`
		Fibers []struct {
			Plugin string `json:"plugin"`
			State  string `json:"state"`
		} `json:"fibers"`
		Services []struct {
			Name string `json:"name"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(payload.Fibers) != 4 {
		t.Fatalf("应挂载 4 个插件,实际 %d: %+v", len(payload.Fibers), payload.Fibers)
	}
	found := false
	for _, sv := range payload.Services {
		if sv.Name == "fs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("快照里缺少 fs 服务: %+v", payload.Services)
	}
}

// 8. inspect:未知路径 → 400。
func TestInspectBadPath(t *testing.T) {
	h := newHandler(t, "../../../examples")
	rec := get(t, h, "/api/inspect?path=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知路径应 400,实际 %d", rec.Code)
	}
}

// 9. 令牌认证:缺失/错误 401,正确放行。
func TestAuth(t *testing.T) {
	h, err := server.NewHandler(server.Options{Root: "../../../examples", Token: "secret"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	request := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/plan?path=plugins", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺 token 应 401,实际 %d", rec.Code)
	}
	if rec := request("Bearer wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401,实际 %d", rec.Code)
	}
	if rec := request("Bearer secret"); rec.Code != http.StatusOK {
		t.Fatalf("正确 token 应 200,实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 10. verify:合法插件返回清单信息;未知路径 400。
// 根目录指向模块根(而不是 examples):插件清单已统一到 plugins/ 下。
func TestVerify(t *testing.T) {
	h := newHandler(t, "../../..")
	rec := get(t, h, "/api/verify?path=plugins/tool/git-helper")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify 应 200,实际 %d: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ID     string   `json:"id"`
		Kind   string   `json:"kind"`
		Caps   []string `json:"caps"`
		Signed bool     `json:"signed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.ID != "git-helper" || payload.Kind != "ts" {
		t.Fatalf("verify 内容不正确: %+v", payload)
	}
	rec = get(t, h, "/api/verify?path=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知路径应 400,实际 %d", rec.Code)
	}
}

// 11. 固定窗口限流:超限 429,窗口后复位。
func TestRateLimit(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := server.NewHandler(server.Options{
		Root: "../../../examples", RateLimit: 2,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("第 1 次应 200,实际 %d", rec.Code)
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("第 2 次应 200,实际 %d", rec.Code)
	}
	rec := get()
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("第 3 次应 429+Retry-After,实际 %d", rec.Code)
	}
	now = now.Add(time.Second)
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("窗口复位后应 200,实际 %d", rec.Code)
	}
}

// 12. Serve:监听 :0 + 请求 + ctx 取消优雅关闭。
func TestServeGraceful(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, server.Options{Root: "../../../examples"}, ln)
	}()
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/plan?path=plugins")
	if err != nil {
		cancel()
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("状态码不正确: %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅关闭应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve 未在超时内退出")
	}
}

// 13. TLS:Serve 用自签名证书,客户端跳过校验请求成功。
func TestServeTLS(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, server.Options{Root: "../../../examples", CertFile: certPath, KeyFile: keyPath}, ln)
	}()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/api/plan?path=plugins")
	if err != nil {
		cancel()
		t.Fatalf("TLS 请求失败: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("状态码不正确: %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TLS 服务退出应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve 未在超时内退出")
	}
}

// 14. CORS:GET 带允许源,OPTIONS 预检 204。
func TestCORS(t *testing.T) {
	h, err := server.NewHandler(server.Options{Root: "../../../examples", AllowOrigin: "https://app.example"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/plan?path=plugins", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("GET CORS 头不正确: %d %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/plan", nil))
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "GET") {
		t.Fatalf("预检不正确: %d", rec.Code)
	}
}
