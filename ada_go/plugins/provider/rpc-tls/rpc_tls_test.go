package rpctls_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	rpcjson "github.com/issueye/ada_go_v2/plugins/provider/rpc-json"
	rpctls "github.com/issueye/ada_go_v2/plugins/provider/rpc-tls"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

type certs struct {
	cert, key, ca string
}

func makeCerts(t *testing.T) certs {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 CA 密钥失败: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("生成 CA 失败: %v", err)
	}
	serverKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caTmpl, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("生成服务器证书失败: %v", err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writePEM(t, caPath, "CERTIFICATE", caDER)
	writePEM(t, certPath, "CERTIFICATE", srvDER)
	keyDER, _ := x509.MarshalECPrivateKey(serverKey)
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certs{cert: certPath, key: keyPath, ca: caPath}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写 %s 失败: %v", path, err)
	}
}

func newTransport(t *testing.T, conf rpctls.Config) *rpctls.Transport {
	t.Helper()
	return rpctls.New(conf)
}

// 1. TLS 传输往返。
func TestTLSRoundTrip(t *testing.T) {
	c := makeCerts(t)
	tr := newTransport(t, rpctls.Config{CertFile: c.cert, KeyFile: c.key, CAFile: c.ca})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := tr.Listen(ctx, seamrpc.Address{Network: seamrpc.NetworkTLS, Name: "127.0.0.1:0"}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	go func() {
		st, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = io.Copy(st, st)
	}()
	client := newTransport(t, rpctls.Config{CAFile: c.ca, ServerName: "127.0.0.1"})
	st, err := client.Dial(ctx, ln.Addr())
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer st.Close()
	if _, err := st.Write([]byte("hi")); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("读失败: %v %q", err, buf)
	}
}

// 2. 没有正确 CA 时握手失败。
func TestTLSRejectsUnknownCA(t *testing.T) {
	c := makeCerts(t)
	tr := newTransport(t, rpctls.Config{CertFile: c.cert, KeyFile: c.key, CAFile: c.ca})
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.NetworkTLS, Name: "127.0.0.1:0"}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// 让服务端完成 TLS 握手后再因客户端不信任 CA 而失败。
		if st, err := ln.Accept(ctx); err == nil {
			_ = st.Close()
		}
	}()
	client := newTransport(t, rpctls.Config{ServerName: "127.0.0.1"})
	if _, err := client.Dial(ctx, ln.Addr()); err == nil {
		t.Fatalf("未知 CA 应失败")
	}
}

// 3. 注册 tls 后,内核服务能取到 rpc-transport。
func TestPlugin(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin) {
		if _, err := app.Plugin(p, kernel.NewConfig(nil)); err != nil {
			t.Fatalf("挂载失败: %v", err)
		}
	}
	mount(seamrpc.Plugin())
	mount(rpctls.Plugin())
	reg, err := kernel.Service[*seamrpc.Registry](app.Root(), "rpc-transport")
	if err != nil {
		t.Fatalf("取 registry 失败: %v", err)
	}
	if _, ok := reg.Lookup(seamrpc.NetworkTLS); !ok {
		t.Fatalf("tls 传输未注册")
	}
}

// 4. TLS + rpc-json 协议 + token 鉴权端到端。
func TestTLSRPCIntegration(t *testing.T) {
	c := makeCerts(t)
	app := kernel.New(kernel.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = app.Stop(context.Background()) })
	mount := func(p kernel.Plugin, cfg map[string]any) {
		if _, err := app.Plugin(p, kernel.NewConfig(cfg)); err != nil {
			t.Fatalf("挂载失败: %v", err)
		}
	}
	mount(seamrpc.Plugin(), nil)
	mount(rpcjson.Plugin(), nil)
	mount(rpctls.Plugin(), map[string]any{"certFile": c.cert, "keyFile": c.key, "caFile": c.ca})
	svc, err := kernel.Service[*seamrpc.Chain](app.Root(), "rpc")
	if err != nil {
		t.Fatalf("取 rpc 服务失败: %v", err)
	}
	s, err := svc.Get()
	if err != nil {
		t.Fatalf("组装 rpc 失败: %v", err)
	}
	srv, err := s.Listen(ctx, seamrpc.Address{Network: seamrpc.NetworkTLS, Name: "127.0.0.1:0"}, seamrpc.ServerOptions{
		Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: []byte("s3")}, PeerIDs: []string{"cli"},
	})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	if _, err := srv.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		return params, nil
	}); err != nil {
		t.Fatalf("注册方法失败: %v", err)
	}
	go func() { _ = srv.Serve(ctx) }()
	addr := srv.Stats().Listeners[0]
	peer, err := s.Dial(ctx, addr, seamrpc.DialOptions{Client: "cli", Auth: seamrpc.Auth{Mode: seamrpc.AuthToken, Token: []byte("s3")}})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	var out json.RawMessage
	if err := peer.Call(ctx, "echo", map[string]string{"x": "y"}, &out); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("调用结果为空")
	}
}
