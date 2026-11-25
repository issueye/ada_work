// Package caps 是 M5 的静态能力授权(L2 基础设施,不是插件)。
//
// 它只回答两个问题:「这个插件声明了哪些能力」「这次调用是否被授权」;
// 能力的实际强制点在装饰器/服务层(fs/shell/sandbox),不在本包。
package caps

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CodeDenied 是能力拒绝的错误码(与安全契约 EDENIED 一致)。
const CodeDenied = "EDENIED"

// Cap 是一个能力名,如 fs.read、fs.write、exec、secrets.GITHUB_TOKEN。
type Cap string

// String 返回能力名。
func (c Cap) String() string { return string(c) }

// Namespace 返回能力的第一段(便于分组与统计)。
func (c Cap) Namespace() string {
	if i := strings.Index(string(c), "."); i >= 0 {
		return string(c)[:i]
	}
	return string(c)
}

// Trust 是信任等级。能力必须满足 MinTrust ≤ trust。
type Trust string

const (
	TrustBuiltin    Trust = "builtin"
	TrustOfficial   Trust = "official"
	TrustVerified   Trust = "verified"
	TrustThirdParty Trust = "third-party"
)

// ValidTrust 判断信任等级是否合法。
func ValidTrust(t Trust) bool {
	switch t {
	case TrustBuiltin, TrustOfficial, TrustVerified, TrustThirdParty:
		return true
	}
	return false
}

func trustRank(t Trust) int {
	switch t {
	case TrustBuiltin:
		return 3
	case TrustOfficial:
		return 2
	case TrustVerified:
		return 1
	}
	return 0
}

// Requirement 返回某能力要求的最低信任等级(design §13.2)。
func Requirement(c Cap) Trust {
	switch c.Namespace() {
	case "exec", "process":
		return TrustOfficial
	case "fs":
		if string(c) == "fs.write" {
			return TrustVerified
		}
		return TrustThirdParty
	case "ui", "config", "secrets", "rpc":
		if string(c) == "rpc.call" || strings.HasPrefix(string(c), "rpc.call.") {
			return TrustThirdParty
		}
		return TrustVerified
	}
	return TrustThirdParty
}

// AllowedByTrust 判断信任等级是否足以持有该能力。
func AllowedByTrust(c Cap, trust Trust) bool {
	return trustRank(trust) >= trustRank(Requirement(c))
}

// Parse 校验能力名语法;未知命名空间视为非法。
func Parse(spec string) (Cap, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", fmt.Errorf("caps: 能力名不能为空")
	}
	parts := strings.Split(s, ".")
	switch parts[0] {
	case "fs":
		if len(parts) != 2 || (parts[1] != "read" && parts[1] != "write") {
			return "", fmt.Errorf("caps: fs 能力必须是 fs.read 或 fs.write,得到 %q", spec)
		}
	case "exec":
		if len(parts) != 1 {
			return "", fmt.Errorf("caps: exec 能力不能带子项: %q", spec)
		}
	case "process":
		if len(parts) != 1 && !(len(parts) == 2 && parts[1] == "isolated") {
			return "", fmt.Errorf("caps: process 能力只能是 process 或 process.isolated,得到 %q", spec)
		}
	case "secrets", "config":
		if len(parts) != 2 || parts[1] == "" {
			return "", fmt.Errorf("caps: %s 能力必须是 %s.<name>,得到 %q", parts[0], parts[0], spec)
		}
	case "ui":
		if len(parts) > 2 {
			return "", fmt.Errorf("caps: ui 能力层级过深: %q", spec)
		}
	case "rpc":
		if len(parts) < 2 {
			return "", fmt.Errorf("caps: rpc 能力必须是 rpc.call 或 rpc.listen.<network>,得到 %q", spec)
		}
		switch parts[1] {
		case "call":
			if len(parts) > 2 && strings.Join(parts[2:], ".") == "" {
				return "", fmt.Errorf("caps: rpc.call 方法为空: %q", spec)
			}
		case "listen":
			if len(parts) != 3 || (parts[2] != "tcp" && parts[2] != "unix" && parts[2] != "pipe" && parts[2] != "tls") {
				return "", fmt.Errorf("caps: rpc.listen 必须是 rpc.listen.<tcp|unix|pipe|tls>,得到 %q", spec)
			}
		default:
			return "", fmt.Errorf("caps: rpc 能力只支持 call/listen: %q", spec)
		}
	default:
		return "", fmt.Errorf("caps: 未知能力命名空间 %q", parts[0])
	}
	return Cap(s), nil
}

// Grants 是某个插件的能力集合(带通配)。
type Grants struct {
	set map[string]bool
}

// NewGrants 校验并构造能力集合。
func NewGrants(specs ...string) (Grants, error) {
	set := make(map[string]bool, len(specs))
	for _, spec := range specs {
		c, err := Parse(spec)
		if err != nil {
			return Grants{}, err
		}
		set[string(c)] = true
	}
	return Grants{set: set}, nil
}

