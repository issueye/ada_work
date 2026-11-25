package jsrt_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

type scriptedConn struct {
	read bool
}

func (c *scriptedConn) Read(_ context.Context, p []byte) (int, error) {
	if c.read {
		return 0, io.EOF
	}
	c.read = true
	return copy(p, "PONG"), nil
}

func (c *scriptedConn) Write(_ context.Context, p []byte) (int, error) { return len(p), nil }
func (c *scriptedConn) Close() error                                   { return nil }
func (c *scriptedConn) RemoteAddr() string                             { return "127.0.0.1:9" }
func (c *scriptedConn) LocalAddr() string                              { return "127.0.0.1:1" }

func TestNetAsyncConnectAndData(t *testing.T) {
	art, err := compile.Build("net-async", "plugin.ts", map[string]string{
		"plugin.ts": `
import net from "node:net";

export function echo(host: string, port: number): Promise<string> {
  return new Promise((resolve, reject) => {
    const s = net.connect({ host, port });
    s.on("connect", () => s.write("PING"));
    s.on("data", (chunk: any) => { s.end(); resolve(chunk.toString()) });
    s.on("error", reject);
  });
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}

	var dials int
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			NetDial: func(context.Context, string, string, time.Duration) (jsrt.NetConn, error) {
				dials++
				return &scriptedConn{}, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled:         true,
			Outbound:        true,
			AllowPrivateIPs: true,
			Allow:           []string{"tcp://127.0.0.1:9"},
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	got, err := rt.Call(context.Background(), "echo", "127.0.0.1", 9)
	if err != nil || got != "PONG" {
		t.Fatalf("异步 socket 结果不符: got=%v err=%v", got, err)
	}
	if dials != 1 {
		t.Fatalf("期望一次 dial，实际 %d", dials)
	}
}
