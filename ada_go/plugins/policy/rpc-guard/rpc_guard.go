// Package rpcguard 是 RPC 的确定性门禁:出站地址/方法/能力,入站监听/对端/认证。
//
// 它是 policy 装饰器,包裹 seam/rpc 的 Service;不读 secret,只把解析出的 token
// 交给协议实现(provider/rpc-json)。P4 的 TLS 见 provider/rpc-tls。
package rpcguard

import (
	"context"
	"net"

	"github.com/issueye/ada_go_v2/platform/caps"
	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	seamsecrets "github.com/issueye/ada_go_v2/plugins/seam/secrets"
)

// PolicyName 是插件 id。
const PolicyName = "rpc-guard"

// Priority 是装饰器优先级(高者在外层);与 fs-sandbox 同层。
const Priority = 200

type Config struct {
	Peers         []string `json:"peers,omitempty"`
	Methods       []string `json:"methods,omitempty"`
	Listen        []string `json:"listen,omitempty"`
	Call          []string `json:"call,omitempty"`
	Auth          string   `json:"auth,omitempty"`
	TokenSecret   string   `json:"tokenSecret,omitempty"`
	AllowRemote   bool     `json:"allowRemote,omitempty"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
}

type guard struct {
	inner   seamrpc.Service
	broker  *caps.Broker
	secrets *seamsecrets.Chain
	conf    Config
}

// New 构造 rpc 门禁;broker/secrets 可为 nil(只做地址/方法/对端白名单)。
func New(inner seamrpc.Service, broker *caps.Broker, secretChain *seamsecrets.Chain, conf Config) seamrpc.Service {
	return &guard{inner: inner, broker: broker, secrets: secretChain, conf: conf}
}

func (g *guard) Dial(ctx context.Context, addr seamrpc.Address, opts seamrpc.DialOptions) (seamrpc.Peer, error) {
	if !allowlisted(g.conf.Call, addr.String()) {
		return nil, seamrpc.Denied("出站地址不在 call 白名单: " + addr.String())
	}
	if err := g.applyAuth(ctx, &opts.Auth); err != nil {
		return nil, err
	}
	peer, err := g.inner.Dial(ctx, addr, opts)
	if err != nil {
		return nil, err
	}
	return &guardedPeer{Peer: peer, g: g, addr: addr}, nil
}

func (g *guard) Listen(ctx context.Context, addr seamrpc.Address, opts seamrpc.ServerOptions) (seamrpc.Server, error) {
	if err := g.authorize(ctx, "rpc.listen."+string(addr.Network), addr.String()); err != nil {
		return nil, err
	}
	if !allowlisted(g.conf.Listen, addr.String()) {
		return nil, seamrpc.Denied("监听地址不在 listen 白名单: " + addr.String())
	}
	if len(g.conf.Peers) == 0 {
		return nil, seamrpc.AuthFailed("rpc-guard 必须配置至少一个允许的客户端名")
	}
	if (addr.Network == seamrpc.TCP || addr.Network == seamrpc.NetworkTLS) && !isLoopback(addr.Name) {
		if !g.conf.AllowRemote {
			return nil, seamrpc.Denied("拒绝非 loopback 监听(需 allowRemote): " + addr.String())
		}
		if addr.Network == seamrpc.TCP {
			return nil, seamrpc.Denied("远程明文 TCP 不允许;请使用 tls://: " + addr.String())
		}
	}
	opts.PeerIDs = append([]string(nil), g.conf.Peers...)
	if err := g.applyAuth(ctx, &opts.Auth); err != nil {
		return nil, err
	}
	srv, err := g.inner.Listen(ctx, addr, opts)
	if err != nil {
		return nil, err
	}
	return &guardedServer{Server: srv, g: g}, nil
}

// authorize 从 principal 取身份并向 Broker 查能力;没有 Broker 时只做白名单。
func (g *guard) authorize(ctx context.Context, spec, target string) error {
	if g.broker == nil {
		return nil
	}
	id, ok := kernel.Principal(ctx)
	if !ok {
		return seamrpc.Denied("缺少调用者身份: 请使用 ctx.Context()")
	}
	if err := g.broker.Authorize(ctx, id, spec, target); err != nil {
		return seamrpc.Denied(err.Error())
	}
	return nil
}

// applyAuth 按配置把 token 从 seam/secrets 解析后注入协议选项。
func (g *guard) applyAuth(ctx context.Context, auth *seamrpc.Auth) error {
	switch g.conf.Auth {
	case "", "os":
		if g.conf.Auth == "os" {
			auth.Mode = seamrpc.AuthOS
		}
	case "token":
		if g.conf.TokenSecret == "" {
			return seamrpc.AuthFailed("auth=token 但未配置 tokenSecret")
		}
		if g.secrets == nil {
			return seamrpc.AuthFailed("auth=token 但 secrets 服务不可用")
		}
		getter, err := g.secrets.Get()
		if err != nil {
			return seamrpc.AuthFailed("取得 secrets 实现失败: " + err.Error())
		}
		token, err := getter.Get(ctx, g.conf.TokenSecret)
		if err != nil || token == "" {
			return seamrpc.AuthFailed("读取 token secret 失败: " + g.conf.TokenSecret)
		}
		auth.Mode = seamrpc.AuthToken
		auth.Token = []byte(token)
	case "none":
		if !g.conf.AllowInsecure {
			return seamrpc.AuthFailed("auth=none 必须显式 allowInsecure")
		}
		auth.Mode = seamrpc.AuthNone
		auth.AllowInsecure = true
	default:
		return seamrpc.Invalid("未知 auth 模式: " + g.conf.Auth)
	}
	return nil
}

func allowlisted(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

// isLoopback 判断 tcp 监听名是否只绑定 loopback。
func isLoopback(name string) bool {
	host, _, err := net.SplitHostPort(name)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// guardedPeer 在每次 Call 上检查方法白名单与 rpc.call.<method>。
type guardedPeer struct {
	seamrpc.Peer
	g    *guard
	addr seamrpc.Address
}

func (p *guardedPeer) Call(ctx context.Context, method string, params, result any) error {
	if !p.g.methodAllowed(method) {
		return seamrpc.Denied("方法不在 methods 白名单: " + method)
	}
	if err := p.g.authorize(ctx, "rpc.call."+method, method); err != nil {
		// 方法级能力失败时,若已授予泛化 rpc.call 则不拦截(保持向后兼容的粗粒度授权)。
		if !p.g.hasGenericCall(ctx) {
			return err
		}
	}
	return p.Peer.Call(ctx, method, params, result)
}

func (g *guard) methodAllowed(method string) bool {
	for _, m := range g.conf.Methods {
		if m == method {
			return true
		}
	}
	return false
}

func (g *guard) hasGenericCall(ctx context.Context) bool {
	if g.broker == nil {
		return true
	}
	id, ok := kernel.Principal(ctx)
	if !ok {
		return false
	}
	return g.broker.Has(id, caps.Cap("rpc.call"), "")
}

// guardedServer 在 Handle 上检查入站方法白名单。
type guardedServer struct {
	seamrpc.Server
	g *guard
}

func (s *guardedServer) Handle(method string, h seamrpc.Handler) (kernel.Dispose, error) {
	if !s.g.methodAllowed(method) {
		return nil, seamrpc.Denied("入站方法不在 methods 白名单: " + method)
	}
	return s.Server.Handle(method, h)
}

// Plugin 返回 rpc-guard 插件:inject rpc,可选 caps/secrets。
func Plugin() kernel.Plugin {
	return kernel.Func(PolicyName, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamrpc.Chain](ctx, "rpc")
		if err != nil {
			return nil, err
		}
		var conf Config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "rpc-guard: 解析配置失败: %v", err)
		}
		if conf.Auth == "token" && conf.TokenSecret == "" {
			return nil, kernel.Fail(kernel.CodeInvalid, "rpc-guard: auth=token 需要 tokenSecret")
		}
		var broker *caps.Broker
		if v, ok := ctx.TryService("caps"); ok {
			broker, _ = v.(*caps.Broker)
		}
		var secretChain *seamsecrets.Chain
		if v, ok := ctx.TryService("secrets"); ok {
			secretChain, _ = v.(*seamsecrets.Chain)
		}
		return chain.Decorate(func(inner seamrpc.Service) seamrpc.Service {
			return New(inner, broker, secretChain, conf)
		}, Priority)
	}).WithOptional("caps", "secrets").WithInject("rpc")
}
