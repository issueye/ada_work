// 权限模型：文件、网络、子进程、环境变量的静态白名单。
//
// 设计见 docs/node-runtime-design.md §8。这里只做类型、校验与匹配；
// 真正的强制点在原生能力入口（moejs 的 FSOptions.Allow、fsx 的每个操作、
// 后续的 Net/Child Guard）。JS 侧不得作为唯一检查点。
package jsrt

import (
	"errors"
	"fmt"
	"net"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LoopMode 是事件循环模式。
type LoopMode uint8

const (
	// LoopScoped 保持现有插件语义：一次宿主调用跑一次事件循环。
	LoopScoped LoopMode = iota
	// LoopResident 是 Node 风格：运行时启动后循环常驻。
	LoopResident
)

// ExitPolicy 控制 process.exit 的行为。
type ExitPolicy uint8

const (
	// ExitDeny 让 process.exit 抛 ERR_ACCESS_DENIED，不结束宿主。
	ExitDeny ExitPolicy = iota
	// ExitRequest 记录退出码并走优雅关闭；不杀宿主进程。
	ExitRequest
)

// Permissions 是运行时的能力白名单；零值表示全部拒绝。
type Permissions struct {
	FS    FSPermission
	Net   NetPermission
	Child ChildPermission
	Env   EnvPermission
}

// FSAccess 是文件访问级别。
type FSAccess uint8

const (
	// FSReadOnly 只允许读；写操作返回 EROFS。
	FSReadOnly FSAccess = iota
	// FSReadWrite 允许读与写（写仍受 Allow/WriteAllow/Deny 约束）。
	FSReadWrite
)

// FSPermission 配置文件能力。
//
// Root 是宿主真实目录，脚本看到的是 Mount（默认 /workspace）。
// Allow 为空表示 Root 下全部可读/可写；WriteAllow 为空表示 Access=FSReadWrite
// 时 Root 下全部可写；Deny 优先于两者。
type FSPermission struct {
	Enabled bool
	Root    string
	Mount   string
	Cwd     string
	Access  FSAccess

	Allow      []string
	WriteAllow []string
	Deny       []string

	Watch bool

	MaxFileBytes  int64
	MaxOpenFiles  int
	MaxDirEntries int
}

// NetPermission 配置网络能力。
type NetPermission struct {
	Enabled  bool
	Outbound bool
	Inbound  bool

	// Allow 是出站目标白名单；空 = 拒绝全部；"*" = 全部。
	// 语法：host、*.example.com、scheme://host:port、CIDR[:port]。
	Allow []string
	Deny  []string
	// Listen 是入站绑定白名单；空 = 不允许监听。
	Listen []string

	DNS             bool
	AllowPrivateIPs bool
	Protocols       []string

	MaxConnections   int
	MaxRequestBytes  int64
	MaxResponseBytes int64
	DialTimeout      time.Duration
	IdleTimeout      time.Duration
}

// ChildPermission 配置子进程能力。
type ChildPermission struct {
	Enabled        bool
	Allow          []string
	PassEnv        []string
	Shell          bool
	MaxOutputBytes int64
	Timeout        time.Duration
}

// EnvPermission 配置环境变量能力。
type EnvPermission struct {
	Allow []string
	Set   bool
}

// Limits 是运行时资源限制。
type Limits struct {
	MemoryBytes    int64
	StartupTimeout time.Duration
	CallTimeout    time.Duration
	CloseTimeout   time.Duration

	MaxTimers    int
	MaxListeners int
	MaxSockets   int
	MaxWatches   int

	MaxHTTPHeaderBytes int
	MaxHTTPBodyBytes   int64

	FSWorkers int
}

// Event 是运行时向宿主上报的可观测事件。
type Event struct {
	Kind     string
	Err      error
	Scope    string
	Op       string
	Resource string
	Allowed  bool
	Code     int
}

// PermissionError 是一次权限拒绝；Code 用于映射成 Node 风格错误码。
type PermissionError struct {
	Scope    string
	Op       string
	Resource string
	Rule     string
	Code     string
}

func (e *PermissionError) Error() string {
	if e == nil {
		return "jsrt: 权限拒绝"
	}
	rule := ""
	if e.Rule != "" {
		rule = "，规则 " + e.Rule
	}
	return fmt.Sprintf("jsrt: %s %s %q 被拒绝%s", e.Scope, e.Op, e.Resource, rule)
}

func deny(scope, op, resource, rule, code string) *PermissionError {
	if code == "" {
		code = "EACCES"
	}
	return &PermissionError{Scope: scope, Op: op, Resource: resource, Rule: rule, Code: code}
}

// Validate 校验配置；零值（未启用）总是合法。
func (p FSPermission) Validate() error {
	if !p.Enabled {
		return nil
	}
	n := p.normalized()
	if strings.TrimSpace(n.Root) == "" {
		return errors.New("jsrt: FSPermission.Root 不能为空")
	}
	if n.Access > FSReadWrite {
		return fmt.Errorf("jsrt: 未知 FSAccess %d", n.Access)
	}
	// normalized 会把越界的 Cwd 折回 Mount；这里单独检查原始值，
	// 让配置错误显式失败，而不是静默改成 Mount。
	if p.Cwd != "" {
		cwd := cleanVirtualPath(p.Cwd)
		if cwd != n.Mount && !strings.HasPrefix(cwd, n.Mount+"/") {
			return fmt.Errorf("jsrt: FSPermission.Cwd %q 不在 Mount %q 内", p.Cwd, n.Mount)
		}
	}
	if n.MaxFileBytes < 0 || n.MaxOpenFiles < 0 || n.MaxDirEntries < 0 {
		return errors.New("jsrt: FSPermission 的配额不能为负")
	}
	for _, pat := range append(append(append([]string{}, n.Allow...), n.WriteAllow...), n.Deny...) {
		if _, err := compilePathPattern(pat, n.Mount, pathCaseFold()); err != nil {
			return err
		}
	}
	return nil
}

func (p FSPermission) normalized() FSPermission {
	if p.Mount == "" {
		p.Mount = "/workspace"
	}
	p.Mount = cleanVirtualPath(p.Mount)
	if p.Cwd == "" {
		p.Cwd = p.Mount
	}
	p.Cwd = cleanVirtualPath(p.Cwd)
	if p.Cwd != p.Mount && !strings.HasPrefix(p.Cwd, p.Mount+"/") {
		p.Cwd = p.Mount
	}
	return p
}

func cleanVirtualPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

func pathCaseFold() bool { return runtime.GOOS == "windows" }

// pathPattern 是编译后的路径 glob。
type pathPattern struct {
	raw      string
	re       *regexp.Regexp
	exact    string
	caseFold bool
}

func compilePathPattern(pattern, mount string, caseFold bool) (pathPattern, error) {
	raw := strings.TrimSpace(pattern)
	if raw == "" {
		return pathPattern{}, errors.New("jsrt: 路径模式不能为空")
	}
	if strings.ContainsRune(raw, 0) {
		return pathPattern{}, errors.New("jsrt: 路径模式不能包含 NUL")
	}
	raw = strings.ReplaceAll(raw, "\\", "/")
	if !strings.HasPrefix(raw, "/") {
		raw = path.Join(mount, raw)
	} else {
		raw = path.Clean(raw)
	}
	if raw != mount && !strings.HasPrefix(raw, mount+"/") {
		return pathPattern{}, fmt.Errorf("jsrt: 路径模式 %q 不在 Mount %q 内", pattern, mount)
	}
	exact := ""
	if strings.HasSuffix(raw, "/**") {
		exact = strings.TrimSuffix(raw, "/**")
		if exact == "" {
			exact = "/"
		}
	}
	var b strings.Builder
	if caseFold {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	for i := 0; i < len(raw); {
		switch raw[i] {
		case '*':
			if i+1 < len(raw) && raw[i+1] == '*' {
				if i+2 < len(raw) && raw[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			r, size := utf8.DecodeRuneInString(raw[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return pathPattern{}, fmt.Errorf("jsrt: 路径模式 %q 无效: %w", pattern, err)
	}
	return pathPattern{raw: pattern, re: re, exact: exact, caseFold: caseFold}, nil
}

func (p pathPattern) match(s string) bool {
	if p.exact != "" {
		if p.caseFold {
			if strings.EqualFold(s, p.exact) {
				return true
			}
		} else if s == p.exact {
			return true
		}
	}
	return p.re.MatchString(s)
}

func compilePathPatterns(patterns []string, mount string, caseFold bool) ([]pathPattern, error) {
	out := make([]pathPattern, 0, len(patterns))
	for _, raw := range patterns {
		p, err := compilePathPattern(raw, mount, caseFold)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func matchPathPatterns(patterns []pathPattern, s string) bool {
	for _, p := range patterns {
		if p.match(s) {
			return true
		}
	}
	return false
}

// fsPolicy 是编译后的文件权限；不可变，可并发使用。
type fsPolicy struct {
	perm       FSPermission
	allow      []pathPattern
	writeAllow []pathPattern
	deny       []pathPattern
}

func newFSPolicy(p FSPermission) (*fsPolicy, error) {
	if !p.Enabled {
		return nil, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p = p.normalized()
	pol := &fsPolicy{perm: p}
	var err error
	if pol.allow, err = compilePathPatterns(p.Allow, p.Mount, pathCaseFold()); err != nil {
		return nil, err
	}
	if pol.writeAllow, err = compilePathPatterns(p.WriteAllow, p.Mount, pathCaseFold()); err != nil {
		return nil, err
	}
	if pol.deny, err = compilePathPatterns(p.Deny, p.Mount, pathCaseFold()); err != nil {
		return nil, err
	}
	return pol, nil
}

func (p *fsPolicy) mount() string {
	if p == nil {
		return ""
	}
	return p.perm.Mount
}

func (p *fsPolicy) virtual(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", deny("fs", "read", v, "空路径", "EACCES")
	}
	if strings.ContainsRune(v, 0) {
		return "", deny("fs", "read", v, "NUL", "EACCES")
	}
	if !strings.HasPrefix(v, "/") {
		v = path.Join(p.perm.Cwd, v)
	}
	return path.Clean(strings.ReplaceAll(v, "\\", "/")), nil
}

// check 判定一次路径访问；op 取 "read" 或 "write"。
func (p *fsPolicy) check(op, vpath string) error {
	if p == nil {
		return deny("fs", op, vpath, "", "EACCES")
	}
	v, err := p.virtual(vpath)
	if err != nil {
		return err
	}
	if v != p.perm.Mount && !strings.HasPrefix(v, p.perm.Mount+"/") {
		return deny("fs", op, v, "不在 Mount 内", "EACCES")
	}
	if matchPathPatterns(p.deny, v) {
		return deny("fs", op, v, "Deny", "EACCES")
	}
	switch op {
	case "read":
		if len(p.allow) > 0 && !matchPathPatterns(p.allow, v) {
			return deny("fs", op, v, "Allow", "EACCES")
		}
	case "write":
		if p.perm.Access != FSReadWrite {
			return deny("fs", op, v, "只读", "EROFS")
		}
		if len(p.allow) > 0 && !matchPathPatterns(p.allow, v) {
			return deny("fs", op, v, "Allow", "EACCES")
		}
		if len(p.writeAllow) > 0 && !matchPathPatterns(p.writeAllow, v) {
			return deny("fs", op, v, "WriteAllow", "EACCES")
		}
	default:
		return fmt.Errorf("jsrt: 未知文件权限操作 %q", op)
	}
	return nil
}

// checkAccess 用于没有路径的 fd 操作：只校验整体读写级别。
func (p *fsPolicy) checkAccess(op string) error {
	if p == nil {
		return deny("fs", op, "", "", "EACCES")
	}
	if op == "write" && p.perm.Access != FSReadWrite {
		return deny("fs", op, "", "只读", "EROFS")
	}
	return nil
}

// Validate 校验网络权限配置。
func (p NetPermission) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.MaxConnections < 0 || p.MaxRequestBytes < 0 || p.MaxResponseBytes < 0 {
		return errors.New("jsrt: NetPermission 的配额不能为负")
	}
	if p.DialTimeout < 0 || p.IdleTimeout < 0 {
		return errors.New("jsrt: NetPermission 的超时不能为负")
	}
	for _, raw := range append(append(append([]string{}, p.Allow...), p.Deny...), p.Listen...) {
		if _, err := parseNetRule(raw); err != nil {
			return err
		}
	}
	for _, proto := range p.Protocols {
		if !validScheme(proto) {
			return fmt.Errorf("jsrt: 非法协议 %q", proto)
		}
	}
	return nil
}

func validScheme(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '+' || r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

type netRule struct {
	raw      string
	scheme   string
	host     string
	wildcard bool
	ipNet    *net.IPNet
	port     int // -1 表示任意端口
}

func parseNetRule(raw string) (netRule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return netRule{}, errors.New("jsrt: 网络规则不能为空")
	}
	r := netRule{raw: raw, port: -1}
	if i := strings.Index(s, "://"); i >= 0 {
		r.scheme = strings.ToLower(s[:i])
		if !validScheme(r.scheme) {
			return netRule{}, fmt.Errorf("jsrt: 非法网络规则 %q", raw)
		}
		s = s[i+3:]
	}
	host, port, err := splitRuleHostPort(s)
	if err != nil {
		return netRule{}, fmt.Errorf("jsrt: 网络规则 %q: %w", raw, err)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return netRule{}, fmt.Errorf("jsrt: 网络规则 %q 缺少主机", raw)
	}
	if port != "" && port != "*" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return netRule{}, fmt.Errorf("jsrt: 网络规则 %q 的端口无效", raw)
		}
		r.port = n
	}
	switch {
	case host == "*":
		r.host = "*"
	case strings.HasPrefix(host, "*."):
		suffix := strings.TrimPrefix(host, "*.")
		if suffix == "" || strings.Contains(suffix, "*") {
			return netRule{}, fmt.Errorf("jsrt: 网络规则 %q 的通配符无效", raw)
		}
		r.host = suffix
		r.wildcard = true
	default:
		if strings.Contains(host, "*") {
			return netRule{}, fmt.Errorf("jsrt: 网络规则 %q 的通配符只能位于开头", raw)
		}
		if ip, ipnet, err := net.ParseCIDR(host); err == nil {
			_ = ip
			r.ipNet = ipnet
			r.host = host
		} else if ip := net.ParseIP(host); ip != nil {
			r.host = ip.String()
		} else {
			r.host = host
		}
	}
	return r, nil
}

func splitRuleHostPort(s string) (host, port string, err error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", errors.New("IPv6 地址缺少 ]")
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", errors.New("] 后只能是 :port")
		}
		return host, rest[1:], nil
	}
	if c := strings.Count(s, ":"); c == 1 {
		i := strings.LastIndex(s, ":")
		p := s[i+1:]
		if p == "*" || isDigits(p) {
			return s[:i], p, nil
		}
		return "", "", errors.New("冒号后不是端口")
	}
	return s, "", nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (r netRule) match(host string, port int, scheme string) bool {
	if r.scheme != "" && !strings.EqualFold(r.scheme, scheme) {
		return false
	}
	if r.port >= 0 && r.port != port {
		return false
	}
	if r.host == "*" {
		return true
	}
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if r.ipNet != nil {
		ip := net.ParseIP(h)
		return ip != nil && r.ipNet.Contains(ip)
	}
	if r.wildcard {
		suffix := r.host
		return len(h) > len(suffix)+1 && strings.HasSuffix(h, "."+suffix)
	}
	if ruleIP := net.ParseIP(r.host); ruleIP != nil {
		hIP := net.ParseIP(h)
		return hIP != nil && ruleIP.Equal(hIP)
	}
	return strings.EqualFold(h, r.host)
}

func (r netRule) matchIP(ip net.IP, port int, scheme string) bool {
	if r.scheme != "" && !strings.EqualFold(r.scheme, scheme) {
		return false
	}
	if r.port >= 0 && r.port != port {
		return false
	}
	if r.ipNet != nil {
		return r.ipNet.Contains(ip)
	}
	if ruleIP := net.ParseIP(r.host); ruleIP != nil {
		return ruleIP.Equal(ip)
	}
	return false
}

// netPolicy 是编译后的网络权限；不可变，可并发使用。
type netPolicy struct {
	perm   NetPermission
	allow  []netRule
	deny   []netRule
	listen []netRule
}

func newNetPolicy(p NetPermission) (*netPolicy, error) {
	if !p.Enabled {
		return nil, nil
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	pol := &netPolicy{perm: p}
	var err error
	if pol.allow, err = compileNetRules(p.Allow); err != nil {
		return nil, err
	}
	if pol.deny, err = compileNetRules(p.Deny); err != nil {
		return nil, err
	}
	if pol.listen, err = compileNetRules(p.Listen); err != nil {
		return nil, err
	}
	return pol, nil
}

func compileNetRules(raw []string) ([]netRule, error) {
	out := make([]netRule, 0, len(raw))
	for _, s := range raw {
		r, err := parseNetRule(s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (p *netPolicy) checkDial(host, port string, scheme string) error {
	if p == nil || !p.perm.Enabled || !p.perm.Outbound {
		return deny("net", "connect", netResource(host, port), "未启用出站", "ERR_ACCESS_DENIED")
	}
	if len(p.perm.Protocols) > 0 && scheme != "" {
		found := false
		for _, proto := range p.perm.Protocols {
			if strings.EqualFold(proto, scheme) {
				found = true
				break
			}
		}
		if !found {
			return deny("net", "connect", netResource(host, port), "协议不在 Protocols 内", "ERR_ACCESS_DENIED")
		}
	}
	portNum, ok := parsePortNumber(port)
	if !ok {
		return deny("net", "connect", netResource(host, port), "非法端口", "ERR_ACCESS_DENIED")
	}
	for _, r := range p.deny {
		if r.match(host, portNum, scheme) {
			return deny("net", "connect", netResource(host, port), "Deny", "ERR_ACCESS_DENIED")
		}
	}
	if len(p.allow) == 0 {
		return deny("net", "connect", netResource(host, port), "Allow 为空", "ERR_ACCESS_DENIED")
	}
	for _, r := range p.allow {
		if r.match(host, portNum, scheme) {
			return nil
		}
	}
	return deny("net", "connect", netResource(host, port), "Allow", "ERR_ACCESS_DENIED")
}

// checkIP 在 DNS 解析之后调用：拒绝命中的 IP/私网。
// 主机名白名单允许的地址仍要过这里，防止 DNS rebinding 与私网 SSRF。
func (p *netPolicy) checkIP(ip net.IP, port int, scheme string) error {
	if p == nil || !p.perm.Enabled || !p.perm.Outbound {
		return deny("net", "connect", ip.String(), "未启用出站", "ERR_ACCESS_DENIED")
	}
	for _, r := range p.deny {
		if r.matchIP(ip, port, scheme) {
			return deny("net", "connect", ip.String(), "Deny", "ERR_ACCESS_DENIED")
		}
	}
	if isPrivateIP(ip) && !p.perm.AllowPrivateIPs {
		for _, r := range p.allow {
			if r.port >= 0 && r.port != port {
				continue
			}
			if r.scheme != "" && !strings.EqualFold(r.scheme, scheme) {
				continue
			}
			if r.ipNet != nil && r.ipNet.Contains(ip) {
				return nil
			}
			if ruleIP := net.ParseIP(r.host); ruleIP != nil && ruleIP.Equal(ip) {
				return nil
			}
		}
		return deny("net", "connect", ip.String(), "私网地址", "ERR_ACCESS_DENIED")
	}
	return nil
}

func (p *netPolicy) checkListen(host, port string) error {
	if p == nil || !p.perm.Enabled || !p.perm.Inbound {
		return deny("net", "listen", netResource(host, port), "未启用入站", "ERR_ACCESS_DENIED")
	}
	portNum, ok := parsePortNumber(port)
	if !ok {
		return deny("net", "listen", netResource(host, port), "非法端口", "ERR_ACCESS_DENIED")
	}
	if len(p.listen) == 0 {
		return deny("net", "listen", netResource(host, port), "Listen 为空", "ERR_ACCESS_DENIED")
	}
	for _, r := range p.listen {
		if r.match(host, portNum, "") {
			return nil
		}
	}
	return deny("net", "listen", netResource(host, port), "Listen", "ERR_ACCESS_DENIED")
}

func (p *netPolicy) checkDNS(host string) error {
	if p == nil || !p.perm.Enabled || !p.perm.DNS {
		return deny("net", "lookup", host, "DNS 未启用", "ERR_ACCESS_DENIED")
	}
	for _, r := range p.deny {
		if r.match(host, -1, "") {
			return deny("net", "lookup", host, "Deny", "ERR_ACCESS_DENIED")
		}
	}
	return nil
}

func netResource(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

func parsePortNumber(port string) (int, bool) {
	if port == "" || port == "*" {
		return -1, true
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return 0, false
	}
	return n, true
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// Getenv 权限：process.env 的读取与写入。
func (p EnvPermission) Validate() error {
	for _, pat := range p.Allow {
		if _, err := path.Match(pat, ""); err != nil {
			return fmt.Errorf("jsrt: 环境变量模式 %q 无效: %w", pat, err)
		}
	}
	return nil
}

func (p EnvPermission) check(name string) bool {
	if name == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	for _, pat := range p.Allow {
		if runtime.GOOS == "windows" {
			pat = strings.ToLower(pat)
		}
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// Validate 校验子进程权限配置。
func (p ChildPermission) Validate() error {
	if !p.Enabled {
		return nil
	}
	if p.MaxOutputBytes < 0 || p.Timeout < 0 {
		return errors.New("jsrt: ChildPermission 的配额不能为负")
	}
	for _, pat := range p.Allow {
		if _, err := filepath.Match(pat, ""); err != nil {
			return fmt.Errorf("jsrt: 子进程模式 %q 无效: %w", pat, err)
		}
	}
	return nil
}

func (p ChildPermission) check(program string) error {
	if !p.Enabled {
		return deny("child", "spawn", program, "未启用", "ERR_ACCESS_DENIED")
	}
	if len(p.Allow) == 0 {
		return deny("child", "spawn", program, "Allow 为空", "ERR_ACCESS_DENIED")
	}
	base := filepath.Base(program)
	clean := filepath.Clean(program)
	for _, pat := range p.Allow {
		target := base
		if strings.ContainsAny(pat, `/\`) {
			target = clean
		}
		if ok, _ := filepath.Match(pat, target); ok {
			return nil
		}
		if runtime.GOOS == "windows" {
			if ok, _ := filepath.Match(strings.ToLower(pat), strings.ToLower(target)); ok {
				return nil
			}
		}
	}
	return deny("child", "spawn", program, "Allow", "ERR_ACCESS_DENIED")
}

// Validate 校验 Limits；负值一律拒绝。
func (l Limits) Validate() error {
	if l.MemoryBytes < 0 || l.StartupTimeout < 0 || l.CallTimeout < 0 || l.CloseTimeout < 0 {
		return errors.New("jsrt: Limits 的内存/超时不能为负")
	}
	if l.MaxTimers < 0 || l.MaxListeners < 0 || l.MaxSockets < 0 || l.MaxWatches < 0 {
		return errors.New("jsrt: Limits 的计数不能为负")
	}
	if l.MaxHTTPHeaderBytes < 0 || l.MaxHTTPBodyBytes < 0 || l.FSWorkers < 0 {
		return errors.New("jsrt: Limits 的 HTTP/FS 限制不能为负")
	}
	return nil
}

func (o Options) validate() error {
	switch o.Mode {
	case LoopScoped, LoopResident:
	default:
		return fmt.Errorf("jsrt: 未知 LoopMode %d", o.Mode)
	}
	switch o.ExitPolicy {
	case ExitDeny, ExitRequest:
	default:
		return fmt.Errorf("jsrt: 未知 ExitPolicy %d", o.ExitPolicy)
	}
	if err := o.Limits.Validate(); err != nil {
		return err
	}
	if err := o.Permissions.FS.Validate(); err != nil {
		return err
	}
	if err := o.Permissions.Net.Validate(); err != nil {
		return err
	}
	if err := o.Permissions.Child.Validate(); err != nil {
		return err
	}
	if err := o.Permissions.Env.Validate(); err != nil {
		return err
	}
	return nil
}

// fsPermission 返回生效的文件权限：Permissions.FS 优先；否则映射旧的 Node.FS。
func (o Options) fsPermission() (FSPermission, error) {
	if o.Permissions.FS.Enabled {
		if err := o.Permissions.FS.Validate(); err != nil {
			return FSPermission{}, err
		}
		return o.Permissions.FS.normalized(), nil
	}
	if o.Node != nil && o.Node.FS != nil && strings.TrimSpace(o.Node.FS.Root) != "" {
		p := FSPermission{
			Enabled: true,
			Root:    o.Node.FS.Root,
			Mount:   o.Node.FS.Mount,
			Cwd:     o.Node.FS.Cwd,
			Access:  FSReadOnly,
		}
		if o.Node.FS.Write {
			p.Access = FSReadWrite
		}
		return p.normalized(), nil
	}
	return FSPermission{}, nil
}

// PermissionView 是只读权限视图，对应 Node 的 process.permission.has。
type PermissionView interface {
	Has(scope, resource string) bool
}

type permissionView struct{ r *Runtime }

func (v permissionView) Has(scope, resource string) bool {
	r := v.r
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "fs.read", "fs.write":
		if r.fsPolicy == nil {
			return false
		}
		op := "read"
		if strings.EqualFold(scope, "fs.write") {
			op = "write"
		}
		return r.fsPolicy.check(op, resource) == nil
	case "net", "net.connect":
		if r.netPolicy == nil {
			return false
		}
		host, port, scheme, ok := splitResource(resource)
		if !ok {
			return false
		}
		return r.netPolicy.checkDial(host, port, scheme) == nil
	case "net.listen":
		if r.netPolicy == nil {
			return false
		}
		host, port, _, ok := splitResource(resource)
		if !ok {
			return false
		}
		return r.netPolicy.checkListen(host, port) == nil
	case "dns":
		if r.netPolicy == nil {
			return false
		}
		return r.netPolicy.checkDNS(resource) == nil
	case "child":
		return r.opts.Permissions.Child.check(resource) == nil
	case "env":
		return r.opts.Permissions.Env.check(resource)
	default:
		return false
	}
}

func splitResource(resource string) (host, port, scheme string, ok bool) {
	s := strings.TrimSpace(resource)
	if s == "" {
		return "", "", "", false
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme = strings.ToLower(s[:i])
		s = s[i+3:]
	}
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", "", "", false
		}
		host = s[1:end]
		rest := s[end+1:]
		if rest == "" {
			return host, "", scheme, true
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", "", false
		}
		return host, rest[1:], scheme, true
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && strings.Count(s, ":") == 1 {
		return s[:i], s[i+1:], scheme, true
	}
	return s, "", scheme, true
}

// emitEvent 把事件交给宿主；宿主回调的 panic 不能影响运行时。
func (r *Runtime) emitEvent(ev Event) {
	if r == nil || r.onEvent == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		r.onEvent(ev)
	}()
}

// Permission 返回当前运行时的只读权限视图。
func (r *Runtime) Permission() PermissionView { return permissionView{r: r} }
