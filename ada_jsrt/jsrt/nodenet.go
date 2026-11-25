package jsrt

// 本文件是 node:http / node:dns / node:net 的宿主适配层。
//
// jsrt 只做参数解码、句柄登记与错误透传;真正的网络 I/O 由宿主实现
// (桥接层的 Host.HTTPDo / Host.DNSLookup / Host.NetDial)。这样执行层不引入
// 网络依赖,也方便测试替换。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// NetConn 是 node:net 的连接抽象。
//
// 读必须尊重 ctx 的 deadline 与取消:同步运行时里一次读就是一次阻塞调用,
// 没有事件循环来兜底,所以超时只能由实现方设置底层 deadline。
type NetConn interface {
	Read(ctx context.Context, p []byte) (int, error)
	Write(ctx context.Context, p []byte) (int, error)
	Close() error
	RemoteAddr() string
	LocalAddr() string
}

// TLSOptions 是 node:tls 传递到宿主的最小配置。
type TLSOptions struct {
	ServerName string
	Insecure   bool
	ALPN       []string
}

// nodeNetRead 是 __adaHost.netRead 的返回值。
type nodeNetRead struct {
	Data string `json:"data"` // base64
	EOF  bool   `json:"eof"`
}

// registerConn 登记一条连接并返回句柄。
func (r *Runtime) registerConn(c NetConn) uint64 {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if r.netPending > 0 {
		r.netPending--
	}
	r.netSeq++
	id := r.netSeq
	r.netConns[id] = c
	return id
}

// reserveConn 在真正拨号前预留一个连接名额；max<=0 表示不限。
func (r *Runtime) reserveConn() error {
	if r.netPolicy == nil {
		return nil
	}
	max := r.netPolicy.perm.MaxConnections
	if max <= 0 {
		return nil
	}
	r.netMu.Lock()
	defer r.netMu.Unlock()
	if len(r.netConns)+r.netPending >= max {
		return deny("net", "connect", "", "MaxConnections", "ERR_ACCESS_DENIED")
	}
	r.netPending++
	return nil
}

// releaseConnReservation 在拨号失败时归还名额。
func (r *Runtime) releaseConnReservation() {
	r.netMu.Lock()
	if r.netPending > 0 {
		r.netPending--
	}
	r.netMu.Unlock()
}

// lookupConn 按句柄取连接。
func (r *Runtime) lookupConn(id uint64) (NetConn, error) {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	c, ok := r.netConns[id]
	if !ok {
		return nil, fmt.Errorf("node:net 连接句柄 %d 不存在或已关闭", id)
	}
	return c, nil
}

// dropConn 移除并关闭一条连接。
func (r *Runtime) dropConn(id uint64) error {
	r.netMu.Lock()
	c, ok := r.netConns[id]
	delete(r.netConns, id)
	r.netMu.Unlock()
	if !ok {
		return nil
	}
	return c.Close()
}

// closeConns 关闭全部连接;运行时不复用时调用。
func (r *Runtime) closeConns() {
	r.netMu.Lock()
	conns := r.netConns
	r.netConns = make(map[uint64]NetConn)
	r.netMu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// nativeHTTPDo 实现 __adaHost.httpDo(spec):spec 与返回值都是 JSON 字符串。
func (r *Runtime) nativeHTTPDo(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.HTTPDo == nil {
			return engine.Undefined(), fmt.Errorf("node:http 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("httpDo(spec) 缺少参数")
		}
		spec, err := toString(raw, args[0])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("httpDo spec 必须是字符串")
		}
		if r.netPolicy != nil {
			target, uerr := httpDoURL(spec)
			if uerr != nil {
				return engine.Undefined(), uerr
			}
			if perr := r.checkNetHTTP(r.currentContext(), target); perr != nil {
				return engine.Undefined(), perr
			}
			rewritten, lerr := r.checkHTTPDoSpec(spec)
			if lerr != nil {
				return engine.Undefined(), lerr
			}
			spec = rewritten
		}
		out, err := r.opts.Host.HTTPDo(r.currentContext(), spec)
		if err != nil {
			return engine.Undefined(), err
		}
		if perr := r.checkHTTPResponse(out); perr != nil {
			return engine.Undefined(), perr
		}
		return raw.FromGo(out)
	}
}

