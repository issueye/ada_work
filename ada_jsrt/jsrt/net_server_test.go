package jsrt_test

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/issueye/ada_jsrt/jsrt"
)

type pingConn struct {
	mu   sync.Mutex
	read bool
}

func (c *pingConn) Read(_ context.Context, p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, "PING"), nil
}

func (c *pingConn) Write(_ context.Context, p []byte) (int, error) { return len(p), nil }
func (c *pingConn) Close() error                                   { return nil }
func (c *pingConn) RemoteAddr() string                             { return "127.0.0.1:5555" }
func (c *pingConn) LocalAddr() string                              { return "127.0.0.1:12345" }

type fakeListener struct {
	mu     sync.Mutex
	conns  []jsrt.NetConn
	closed chan struct{}
	once   sync.Once
}

func newFakeListener(conns ...jsrt.NetConn) *fakeListener {
	return &fakeListener{conns: conns, closed: make(chan struct{})}
}

func (l *fakeListener) Accept(ctx context.Context) (jsrt.NetConn, error) {
	l.mu.Lock()
	if len(l.conns) > 0 {
		c := l.conns[0]
		l.conns = l.conns[1:]
		l.mu.Unlock()
		return c, nil
	}
	l.mu.Unlock()
	select {
	case <-l.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() string { return "127.0.0.1:12345" }

func TestNetServerAcceptAndData(t *testing.T) {
	ln := newFakeListener(&pingConn{})
	var listenArgs []string
	rt := newResident(t, `
import net from "node:net";

export function run(): Promise<string> {
  return new Promise((resolve, reject) => {
    const srv = net.createServer((sock: any) => {
      sock.on("data", (chunk: any) => {
        sock.write("PONG");
        resolve(chunk.toString());
        srv.close();
      });
      sock.on("error", reject);
    });
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {});
  });
}
`, jsrt.Options{
		Host: jsrt.Host{
			NetListen: func(_ context.Context, _ string, address string) (jsrt.NetListener, error) {
				listenArgs = append(listenArgs, address)
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
	if err != nil || got != "PING" {
		t.Fatalf("服务端接收/数据结果不符: got=%v err=%v", got, err)
	}
	if len(listenArgs) != 1 || listenArgs[0] != "127.0.0.1:0" {
		t.Fatalf("监听地址不符: %v", listenArgs)
	}
}
