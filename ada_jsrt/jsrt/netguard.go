// 网络权限守卫：在调用任何网络 provider（内置或 Host 注入）之前执行。
//
// 现有 Host.NetDial / HTTPDo / DNSLookup 是同步的；这里先把 Go 侧强制
// 权限接上（host/port、scheme、解析后的 IP），异步 socket/TLS 在后续阶段替换。
package jsrt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// checkNetDial 校验一次出站连接；会解析域名并逐个检查 IP（DNS rebinding 防护）。
func (r *Runtime) checkNetDial(ctx context.Context, network, address, scheme string) error {
	if r.netPolicy == nil {
		return nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return deny("net", "connect", address, "地址缺少 host:port", "ERR_ACCESS_DENIED")
	}
	if strings.TrimSpace(host) == "" {
		return deny("net", "connect", address, "空主机", "ERR_ACCESS_DENIED")
	}
	portNum, ok := parsePortNumber(port)
	if !ok {
		return deny("net", "connect", address, "非法端口", "ERR_ACCESS_DENIED")
	}
	if err := r.netPolicy.checkDial(host, port, scheme); err != nil {
		r.emitPermission("net", "connect", address, err)
		return err
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := r.netPolicy.checkIP(ip, portNum, scheme); err != nil {
			r.emitPermission("net", "connect", address, err)
			return err
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("jsrt: 解析 %q 失败: %w", host, err)
	}
	for _, ip := range ips {
		if err := r.netPolicy.checkIP(ip.IP, portNum, scheme); err != nil {
			r.emitPermission("net", "connect", address, err)
			return err
		}
	}
	return nil
}

// checkNetHTTP 校验一次 HTTP(S) 请求的 URL。
func (r *Runtime) checkNetHTTP(ctx context.Context, raw string) error {
	if r.netPolicy == nil {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return deny("net", "connect", raw, "URL 非法", "ERR_ACCESS_DENIED")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return deny("net", "connect", raw, "只允许 http/https", "ERR_ACCESS_DENIED")
	}
	host := u.Hostname()
	if host == "" {
		return deny("net", "connect", raw, "URL 缺少主机", "ERR_ACCESS_DENIED")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(host, port)
	return r.checkNetDial(ctx, "tcp", address, u.Scheme)
}

// checkNetDNS 校验一次 DNS 解析。reverse 的目标是 IP，按 IP 规则检查。
func (r *Runtime) checkNetDNS(ctx context.Context, op, host string) error {
	if r.netPolicy == nil {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if op == "reverse" {
			if err := r.netPolicy.checkIP(ip, -1, ""); err != nil {
				r.emitPermission("net", "lookup", host, err)
				return err
			}
		}
	}
	if err := r.netPolicy.checkDNS(host); err != nil {
		r.emitPermission("net", "lookup", host, err)
		return err
	}
	return nil
}

// emitPermission 上报一次权限拒绝。
func (r *Runtime) emitPermission(scope, op, resource string, err error) {
	if err == nil {
		return
	}
	r.emitEvent(Event{
		Kind: "permission", Err: err, Scope: scope, Op: op,
		Resource: resource, Allowed: false,
	})
}

// httpDoURL 从 __adaHost.httpDo 的 JSON 入参里取 URL。
func httpDoURL(spec string) (string, error) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(spec), &req); err != nil {
		return "", fmt.Errorf("jsrt: httpDo 参数不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(req.URL) == "" {
		return "", fmt.Errorf("jsrt: httpDo 缺少 url")
	}
	return req.URL, nil
}

// checkHTTPDoSpec 在调用 Host.HTTPDo 前检查请求体上限，并把 MaxResponseBytes
// 写进 spec 的 maxBody（宿主实现会按它截断响应）。
func (r *Runtime) checkHTTPDoSpec(spec string) (string, error) {
	if r.netPolicy == nil {
		return spec, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(spec), &obj); err != nil {
		return spec, nil // 交给宿主报参数错误
	}
	if max := r.netPolicy.perm.MaxRequestBytes; max > 0 {
		if body, ok := obj["body"].(string); ok && body != "" {
			raw, err := base64.StdEncoding.DecodeString(body)
			if err == nil && int64(len(raw)) > max {
				err := deny("net", "request", "body", "MaxRequestBytes", "ERR_ACCESS_DENIED")
				r.emitPermission("net", "request", "body", err)
				return "", err
			}
		}
	}
	if max := r.netPolicy.perm.MaxResponseBytes; max > 0 {
		existing := int64(0)
		if v, ok := obj["maxBody"].(float64); ok {
			existing = int64(v)
		}
		if existing <= 0 || existing > max {
			obj["maxBody"] = max
			out, err := json.Marshal(obj)
			if err != nil {
				return spec, nil
			}
			return string(out), nil
		}
	}
	return spec, nil
}

// checkHTTPResponse 在 Host 返回后复查响应体大小；maxBody 已经截断时这只是兜底。
func (r *Runtime) checkHTTPResponse(out string) error {
	if r.netPolicy == nil {
		return nil
	}
	max := r.netPolicy.perm.MaxResponseBytes
	if max <= 0 {
		return nil
	}
	var resp struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		return nil
	}
	if int64(len(raw)) > max {
		perr := deny("net", "response", "body", "MaxResponseBytes", "ERR_ACCESS_DENIED")
		r.emitPermission("net", "response", "body", perr)
		return perr
	}
	return nil
}

// checkNetListen 校验一次入站监听。
func (r *Runtime) checkNetListen(ctx context.Context, address, scheme string) error {
	if r.netPolicy == nil {
		return nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return deny("net", "listen", address, "地址缺少 host:port", "ERR_ACCESS_DENIED")
	}
	if len(r.netPolicy.perm.Protocols) > 0 {
		found := false
		for _, proto := range r.netPolicy.perm.Protocols {
			if strings.EqualFold(proto, scheme) {
				found = true
				break
			}
		}
		if !found {
			return deny("net", "listen", address, "协议不在 Protocols 内", "ERR_ACCESS_DENIED")
		}
	}
	if err := r.netPolicy.checkListen(host, port); err != nil {
		r.emitPermission("net", "listen", address, err)
		return err
	}
	return nil
}