// nativeDNSLookup 实现 __adaHost.dnsLookup(op, host, rrtype)。
func (r *Runtime) nativeDNSLookup(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.DNSLookup == nil {
			return engine.Undefined(), fmt.Errorf("node:dns 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("dnsLookup(op, host, rrtype) 至少需要两个参数")
		}
		op, err := toString(raw, args[0])
		if err != nil || op == "" {
			return engine.Undefined(), fmt.Errorf("dnsLookup op 必须是非空字符串")
		}
		host, err := toString(raw, args[1])
		if err != nil || host == "" {
			return engine.Undefined(), fmt.Errorf("dnsLookup host 必须是非空字符串")
		}
		rrtype := ""
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil {
				rrtype = s
			}
		}
		if r.netPolicy != nil {
			if perr := r.checkNetDNS(r.currentContext(), op, host); perr != nil {
				return engine.Undefined(), perr
			}
		}
		out, err := r.opts.Host.DNSLookup(r.currentContext(), op, host, rrtype)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
	}
}

// nativeDNSLookupAsync 实现 __adaHost.dnsLookupAsync(op, host, rrtype)：返回 Promise。
//
// 解析在 goroutine 里跑，结算回到事件循环；权限检查在发起前完成。
func (r *Runtime) nativeDNSLookupAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.DNSLookup == nil {
			return engine.Undefined(), fmt.Errorf("node:dns 未启用")
		}
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("dnsLookupAsync(op, host, rrtype) 至少需要两个参数")
		}
		op, err := toString(raw, args[0])
		if err != nil || op == "" {
			return engine.Undefined(), fmt.Errorf("dnsLookupAsync op 必须是非空字符串")
		}
		host, err := toString(raw, args[1])
		if err != nil || host == "" {
			return engine.Undefined(), fmt.Errorf("dnsLookupAsync host 必须是非空字符串")
		}
		rrtype := ""
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil {
				rrtype = s
			}
		}
		ctx := r.currentContext()
		if r.netPolicy != nil {
			if perr := r.checkNetDNS(ctx, op, host); perr != nil {
				return engine.Undefined(), perr
			}
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("dns: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			out, derr := r.opts.Host.DNSLookup(ctx, op, host, rrtype)
			if derr != nil {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), derr })
				return
			}
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(out) })
		}()
		return p, nil
	}
}

// nativeNetDial 实现 __adaHost.netDial(address, timeoutMs):返回连接句柄。
func (r *Runtime) nativeNetDial(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.NetDial == nil {
			return engine.Undefined(), fmt.Errorf("node:net 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("netDial(address, timeoutMs) 至少需要 address")
		}
		address, err := toString(raw, args[0])
		if err != nil || address == "" {
			return engine.Undefined(), fmt.Errorf("netDial address 必须是非空字符串")
		}
		if r.netPolicy != nil {
			if perr := r.checkNetDial(r.currentContext(), "tcp", address, "tcp"); perr != nil {
				return engine.Undefined(), perr
			}
		}
		timeout := time.Duration(numberArg(raw, args, 1, 0)) * time.Millisecond
		if rerr := r.reserveConn(); rerr != nil {
			return engine.Undefined(), rerr
		}
		conn, derr := r.opts.Host.NetDial(r.currentContext(), "tcp", address, timeout)
		if derr != nil {
			r.releaseConnReservation()
			return engine.Undefined(), derr
		}
		id := r.registerConn(conn)
		return raw.FromGo(float64(id))
	}
}

// nativeNetDialAsync 实现 __adaHost.net.dialAsync(address, timeoutMs)：返回 Promise<handle>。
func (r *Runtime) nativeNetDialAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.NetDial == nil {
			return engine.Undefined(), fmt.Errorf("node:net 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.dialAsync(address, timeoutMs) 至少需要 address")
		}
		address, err := toString(raw, args[0])
		if err != nil || address == "" {
			return engine.Undefined(), fmt.Errorf("net.dialAsync address 必须是非空字符串")
		}
		timeout := time.Duration(numberArg(raw, args, 1, 0)) * time.Millisecond
		scheme := "tcp"
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil && s != "" {
				scheme = s
			}
		}
		ctx := r.currentContext()
		if r.netPolicy != nil {
			if perr := r.checkNetDial(ctx, "tcp", address, scheme); perr != nil {
				return engine.Undefined(), perr
			}
			if timeout == 0 && r.netPolicy.perm.DialTimeout > 0 {
				timeout = r.netPolicy.perm.DialTimeout
			}
		}
		if rerr := r.reserveConn(); rerr != nil {
			return engine.Undefined(), rerr
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			r.releaseConnReservation()
			return engine.Undefined(), fmt.Errorf("net: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			conn, derr := r.opts.Host.NetDial(ctx, "tcp", address, timeout)
			if derr != nil {
				r.releaseConnReservation()
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), derr })
				return
			}
			id := r.registerConn(conn)
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(float64(id)) })
		}()
		return p, nil
	}
}

