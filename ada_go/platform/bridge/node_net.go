package bridge

// 本文件是 Node 网络模块(node:http / node:https / node:dns / node:net)的宿主实现。
//
// 安全边界见 ADR 0011/0012:这些模块直连标准库,**不经过**任何受控 seam 或 caps。
// 决策依据是产品约束只针对「文件与写入」:网络请求不改变本地文件,
// 不需要第二道审批。保留的硬限制有两类:
//   - 只支持 http/https,不做任意协议透传;
//   - 请求体与响应体都受 JSON 边界上限约束(见 maxNodeHTTPBody)。
//
// 所有入口都接收调用级 ctx:工具超时、取消与插件卸载都会中断在途请求。

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/issueye/ada_jsrt/jsrt"
)

const (
	// maxNodeHTTPBody 是单次请求/响应的字节上限。跨 JSON 边界传 base64,
	// 4 MiB 原文约 5.4 MiB 文本,仍在内存可接受范围内。
	maxNodeHTTPBody = 4 << 20
	// defaultHTTPTimeout 是没有显式 timeout 时的请求上限。
	defaultHTTPTimeout = 30 * time.Second
	// defaultNetTimeout 是 TCP 读写的默认等待上限。
	defaultNetTimeout = 30 * time.Second
)

// nodeHTTPRequest 是 __adaHost.httpDo 的入参。
type nodeHTTPRequest struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"` // base64
	TimeoutMs int64             `json:"timeoutMs"`
	Insecure  bool              `json:"insecure"`
	MaxBody   int64             `json:"maxBody"`
}

// nodeHTTPResponse 是 __adaHost.httpDo 的返回值。
type nodeHTTPResponse struct {
	URL         string            `json:"url"`
	Status      int               `json:"status"`
	StatusText  string            `json:"statusText"`
	HTTPVersion string            `json:"httpVersion"`
	Headers     map[string]string `json:"headers"`
	RawHeaders  []string          `json:"rawHeaders"`
	Body        string            `json:"body"` // base64
	Truncated   bool              `json:"truncated"`
	DurationMs  int64             `json:"durationMs"`
}

var (
	nodeHTTPOnce      sync.Once
	nodeHTTPClientV   *http.Client
	nodeHTTPSInsecure *http.Client
)

// nodeHTTPClient 返回复用连接池的客户端。Node 的 http 不跟随重定向,
// 这里保持一致:3xx 原样返回,由脚本自己决定下一步。
func nodeHTTPClient(insecure bool) *http.Client {
	nodeHTTPOnce.Do(func() {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			base = &http.Transport{}
		}
		secureTransport := base.Clone()
		insecureTransport := base.Clone()
		insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 只在脚本显式 rejectUnauthorized:false 时使用
		newClient := func(tr *http.Transport) *http.Client {
			return &http.Client{
				Transport: tr,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
		}
		nodeHTTPClientV = newClient(secureTransport)
		nodeHTTPSInsecure = newClient(insecureTransport)
	})
	if insecure {
		return nodeHTTPSInsecure
	}
	return nodeHTTPClientV
}

