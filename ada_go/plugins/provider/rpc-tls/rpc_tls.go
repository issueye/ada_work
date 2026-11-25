// Package rpctls 是 P4 的 TLS over TCP 传输:注册 `tls` Network,证书由配置加载。
//
// 协议语义仍由 provider/rpc-json 负责;本包只提供加密字节流。
package rpctls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// ProviderName 是插件 id。
const ProviderName = "rpc-tls"

type Config struct {
	CertFile   string `json:"certFile,omitempty"`
	KeyFile    string `json:"keyFile,omitempty"`
	CAFile     string `json:"caFile,omitempty"`
	ServerName string `json:"serverName,omitempty"`
}

type Transport struct {
	conf Config
}

func (t *Transport) clientConfig(addr seamrpc.Address) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: t.conf.ServerName}
	if cfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr.Name)
		if err != nil {
			return nil, seamrpc.Invalid("tls 地址必须是 host:port")
		}
		cfg.ServerName = host
	}
	if t.conf.CAFile != "" {
		pem, err := os.ReadFile(t.conf.CAFile)
		if err != nil {
			return nil, seamrpc.Unavailable("读取 CA 失败: " + err.Error())
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, seamrpc.Invalid("CA 文件里没有证书")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func (t *Transport) Dial(ctx context.Context, addr seamrpc.Address) (seamrpc.Stream, error) {
	if addr.Network != seamrpc.NetworkTLS {
		return nil, seamrpc.Unimplemented("rpc-tls 不支持传输 " + string(addr.Network))
	}
	cfg, err := t.clientConfig(addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr.Name)
	if err != nil {
		return nil, seamrpc.Unavailable("连接 " + addr.String() + " 失败: " + err.Error())
	}
	conn := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		_ = conn.Close()
		return nil, seamrpc.AuthFailed("TLS 握手失败: " + err.Error())
	}
	return conn, nil
}

func (t *Transport) Listen(ctx context.Context, addr seamrpc.Address, _ seamrpc.ListenOptions) (seamrpc.Listener, error) {
	if addr.Network != seamrpc.NetworkTLS {
		return nil, seamrpc.Unimplemented("rpc-tls 不支持传输 " + string(addr.Network))
	}
	if t.conf.CertFile == "" || t.conf.KeyFile == "" {
		return nil, seamrpc.Invalid("rpc-tls 监听需要 certFile 与 keyFile")
	}
	cert, err := tls.LoadX509KeyPair(t.conf.CertFile, t.conf.KeyFile)
	if err != nil {
		return nil, seamrpc.Invalid("加载证书失败: " + err.Error())
	}
	ln, err := net.Listen("tcp", addr.Name)
	if err != nil {
		return nil, seamrpc.Unavailable("监听 " + addr.String() + " 失败: " + err.Error())
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
	})
	actual := seamrpc.Address{Network: seamrpc.NetworkTLS, Name: tlsLn.Addr().String()}
	return &listener{ln: tlsLn, addr: actual}, nil
}

type listener struct {
	ln   net.Listener
	addr seamrpc.Address
}

func (l *listener) Accept(ctx context.Context) (seamrpc.Stream, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := l.ln.Accept()
		ch <- result{c: c, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.c, nil
	case <-ctx.Done():
		_ = l.ln.Close()
		return nil, fmt.Errorf("rpc-tls: accept 取消: %w", ctx.Err())
	}
}

func (l *listener) Addr() seamrpc.Address { return l.addr }
func (l *listener) Close() error          { return l.ln.Close() }

// New 构造 TLS 传输(供测试与插件使用)。
func New(conf Config) *Transport { return &Transport{conf: conf} }

// Plugin 返回 rpc-tls 插件:注册 tls 传输。
func Plugin() kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamrpc.Registry](ctx, "rpc-transport")
		if err != nil {
			return nil, err
		}
		var conf Config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "rpc-tls: 解析配置失败: %v", err)
		}
		return reg.Register(seamrpc.NetworkTLS, New(conf))
	}).WithInject("rpc-transport")
}