// nativeTLSDialAsync 实现 __adaHost.net.tlsDialAsync(address, timeoutMs, cfgJSON)：返回 Promise<handle>。
func (r *Runtime) nativeTLSDialAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.TLSDial == nil {
			return engine.Undefined(), fmt.Errorf("node:tls 未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.tlsDialAsync(address, timeoutMs, cfg) 至少需要 address")
		}
		address, err := toString(raw, args[0])
		if err != nil || address == "" {
			return engine.Undefined(), fmt.Errorf("net.tlsDialAsync address 必须是非空字符串")
		}
		timeout := time.Duration(numberArg(raw, args, 1, 0)) * time.Millisecond
		var cfg struct {
			ServerName string   `json:"serverName"`
			Insecure   bool     `json:"insecure"`
			ALPN       []string `json:"alpn"`
			Scheme     string   `json:"scheme"`
		}
		if len(args) > 2 {
			if s, convErr := toString(raw, args[2]); convErr == nil && s != "" {
				if uerr := json.Unmarshal([]byte(s), &cfg); uerr != nil {
					return engine.Undefined(), fmt.Errorf("net.tlsDialAsync cfg 不是合法 JSON: %w", uerr)
				}
			}
		}
		scheme := cfg.Scheme
		if scheme == "" {
			scheme = "tls"
		}
		ctx := r.currentContext()
		if r.netPolicy != nil {
			if perr := r.checkNetDial(ctx, "tcp", address, scheme); perr != nil {
				return engine.Undefined(), perr
			}
			if timeout == 0 && r.netPolicy.perm.DialTimeout > 0 {
				timeout = r.netPolicy.perm.DialTimeout
			}
		}
		if rerr := r.reserveConn(); rerr != nil {
			return engine.Undefined(), rerr
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			r.releaseConnReservation()
			return engine.Undefined(), fmt.Errorf("tls: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			conn, derr := r.opts.Host.TLSDial(ctx, "tcp", address, TLSOptions{
				ServerName: cfg.ServerName,
				Insecure:   cfg.Insecure,
				ALPN:       cfg.ALPN,
			})
			if derr != nil {
				r.releaseConnReservation()
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), derr })
				return
			}
			id := r.registerConn(conn)
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(float64(id)) })
		}()
		return p, nil
	}
}

// nativeNetReadAsync 实现 __adaHost.net.readAsync(handle, maxBytes)：返回 Promise<JSON>。
//
// 读取在 goroutine 里阻塞，数据/EOF 结算回事件循环；这对常驻循环的
// data 事件泵是必需的，否则一次读会把整个循环卡住。
func (r *Runtime) nativeNetReadAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.readAsync(handle, maxBytes) 至少需要句柄")
		}
		conn, err := r.lookupConn(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		want := int(numberArg(raw, args, 1, 0))
		if want <= 0 {
			want = 64 * 1024
		}
		if want > 1<<20 {
			want = 1 << 20
		}
		ctx := r.currentContext()
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("net: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			buf := make([]byte, want)
			n, rerr := conn.Read(ctx, buf)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), rerr })
				return
			}
			res := nodeNetRead{Data: base64.StdEncoding.EncodeToString(buf[:n]), EOF: errors.Is(rerr, io.EOF)}
			out, merr := json.Marshal(res)
			if merr != nil {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), merr })
				return
			}
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(string(out)) })
		}()
		return p, nil
	}
}

// nativeNetRead 实现 __adaHost.netRead(handle, maxBytes):返回 JSON。
func (r *Runtime) nativeNetRead(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("netRead(handle, maxBytes) 至少需要句柄")
		}
		conn, err := r.lookupConn(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		size := numberArg(raw, args, 1, 0)
		if size <= 0 || size > 1<<20 {
			size = 4096
		}
		buf := make([]byte, int(size))
		n, rerr := conn.Read(r.currentContext(), buf)
		if n > 0 {
			out, merr := json.Marshal(nodeNetRead{Data: encodeBase64(buf[:n])})
			if merr != nil {
				return engine.Undefined(), merr
			}
			return raw.FromGo(string(out))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return raw.FromGo(`{"data":"","eof":true}`)
			}
			// 超时不是错误:Node 的 socket 也会先进入 idle,由脚本决定重试。
			if terr, ok := rerr.(interface{ Timeout() bool }); ok && terr.Timeout() {
				return raw.FromGo(`{"data":"","eof":false}`)
			}
			return engine.Undefined(), fmt.Errorf("node:net 读取失败: %w", rerr)
		}
		return raw.FromGo(`{"data":"","eof":false}`)
	}
}

