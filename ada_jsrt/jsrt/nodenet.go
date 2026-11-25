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

// NetConn 是 node:net 的同步连接抽象。
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

// nodeNetRead 是 __adaHost.netRead 的返回值。
type nodeNetRead struct {
	Data string `json:"data"` // base64
	EOF  bool   `json:"eof"`
}

// registerConn 登记一条连接并返回句柄。
func (r *Runtime) registerConn(c NetConn) uint64 {
	r.netMu.Lock()
	defer r.netMu.Unlock()
	r.netSeq++
	id := r.netSeq
	r.netConns[id] = c
	return id
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
		out, err := r.opts.Host.HTTPDo(r.currentContext(), spec)
		if err != nil {
			return engine.Undefined(), err
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
		out, err := r.opts.Host.DNSLookup(r.currentContext(), op, host, rrtype)
		if err != nil {
			return engine.Undefined(), err
		}
		return raw.FromGo(out)
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
		timeout := time.Duration(numberArg(raw, args, 1, 0)) * time.Millisecond
		conn, derr := r.opts.Host.NetDial(r.currentContext(), "tcp", address, timeout)
		if derr != nil {
			return engine.Undefined(), derr
		}
		id := r.registerConn(conn)
		return raw.FromGo(float64(id))
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