// nodeHTTPDo 执行一次 HTTP(S) 请求,入参与返回值都是 JSON 字符串。
func nodeHTTPDo(ctx context.Context, spec string) (string, error) {
	var req nodeHTTPRequest
	if err := json.Unmarshal([]byte(spec), &req); err != nil {
		return "", fmt.Errorf("node:http 请求参数非法: %w", err)
	}
	raw := strings.TrimSpace(req.URL)
	if raw == "" {
		return "", fmt.Errorf("node:http 请求缺少 url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("node:http 请求地址非法 %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("node:http 只支持 http/https,得到 %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("node:http 请求地址缺少主机: %q", raw)
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if req.Body != "" {
		decoded, derr := base64.StdEncoding.DecodeString(req.Body)
		if derr != nil {
			return "", fmt.Errorf("node:http 请求体不是合法 base64: %w", derr)
		}
		body = bytes.NewReader(decoded)
	}
	timeout := defaultHTTPTimeout
	if req.TimeoutMs > 0 {
		timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(callCtx, method, u.String(), body)
	if err != nil {
		return "", fmt.Errorf("node:http 构造请求失败: %w", err)
	}
	for k, v := range req.Headers {
		if k == "" {
			continue
		}
		hreq.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := nodeHTTPClient(req.Insecure).Do(hreq)
	if err != nil {
		return "", fmt.Errorf("node:http 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limit := req.MaxBody
	if limit <= 0 || limit > maxNodeHTTPBody {
		limit = maxNodeHTTPBody
	}
	buf, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if rerr != nil {
		return "", fmt.Errorf("node:http 读取响应失败: %w", rerr)
	}
	truncated := int64(len(buf)) > limit
	if truncated {
		buf = buf[:limit]
	}

	headers := make(map[string]string, len(resp.Header))
	keys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rawHeaders := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		joined := strings.Join(resp.Header.Values(k), ", ")
		headers[strings.ToLower(k)] = joined
		rawHeaders = append(rawHeaders, k, joined)
	}
	version := resp.Proto
	if version == "" {
		version = "HTTP/1.1"
	}
	out, err := json.Marshal(nodeHTTPResponse{
		URL:         u.String(),
		Status:      resp.StatusCode,
		StatusText:  http.StatusText(resp.StatusCode),
		HTTPVersion: version,
		Headers:     headers,
		RawHeaders:  rawHeaders,
		Body:        base64.StdEncoding.EncodeToString(buf),
		Truncated:   truncated,
		DurationMs:  time.Since(start).Milliseconds(),
	})
	if err != nil {
		return "", fmt.Errorf("node:http 序列化响应失败: %w", err)
	}
	return string(out), nil
}

// nodeDNSLookup 执行一次域名解析。op 取值:
//
//	lookup / lookupAll / resolve4 / resolve6 / resolveCname / resolveNs
//	/ resolveMx / resolveTxt / resolveSrv / reverse
//
// 返回值统一是 JSON:lookup 系返回 {address,family} 或 [{address,family}],
// resolve 系返回 Node 形状的数组。空结果返回 "[]" 而不是 nil,避免 JS 侧多一个分支。
func nodeDNSLookup(ctx context.Context, op, host, rrtype string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("node:dns 缺少域名")
	}
	resolver := net.DefaultResolver
	marshal := func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("node:dns 序列化失败: %w", err)
		}
		return string(b), nil
	}
	family := func(addr net.IP) int {
		if addr.To4() != nil {
			return 4
		}
		return 6
	}
	switch op {
	case "lookup", "lookupAll":
		ips, err := resolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 %s 失败: %w", host, err)
		}
		want := 0
		if op == "lookup" || rrtype == "4" || rrtype == "6" {
			if rrtype == "4" {
				want = 4
			} else if rrtype == "6" {
				want = 6
			}
		}
		type entry struct {
			Address string `json:"address"`
			Family  int    `json:"family"`
		}
		list := make([]entry, 0, len(ips))
		for _, ip := range ips {
			f := family(ip)
			if want != 0 && f != want {
				continue
			}
			list = append(list, entry{Address: ip.String(), Family: f})
		}
		if len(list) == 0 {
			return "", fmt.Errorf("node:dns 解析 %s 没有地址记录", host)
		}
		if op == "lookup" {
			return marshal(list[0])
		}
		return marshal(list)
	case "resolve4", "resolve6":
		network := "ip4"
		if op == "resolve6" {
			network = "ip6"
		}
		ips, err := resolver.LookupIP(ctx, network, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 %s 失败: %w", host, err)
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, ip.String())
		}
		return marshal(out)
	case "resolveCname":
		name, err := resolver.LookupCNAME(ctx, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 CNAME 失败: %w", err)
		}
		return marshal([]string{strings.TrimSuffix(name, ".")})
	case "resolveNs":
		records, err := resolver.LookupNS(ctx, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 NS 失败: %w", err)
		}
		out := make([]string, 0, len(records))
		for _, r := range records {
			out = append(out, strings.TrimSuffix(r.Host, "."))
		}
		return marshal(out)
	case "resolveMx":
		records, err := resolver.LookupMX(ctx, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 MX 失败: %w", err)
		}
		type mx struct {
			Exchange string `json:"exchange"`
			Priority uint16 `json:"priority"`
		}
		out := make([]mx, 0, len(records))
		for _, r := range records {
			out = append(out, mx{Exchange: strings.TrimSuffix(r.Host, "."), Priority: r.Pref})
		}
		return marshal(out)
	case "resolveTxt":
		records, err := resolver.LookupTXT(ctx, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 TXT 失败: %w", err)
		}
		// Node 的 resolveTxt 返回 string[][];Go 侧一条记录就是一个字符串。
		out := make([][]string, 0, len(records))
		for _, r := range records {
			out = append(out, []string{r})
		}
		return marshal(out)
	case "resolveSrv":
		_, records, err := resolver.LookupSRV(ctx, "", "", host)
		if err != nil {
			return "", fmt.Errorf("node:dns 解析 SRV 失败: %w", err)
		}
		type srv struct {
			Name     string `json:"name"`
			Port     uint16 `json:"port"`
			Priority uint16 `json:"priority"`
			Weight   uint16 `json:"weight"`
		}
		out := make([]srv, 0, len(records))
		for _, r := range records {
			out = append(out, srv{
				Name: strings.TrimSuffix(r.Target, "."),
				Port: r.Port, Priority: r.Priority, Weight: r.Weight,
			})
		}
		return marshal(out)
	case "reverse":
		names, err := resolver.LookupAddr(ctx, host)
		if err != nil {
			return "", fmt.Errorf("node:dns 反向解析 %s 失败: %w", host, err)
		}
		out := make([]string, 0, len(names))
		for _, n := range names {
			out = append(out, strings.TrimSuffix(n, "."))
		}
		return marshal(out)
	default:
		return "", fmt.Errorf("node:dns 不支持的解析类型 %q", op)
	}
}

// nodeNetConn 是 node:net 的一条 TCP 连接。
//
// 句柄由 jsrt 分配与持有;插件卸载时 jsrt 会关闭全部连接,
// 所以桥接层不另建连接表,避免两套生命周期。
type nodeNetConn struct {
	conn net.Conn
}

// Read 读一次;没有 deadline 时用默认上限,避免脚本写出永久阻塞的调用。
func (c *nodeNetConn) Read(ctx context.Context, p []byte) (int, error) {
	deadline := time.Now().Add(defaultNetTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	_ = c.conn.SetReadDeadline(deadline)
	return c.conn.Read(p)
}

// Write 写一次,超时语义与 Read 一致。
func (c *nodeNetConn) Write(ctx context.Context, p []byte) (int, error) {
	deadline := time.Now().Add(defaultNetTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	_ = c.conn.SetWriteDeadline(deadline)
	return c.conn.Write(p)
}

// Close 关闭连接。
func (c *nodeNetConn) Close() error { return c.conn.Close() }

// RemoteAddr 返回对端地址。
func (c *nodeNetConn) RemoteAddr() string { return c.conn.RemoteAddr().String() }

// LocalAddr 返回本端地址。
func (c *nodeNetConn) LocalAddr() string { return c.conn.LocalAddr().String() }

// nodeDial 建立一条 TCP 连接。
func nodeDial(ctx context.Context, network, address string, timeout time.Duration) (jsrt.NetConn, error) {
	if timeout <= 0 {
		timeout = defaultNetTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("node:net 连接 %s 失败: %w", address, err)
	}
	return &nodeNetConn{conn: conn}, nil
}

// 编译期确认实现了 jsrt 的连接契约。
var _ jsrt.NetConn = (*nodeNetConn)(nil)

// nodeTLSDial 是 jsrt.Host.TLSDial 的宿主实现：标准库 TLS 客户端。
// 证书校验默认开启；只有脚本显式 rejectUnauthorized:false 才跳过。
func nodeTLSDial(ctx context.Context, network, address string, opts jsrt.TLSOptions) (jsrt.NetConn, error) {
	cfg := &tls.Config{
		ServerName:         opts.ServerName,
		InsecureSkipVerify: opts.Insecure, //nolint:gosec // 只在脚本显式 rejectUnauthorized:false 时使用
		NextProtos:         opts.ALPN,
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(address); err == nil {
			cfg.ServerName = host
		}
	}
	dialer := &net.Dialer{Timeout: defaultNetTimeout}
	conn, err := tls.DialWithDialer(dialer, network, address, cfg)
	if err != nil {
		return nil, fmt.Errorf("node:tls 连接 %s 失败: %w", address, err)
	}
	return &nodeNetConn{conn: conn}, nil
}