// nativeNetWrite 实现 __adaHost.netWrite(handle, base64)。
func (r *Runtime) nativeNetWrite(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 2 {
			return engine.Undefined(), fmt.Errorf("netWrite(handle, data) 需要两个参数")
		}
		conn, err := r.lookupConn(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		text, err := toString(raw, args[1])
		if err != nil {
			return engine.Undefined(), fmt.Errorf("netWrite data 必须是字符串")
		}
		data, derr := decodeBase64(text)
		if derr != nil {
			return engine.Undefined(), fmt.Errorf("netWrite data 不是合法 base64: %w", derr)
		}
		if _, werr := conn.Write(r.currentContext(), data); werr != nil {
			return engine.Undefined(), fmt.Errorf("node:net 写入失败: %w", werr)
		}
		return engine.Undefined(), nil
	}
}

// nativeNetClose 实现 __adaHost.netClose(handle)。
func (r *Runtime) nativeNetClose(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("netClose(handle) 需要句柄")
		}
		if err := r.dropConn(uint64(numberArg(raw, args, 0, 0))); err != nil {
			return engine.Undefined(), fmt.Errorf("node:net 关闭失败: %w", err)
		}
		return engine.Undefined(), nil
	}
}

// nativeNetAddr 实现 __adaHost.netAddr(handle),返回 JSON 地址信息。
func (r *Runtime) nativeNetAddr(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("netAddr(handle) 需要句柄")
		}
		conn, err := r.lookupConn(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		payload, merr := json.Marshal(map[string]string{
			"remoteAddress": conn.RemoteAddr(),
			"localAddress":  conn.LocalAddr(),
		})
		if merr != nil {
			return engine.Undefined(), merr
		}
		return raw.FromGo(string(payload))
	}
}

// numberArg 读取第 i 个数字参数;缺失或非数字时返回 fallback。
func numberArg(raw *moejs.Runtime, args []engine.Value, i int, fallback float64) float64 {
	if i >= len(args) {
		return fallback
	}
	v, err := raw.ToGo(args[i])
	if err != nil {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return fallback
}

// encodeBase64 把二进制编码成 base64 字符串,用于跨 JSON 边界的 socket 数据。
func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// decodeBase64 解码 base64 字符串。
func decodeBase64(text string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(text)
}

// NetListener 是 node:net 服务端监听器的宿主抽象。
type NetListener interface {
	Accept(ctx context.Context) (NetConn, error)
	Close() error
	Addr() string
}

// registerListener 登记一个监听器。
func (r *Runtime) registerListener(ln NetListener) uint64 {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.listenerSeq++
	id := r.listenerSeq
	r.netListeners[id] = ln
	return id
}

func (r *Runtime) lookupListener(id uint64) (NetListener, error) {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	ln, ok := r.netListeners[id]
	if !ok {
		return nil, fmt.Errorf("net: 监听器 %d 不存在", id)
	}
	return ln, nil
}

func (r *Runtime) removeListener(id uint64) NetListener {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	ln := r.netListeners[id]
	delete(r.netListeners, id)
	return ln
}

// closeListeners 关闭全部监听器；Close/shutdown 使用。
func (r *Runtime) closeListeners() {
	r.netMu.Lock()
	list := make([]NetListener, 0, len(r.netListeners))
	for id, ln := range r.netListeners {
		list = append(list, ln)
		delete(r.netListeners, id)
	}
	r.netMu.Unlock()
	for _, ln := range list {
		_ = ln.Close()
	}
}

// nativeNetListenAsync 实现 __adaHost.net.listenAsync(address)：返回 Promise<listenerHandle>。
func (r *Runtime) nativeNetListenAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.NetListen == nil {
			return engine.Undefined(), fmt.Errorf("node:net 监听未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.listenAsync(address) 需要 address")
		}
		address, err := toString(raw, args[0])
		if err != nil || address == "" {
			return engine.Undefined(), fmt.Errorf("net.listenAsync address 必须是非空字符串")
		}
		ctx := r.currentContext()
		if r.netPolicy != nil {
			if perr := r.checkNetListen(ctx, address, "tcp"); perr != nil {
				return engine.Undefined(), perr
			}
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("net: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			ln, derr := r.opts.Host.NetListen(ctx, "tcp", address)
			if derr != nil {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), derr })
				return
			}
			id := r.registerListener(ln)
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(float64(id)) })
		}()
		return p, nil
	}
}

