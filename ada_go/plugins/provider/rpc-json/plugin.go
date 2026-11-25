package rpcjson

import (
	"context"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// 默认值(与 docs/seam-rpc.md 对齐)。
const (
	defaultHeartbeat  = 30 * time.Second
	defaultMaxParams  = 1 << 20
	defaultInflight   = 32
	defaultEventQueue = 1024
	defaultMaxConns   = 64
	minFrame          = 4 << 10
	maxFrameCap       = 64 << 20
)

type config struct {
	HeartbeatMs    int `json:"heartbeatMs,omitempty"`
	MaxFrameBytes  int `json:"maxFrameBytes,omitempty"`
	MaxParamsBytes int `json:"maxParamsBytes,omitempty"`
	MaxInflight    int `json:"maxInflight,omitempty"`
	MaxEventQueue  int `json:"maxEventQueue,omitempty"`
	MaxConns       int `json:"maxConns,omitempty"`
}

func (c *config) normalize() {
	if c.HeartbeatMs <= 0 {
		c.HeartbeatMs = int(defaultHeartbeat / time.Millisecond)
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = defaultMaxFrame
	}
	if c.MaxFrameBytes < minFrame {
		c.MaxFrameBytes = minFrame
	}
	if c.MaxFrameBytes > maxFrameCap {
		c.MaxFrameBytes = maxFrameCap
	}
	if c.MaxParamsBytes <= 0 {
		c.MaxParamsBytes = defaultMaxParams
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = defaultInflight
	}
	if c.MaxEventQueue <= 0 {
		c.MaxEventQueue = defaultEventQueue
	}
	if c.MaxConns <= 0 {
		c.MaxConns = defaultMaxConns
	}
}

func (c config) heartbeat() time.Duration {
	return time.Duration(c.HeartbeatMs) * time.Millisecond
}

type service struct {
	reg  *seamrpc.Registry
	conf config
}

func (s *service) Dial(ctx context.Context, addr seamrpc.Address, opts seamrpc.DialOptions) (seamrpc.Peer, error) {
	tr, ok := s.reg.Lookup(addr.Network)
	if !ok {
		return nil, seamrpc.Unimplemented("没有注册传输 " + string(addr.Network))
	}
	st, err := tr.Dial(ctx, addr)
	if err != nil {
		return nil, seamrpc.Unavailable("连接 " + addr.String() + " 失败: " + err.Error())
	}
	p, err := newPeer(ctx, st, opts, s.conf)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return p, nil
}

func (s *service) Listen(ctx context.Context, addr seamrpc.Address, opts seamrpc.ServerOptions) (seamrpc.Server, error) {
	tr, ok := s.reg.Lookup(addr.Network)
	if !ok {
		return nil, seamrpc.Unimplemented("没有注册传输 " + string(addr.Network))
	}
	if opts.Auth.Mode == "" {
		if addr.Network == seamrpc.TCP || addr.Network == seamrpc.NetworkTLS {
			opts.Auth.Mode = seamrpc.AuthToken
		} else {
			opts.Auth.Mode = seamrpc.AuthOS
		}
	}
	switch opts.Auth.Mode {
	case seamrpc.AuthOS:
		if addr.Network == seamrpc.TCP || addr.Network == seamrpc.NetworkTLS {
			return nil, seamrpc.Invalid("tcp/tls 不能用 os 鉴权")
		}
	case seamrpc.AuthToken:
		if len(opts.Auth.Token) == 0 {
			return nil, seamrpc.AuthFailed("token 鉴权缺少 token")
		}
	case seamrpc.AuthNone:
		if !opts.Auth.AllowInsecure {
			return nil, seamrpc.AuthFailed("none 鉴权必须显式 AllowInsecure")
		}
	default:
		return nil, seamrpc.Invalid("未知鉴权模式: " + string(opts.Auth.Mode))
	}
	if len(opts.PeerIDs) == 0 {
		return nil, seamrpc.Invalid("必须配置至少一个允许的客户端名")
	}
	ln, err := tr.Listen(ctx, addr, seamrpc.ListenOptions{Perm: 0o600})
	if err != nil {
		return nil, seamrpc.Unavailable("监听 " + addr.String() + " 失败: " + err.Error())
	}
	conf := s.conf
	if opts.Limits.MaxFrameBytes > 0 {
		conf.MaxFrameBytes = opts.Limits.MaxFrameBytes
	}
	if opts.Limits.MaxParamsBytes > 0 {
		conf.MaxParamsBytes = opts.Limits.MaxParamsBytes
	}
	if opts.Limits.MaxInflight > 0 {
		conf.MaxInflight = opts.Limits.MaxInflight
	}
	if opts.Limits.MaxEventQueue > 0 {
		conf.MaxEventQueue = opts.Limits.MaxEventQueue
	}
	if opts.Limits.MaxConns > 0 {
		conf.MaxConns = opts.Limits.MaxConns
	}
	conf.normalize()
	srv := newServer(ctx, ln, opts, conf)
	srv.modes = []string{string(opts.Auth.Mode)}
	return srv, nil
}

const pluginID = ProviderName

// Plugin 返回 rpc-json 插件:安装协议底座,不自动监听。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, cfg kernel.Config) (kernel.Dispose, error) {
		chain, err := kernel.Service[*seamrpc.Chain](ctx, "rpc")
		if err != nil {
			return nil, err
		}
		reg, err := kernel.Service[*seamrpc.Registry](ctx, "rpc-transport")
		if err != nil {
			return nil, err
		}
		var conf config
		if err := cfg.Decode(&conf); err != nil {
			return nil, kernel.Fail(kernel.CodeInvalid, "rpc-json: 解析配置失败: %v", err)
		}
		conf.normalize()
		return chain.Install(&service{reg: reg, conf: conf})
	}).WithInject("rpc", "rpc-transport")
}