// Has 判断能力是否被授予。通配规则:逐级前缀通配。
//
// 例:授予 secrets.* 可以覆盖 secrets.GITHUB_TOKEN,授予 rpc.call 可以覆盖 rpc.call.agent.session.list。
// 通配只放宽「目标」,不放宽命名空间(fs 不会因为持有 rpc.call 而获得 fs.read)。
func (g Grants) Has(c Cap, target string) bool {
	if g.set == nil {
		return false
	}
	s := string(c)
	if g.set[s] {
		return true
	}
	parts := strings.Split(s, ".")
	for i := len(parts) - 1; i >= 1; i-- {
		if g.set[strings.Join(parts[:i], ".")] || g.set[strings.Join(parts[:i], ".")+".*"] {
			return true
		}
	}
	return false
}

// List 返回能力名(排序,诊断用)。
func (g Grants) List() []string {
	out := make([]string, 0, len(g.set))
	for s := range g.set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// DeniedError 是能力拒绝错误。
type DeniedError struct {
	Plugin string
	Cap    Cap
	Target string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("%s: plugin %s 未被授权 %s(%s): %s", CodeDenied, e.Plugin, e.Cap, e.Target, e.Reason)
}

// IsDenied 判断错误是否为能力拒绝。
func IsDenied(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*DeniedError)
	return ok
}

// Deny 是一条拒绝审计记录。
type Deny struct {
	Plugin string    `json:"plugin"`
	Cap    string    `json:"cap"`
	Target string    `json:"target,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// EventDeny 是能力拒绝的观察事件名;audit-log 等观察者订阅它。
const EventDeny = "caps/deny"

// RedactTarget 去掉 URL 目标的 userinfo、fragment 与 query,避免 secret 进入审计。
// 非 URL 目标(如 fs 路径、secret 名)原样返回。
func RedactTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	return u.String()
}

// Broker 是能力授权表:插件 id → 能力集合。
//
// 授权是 fail-closed 的:未登记插件、未授予能力、语法非法都视为拒绝。
type Broker struct {
	mu     sync.RWMutex
	grants map[string]Grants
	audit  func(Deny)
}

// NewBroker 创建授权表;audit 为 nil 时丢弃拒绝审计。
func NewBroker(audit func(Deny)) *Broker {
	if audit == nil {
		audit = func(Deny) {}
	}
	return &Broker{grants: make(map[string]Grants), audit: audit}
}

// Grant 为插件授予能力;任一能力名非法时整批拒绝(不部分生效)。
func (b *Broker) Grant(plugin string, specs ...string) error {
	if plugin == "" {
		return fmt.Errorf("caps: 插件 id 不能为空")
	}
	grants, err := NewGrants(specs...)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.grants[plugin] = grants
	b.mu.Unlock()
	return nil
}

// Revoke 撤销插件的全部能力。
func (b *Broker) Revoke(plugin string) {
	b.mu.Lock()
	delete(b.grants, plugin)
	b.mu.Unlock()
}

// Has 判断某插件是否被授予能力。
func (b *Broker) Has(plugin string, c Cap, target string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	g, ok := b.grants[plugin]
	if !ok {
		return false
	}
	return g.Has(c, target)
}

// Authorize 是调用点的授权检查:未授权即拒绝并记审计。
func (b *Broker) Authorize(ctx context.Context, plugin, spec, target string) error {
	c, err := Parse(spec)
	if err != nil {
		target = RedactTarget(target)
		b.audit(Deny{Plugin: plugin, Cap: spec, Target: target,
			Reason: "能力名非法: " + err.Error(), At: time.Now()})
		return &DeniedError{Plugin: plugin, Cap: Cap(spec), Target: target, Reason: "能力名非法: " + err.Error()}
	}
	return b.AuthorizeCap(ctx, plugin, c, target)
}

// AuthorizeCap 是类型化版本的授权检查。
func (b *Broker) AuthorizeCap(_ context.Context, plugin string, c Cap, target string) error {
	if b.Has(plugin, c, target) {
		return nil
	}
	target = RedactTarget(target)
	b.audit(Deny{Plugin: plugin, Cap: string(c), Target: target,
		Reason: "能力未授予", At: time.Now()})
	return &DeniedError{Plugin: plugin, Cap: c, Target: target, Reason: "能力未授予"}
}

// AuthorizeWithTrust 额外检查信任等级是否足以持有该能力。
func (b *Broker) AuthorizeWithTrust(ctx context.Context, plugin string, c Cap, target string, trust Trust) error {
	if !ValidTrust(trust) {
		target = RedactTarget(target)
		b.audit(Deny{Plugin: plugin, Cap: string(c), Target: target,
			Reason: "信任等级非法", At: time.Now()})
		return &DeniedError{Plugin: plugin, Cap: c, Target: target, Reason: "信任等级非法"}
	}
	if !AllowedByTrust(c, trust) {
		target = RedactTarget(target)
		reason := fmt.Sprintf("信任等级 %s 不足(需要 %s)", trust, Requirement(c))
		b.audit(Deny{Plugin: plugin, Cap: string(c), Target: target, Reason: reason, At: time.Now()})
		return &DeniedError{Plugin: plugin, Cap: c, Target: target, Reason: reason}
	}
	return b.AuthorizeCap(ctx, plugin, c, target)
}

// Snapshot 返回全部插件的授权集合(诊断用)。
func (b *Broker) Snapshot() map[string][]string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string][]string, len(b.grants))
	for plugin, g := range b.grants {
		out[plugin] = g.List()
	}
	return out
}