// nativeTLSListenAsync 实现 __adaHost.net.tlsListenAsync(address, cfgJSON)：返回 Promise<handle>。
func (r *Runtime) nativeTLSListenAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if r.opts.Host.TLSListen == nil {
			return engine.Undefined(), fmt.Errorf("node:tls 监听未启用")
		}
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.tlsListenAsync(address, cfg) 需要 address")
		}
		address, err := toString(raw, args[0])
		if err != nil || address == "" {
			return engine.Undefined(), fmt.Errorf("net.tlsListenAsync address 必须是非空字符串")
		}
		var cfg struct {
			ServerName string   `json:"serverName"`
			Insecure   bool     `json:"insecure"`
			ALPN       []string `json:"alpn"`
		}
		if len(args) > 1 {
			if s, convErr := toString(raw, args[1]); convErr == nil && s != "" {
				if uerr := json.Unmarshal([]byte(s), &cfg); uerr != nil {
					return engine.Undefined(), fmt.Errorf("net.tlsListenAsync cfg 不是合法 JSON: %w", uerr)
				}
			}
		}
		ctx := r.currentContext()
		if r.netPolicy != nil {
			if perr := r.checkNetListen(ctx, address, "tls"); perr != nil {
				return engine.Undefined(), perr
			}
		}
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("tls: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			ln, derr := r.opts.Host.TLSListen(ctx, "tcp", address, TLSOptions{
				ServerName: cfg.ServerName,
				Insecure:   cfg.Insecure,
				ALPN:       cfg.ALPN,
			})
			if derr != nil {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), derr })
				return
			}
			id := r.registerListener(ln)
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(float64(id)) })
		}()
		return p, nil
	}
}

// nativeNetAcceptAsync 实现 __adaHost.net.acceptAsync(listenerHandle)：返回 Promise<JSON>。
func (r *Runtime) nativeNetAcceptAsync(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.acceptAsync(handle) 需要监听器句柄")
		}
		ln, err := r.lookupListener(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		ctx := r.currentContext()
		r.mu.Lock()
		loop := r.loop
		r.mu.Unlock()
		if loop == nil {
			return engine.Undefined(), fmt.Errorf("net: 事件循环不可用")
		}
		p, settle := loop.NewPromise()
		go func() {
			conn, aerr := ln.Accept(ctx)
			if aerr != nil {
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), aerr })
				return
			}
			if qerr := r.reserveConn(); qerr != nil {
				_ = conn.Close()
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), qerr })
				return
			}
			id := r.registerConn(conn)
			info := map[string]any{
				"id":            float64(id),
				"remoteAddress": conn.RemoteAddr(),
				"localAddress":  conn.LocalAddr(),
			}
			out, merr := json.Marshal(info)
			if merr != nil {
				_ = conn.Close()
				settle(func(*moejs.Runtime) (engine.Value, error) { return engine.Undefined(), merr })
				return
			}
			settle(func(rt *moejs.Runtime) (engine.Value, error) { return rt.FromGo(string(out)) })
		}()
		return p, nil
	}
}

// nativeNetListenerClose 实现 __adaHost.net.closeListener(handle)。
func (r *Runtime) nativeNetListenerClose(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.closeListener(handle) 需要监听器句柄")
		}
		if ln := r.removeListener(uint64(numberArg(raw, args, 0, 0))); ln != nil {
			_ = ln.Close()
		}
		return engine.Undefined(), nil
	}
}

// nativeNetListenerAddr 实现 __adaHost.net.listenerAddr(handle)：返回 JSON。
func (r *Runtime) nativeNetListenerAddr(raw *moejs.Runtime) moejs.NativeFunc {
	return func(_ *engine.Realm, _ engine.Value, args []engine.Value) (engine.Value, error) {
		if len(args) < 1 {
			return engine.Undefined(), fmt.Errorf("net.listenerAddr(handle) 需要监听器句柄")
		}
		ln, err := r.lookupListener(uint64(numberArg(raw, args, 0, 0)))
		if err != nil {
			return engine.Undefined(), err
		}
		out, _ := json.Marshal(map[string]any{"address": ln.Addr()})
		return raw.FromGo(string(out))
	}
}
