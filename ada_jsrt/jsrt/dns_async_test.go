package jsrt_test

import (
	"context"
	"testing"
	"time"

	"github.com/issueye/ada_jsrt/compile"
	"github.com/issueye/ada_jsrt/jsrt"
)

func TestDNSAsyncCallbackAndPromise(t *testing.T) {
	art, err := compile.Build("dns-async", "plugin.ts", map[string]string{
		"plugin.ts": `
import dns from "node:dns";
import { lookup as lookupPromise } from "node:dns/promises";

export function viaCallback(host: string): Promise<string> {
  return new Promise((resolve, reject) => {
    dns.lookup(host, (err, address, family) => {
      if (err) reject(err);
      else resolve(address + "/" + family);
    });
  });
}

export async function viaPromise(host: string): Promise<string> {
  const r = await lookupPromise(host);
  return r.address + "/" + r.family;
}
`,
	}, compile.Options{})
	if err != nil {
		t.Fatalf("构建 Artifact 失败: %v", err)
	}

	var calls int
	rt, err := jsrt.New(context.Background(), jsrt.Options{
		Artifact: art,
		Host: jsrt.Host{
			DNSLookup: func(ctx context.Context, op, host, rrtype string) (string, error) {
				calls++
				time.Sleep(10 * time.Millisecond)
				return `{"address":"127.0.0.1","family":4}`, nil
			},
		},
		Permissions: jsrt.Permissions{Net: jsrt.NetPermission{
			Enabled: true, Outbound: true, DNS: true,
		}},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	ctx := context.Background()

	got, err := rt.Call(ctx, "viaCallback", "example.test")
	if err != nil || got != "127.0.0.1/4" {
		t.Fatalf("dns.lookup 回调结果不符: got=%v err=%v", got, err)
	}
	got, err = rt.Call(ctx, "viaPromise", "example.test")
	if err != nil || got != "127.0.0.1/4" {
		t.Fatalf("dns.promises.lookup 结果不符: got=%v err=%v", got, err)
	}
	if calls != 2 {
		t.Fatalf("期望两次宿主解析，实际 %d", calls)
	}
}
