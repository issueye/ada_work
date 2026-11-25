package jsrt

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNetGuardDial(t *testing.T) {
	r := &Runtime{netConns: make(map[uint64]NetConn), netPolicy: mustNetPolicy(t, NetPermission{
		Enabled:         true,
		Outbound:        true,
		AllowPrivateIPs: false,
		Allow:           []string{"tcp://127.0.0.1:9", "tcp://10.0.0.0/8:9"},
	})}
	ctx := context.Background()
	if err := r.checkNetDial(ctx, "tcp", "127.0.0.1:9", "tcp"); err != nil {
		t.Fatalf("精确 IP 允许的私网连接被拒: %v", err)
	}
	if err := r.checkNetDial(ctx, "tcp", "10.1.2.3:9", "tcp"); err != nil {
		t.Fatalf("CIDR 允许的私网连接被拒: %v", err)
	}
	assertCode(t, r.checkNetDial(ctx, "tcp", "127.0.0.1:10", "tcp"), "ERR_ACCESS_DENIED")
	assertCode(t, r.checkNetDial(ctx, "tcp", "8.8.8.8:9", "tcp"), "ERR_ACCESS_DENIED")
}

func TestNetGuardHTTP(t *testing.T) {
	r := &Runtime{netConns: make(map[uint64]NetConn), netPolicy: mustNetPolicy(t, NetPermission{
		Enabled:         true,
		Outbound:        true,
		AllowPrivateIPs: true,
		Allow:           []string{"http://127.0.0.1:9", "https://api.example.com:443"},
	})}
	ctx := context.Background()
	if err := r.checkNetHTTP(ctx, "http://127.0.0.1:9/x"); err != nil {
		t.Fatalf("允许的 HTTP 被拒: %v", err)
	}
	assertCode(t, r.checkNetHTTP(ctx, "http://127.0.0.1:10/x"), "ERR_ACCESS_DENIED")
	assertCode(t, r.checkNetHTTP(ctx, "ftp://127.0.0.1:9/x"), "ERR_ACCESS_DENIED")
	// 不允许的域名会先被 host 规则拒绝，不会走真实 DNS。
	assertCode(t, r.checkNetHTTP(ctx, "https://other.example.com/x"), "ERR_ACCESS_DENIED")
}

func TestNetGuardDNS(t *testing.T) {
	r := &Runtime{netConns: make(map[uint64]NetConn), netPolicy: mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true, DNS: true,
		Deny: []string{"blocked.example.com"},
	})}
	ctx := context.Background()
	if err := r.checkNetDNS(ctx, "lookup", "example.com"); err != nil {
		t.Fatalf("DNS 允许被拒: %v", err)
	}
	assertCode(t, r.checkNetDNS(ctx, "lookup", "blocked.example.com"), "ERR_ACCESS_DENIED")

	noDNS := &Runtime{netPolicy: mustNetPolicy(t, NetPermission{Enabled: true, Outbound: true})}
	assertCode(t, noDNS.checkNetDNS(ctx, "lookup", "example.com"), "ERR_ACCESS_DENIED")
}

func TestNetGuardPermissionEvent(t *testing.T) {
	var events []Event
	r := &Runtime{
		netPolicy: mustNetPolicy(t, NetPermission{
			Enabled: true, Outbound: true,
			Allow: []string{"tcp://127.0.0.1:9"},
		}),
		onEvent: func(ev Event) { events = append(events, ev) },
	}
	err := r.checkNetDial(context.Background(), "tcp", "127.0.0.1:10", "tcp")
	if err == nil {
		t.Fatal("应拒绝")
	}
	if len(events) != 1 || events[0].Kind != "permission" || events[0].Scope != "net" {
		t.Fatalf("权限事件不符: %+v", events)
	}
	var pe *PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("期望 PermissionError，实际 %v", err)
	}
}

func TestMaxConnectionsQuota(t *testing.T) {
	r := &Runtime{netConns: make(map[uint64]NetConn), netPolicy: mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true, MaxConnections: 1,
		Allow: []string{"tcp://127.0.0.1:9"},
	})}
	if err := r.reserveConn(); err != nil {
		t.Fatalf("第一个连接名额应成功: %v", err)
	}
	assertCode(t, r.reserveConn(), "ERR_ACCESS_DENIED")
	r.releaseConnReservation()
	if err := r.reserveConn(); err != nil {
		t.Fatalf("释放后应能再次预留: %v", err)
	}
	_ = r.registerConn(nil)
	if got := len(r.netConns); got != 1 {
		t.Fatalf("登记连接数不符: %d", got)
	}
	if r.netPending != 0 {
		t.Fatalf("登记后 pending 应归零: %d", r.netPending)
	}
}

func TestHTTPDoByteLimits(t *testing.T) {
	r := &Runtime{netConns: make(map[uint64]NetConn), netPolicy: mustNetPolicy(t, NetPermission{
		Enabled: true, Outbound: true, AllowPrivateIPs: true,
		Allow:            []string{"http://127.0.0.1:9"},
		MaxRequestBytes:  4,
		MaxResponseBytes: 8,
	})}
	big := base64.StdEncoding.EncodeToString([]byte("hello"))
	spec := fmt.Sprintf(`{"url":"http://127.0.0.1:9/","body":%q}`, big)
	if _, err := r.checkHTTPDoSpec(spec); err == nil {
		t.Fatal("超过 MaxRequestBytes 应被拒绝")
	}
	small := fmt.Sprintf(`{"url":"http://127.0.0.1:9/","body":%q}`, base64.StdEncoding.EncodeToString([]byte("hi")))
	out, err := r.checkHTTPDoSpec(small)
	if err != nil {
		t.Fatalf("小请求体应允许: %v", err)
	}
	if !strings.Contains(out, `"maxBody":8`) {
		t.Fatalf("应把 MaxResponseBytes 写入 maxBody: %s", out)
	}
	resp := fmt.Sprintf(`{"body":%q}`, base64.StdEncoding.EncodeToString([]byte("123456789")))
	assertCode(t, r.checkHTTPResponse(resp), "ERR_ACCESS_DENIED")
}
