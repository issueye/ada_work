package registry_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/host/registry"
)

func setup(t *testing.T) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "repo.sig.json"), []byte(`{"keyId":"k1"}`), 0o644); err != nil {
		t.Fatalf("写签名失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "p.zip"), []byte("zipdata"), 0o644); err != nil {
		t.Fatalf("写 artifact 失败: %v", err)
	}
	h, err := registry.NewHandler(registry.Options{Root: root})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
		t.Fatal(err)
	}
	return h, root
}

// 1. 索引/签名/artifact 可读取。
func TestServe(t *testing.T) {
	h, _ := setup(t)
	for _, tc := range []struct{ path, want string }{
		{"/repo.json", `{"entries":[]}`},
		{"/repo.sig.json", `{"keyId":"k1"}`},
		{"/artifacts/p.zip", "zipdata"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK || rec.Body.String() != tc.want {
			t.Fatalf("%s 返回不正确: %d %q", tc.path, rec.Code, rec.Body.String())
		}
	}
}

// 2. Token 认证。
func TestAuth(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	h, err := registry.NewHandler(registry.Options{Root: root, Token: "secret"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	noAuth := httptest.NewRequest(http.MethodGet, "/repo.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, noAuth)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺 token 应 401,实际 %d", rec.Code)
	}
	withAuth := httptest.NewRequest(http.MethodGet, "/repo.json", nil)
	withAuth.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withAuth)
	if rec.Code != http.StatusOK {
		t.Fatalf("正确 token 应 200,实际 %d", rec.Code)
	}
}

// 3. Serve:监听 :0 + 请求 + ctx 取消优雅关闭。
func TestServeGraceful(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- registry.Serve(ctx, registry.Options{Root: root}, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/repo.json")
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

// 4. TLS:Serve 用自签名证书提供 repo.json。
func TestServeTLSPull(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	certPath, keyPath := tlsCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- registry.Serve(ctx, registry.Options{Root: root, CertFile: certPath, KeyFile: keyPath}, ln)
	}()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/repo.json")
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
			t.Fatalf("TLS 退出应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve 未在超时内退出")
	}
}

func tlsCert(t *testing.T) (string, string) {
	t.Helper()
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
	return certPath, keyPath
}

// 5. 固定窗口限流:超限 429,窗口后复位。
func TestRateLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := registry.NewHandler(registry.Options{
		Root: root, RateLimit: 2,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo.json", nil))
		return rec
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("第 1 次应 200,实际 %d", rec.Code)
	}
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("第 2 次应 200,实际 %d", rec.Code)
	}
	if rec := get(); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("第 3 次应 429+Retry-After,实际 %d", rec.Code)
	}
	now = now.Add(time.Second)
	if rec := get(); rec.Code != http.StatusOK {
		t.Fatalf("窗口复位后应 200,实际 %d", rec.Code)
	}
}

// 6. CORS:GET 带允许源,OPTIONS 预检 204。
func TestCORS(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "repo.json"), []byte(`{"entries":[]}`), 0o644); err != nil {
		t.Fatalf("写索引失败: %v", err)
	}
	h, err := registry.NewHandler(registry.Options{Root: root, AllowOrigin: "https://app.example"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repo.json", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("GET CORS 头不正确: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/repo.json", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检应 204,实际 %d", rec.Code)
	}
}
