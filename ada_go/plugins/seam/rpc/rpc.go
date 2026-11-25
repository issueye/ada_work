// Package rpc 是 `seam/rpc` 的契约层:进程间控制面调用的地址、帧、对端与传输接口。
//
// 它只放类型/错误/事件名与无 I/O 的编排(Chain/Registry);协议实现由
// provider/rpc-json 提供,传输实现由 provider/rpc-net / rpc-pipe 提供。
// 契约来源:docs/seam-rpc.md。
package rpc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// Network 是传输类型。
type Network string

const (
	// TCP 是 TCP/Winsock 传输。
	TCP Network = "tcp"
	// Unix 是 AF_UNIX 传输(Linux/macOS,Windows 10 1803+)。
	Unix Network = "unix"
	// Pipe 是 Windows 命名管道传输(只有 windows 构建注册)。
	Pipe Network = "pipe"
	// TLS 是 TLS over TCP 传输(P4;证书由 provider/rpc-tls 加载)。
	NetworkTLS Network = "tls"
)

// Address 是一个 RPC 端点。
type Address struct {
	Network Network `json:"network"`
	Name    string  `json:"name"`
}

// Parse 解析 "tcp://host:port"、"unix:///abs/path"、"pipe://name"。
//
// 只做语法校验,不做可达性/权限判断;那些属于 provider 与 policy。
func Parse(raw string) (Address, error) {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return Address{}, Invalid("地址缺少 scheme: " + raw)
	}
	n := Network(strings.ToLower(raw[:i]))
	name := raw[i+3:]
	if name == "" {
		return Address{}, Invalid("地址缺少名字: " + raw)
	}
	if strings.ContainsRune(name, 0) {
		return Address{}, Invalid("地址包含 NUL: " + raw)
	}
	switch n {
	case TCP, NetworkTLS:
		host, port, err := net.SplitHostPort(name)
		if err != nil {
			return Address{}, Invalid("tcp 地址必须是 host:port: " + raw)
		}
		if port == "" {
			return Address{}, Invalid("tcp 端口为空: " + raw)
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 0 || p > 65535 {
			return Address{}, Invalid("tcp 端口非法: " + raw)
		}
		_ = host // 空 host 表示监听所有接口,由 policy 决定是否允许
	case Unix:
		if !strings.HasPrefix(name, "/") {
			return Address{}, Invalid("unix 地址必须是绝对路径: " + raw)
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return Address{}, Invalid("unix 地址不能包含 ..: " + raw)
			}
		}
	case Pipe:
		if strings.ContainsAny(name, `\/`) || strings.Contains(name, "..") {
			return Address{}, Invalid("pipe 名字不能含路径分隔符或 ..: " + raw)
		}
	default:
		return Address{}, Invalid("未知传输 scheme: " + raw)
	}
	return Address{Network: n, Name: name}, nil
}

// String 返回规范形式。
func (a Address) String() string {
	return string(a.Network) + "://" + a.Name
}

// Stream 是传输层的双向字节流。
//
// SetDeadline 尽力而为:命名管道的同步句柄返回 ErrUnsupported,
// 协议层用 ctx + Close 取消,不依赖 deadline。
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// Transport 是一个可注册的传输实现。
type Transport interface {
	Dial(ctx context.Context, addr Address) (Stream, error)
	Listen(ctx context.Context, addr Address, opts ListenOptions) (Listener, error)
}

// Listener 是传输层监听器。
type Listener interface {
	Accept(ctx context.Context) (Stream, error)
	Addr() Address
	Close() error
}

// ListenOptions 是传输层选项,不含协议语义。
type ListenOptions struct {
	Perm        os.FileMode // unix socket 权限;0 = 默认 0600
	RemoveStale bool        // 只删除确认是 socket 的 stale 文件
	MaxConns    int         // 0 = 不限(协议层仍限)
}

// Handler 是入站方法;params 是原始 JSON,由方法自己解码。
//
// 它在一个独立的 goroutine 里执行,ctx 带 rpc:<peer> principal。
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Event 是服务端推送的一条事件。
type Event struct {
	Topic string          `json:"topic"`
	Seq   uint64          `json:"seq"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Peer 是客户端连接。
type Peer interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error
	Subscribe(ctx context.Context, topics ...string) error
	Events() <-chan Event
	Remote() string
	Close() error
}

// Server 是入站服务。
type Server interface {
	Handle(method string, h Handler) (kernel.Dispose, error)
	Publish(topic string, data any) error
	Serve(ctx context.Context) error
	Stats() Stats
	Close() error
}

// Stats 是只读诊断计数。
type Stats struct {
	Listeners []Address `json:"listeners,omitempty"`
	Conns     int       `json:"conns"`
	Inflight  int       `json:"inflight"`
	FramesIn  uint64    `json:"framesIn"`
	FramesOut uint64    `json:"framesOut"`
	EventsOut uint64    `json:"eventsOut"`
}

// AuthMode 是鉴权模式。
type AuthMode string

const (
	// AuthOS 依赖 unix socket 权限/命名管道 ACL;不能配在 tcp 上。
	AuthOS AuthMode = "os"
	// AuthToken 使用 HMAC 挑战应答。
	AuthToken AuthMode = "token"
	// AuthNone 仅测试使用,必须显式 AllowInsecure。
	AuthNone AuthMode = "none"
)

// Auth 是鉴权配置;Token 由调用方从 seam/secrets 解析后传入,
// provider 不直接读 secret。
type Auth struct {
	Mode          AuthMode `json:"mode,omitempty"`
	Token         []byte   `json:"-"`
	AllowInsecure bool     `json:"allowInsecure,omitempty"`
}

// Limits 是协议层上限;零值由 provider 填默认值。
type Limits struct {
	MaxFrameBytes  int `json:"maxFrameBytes,omitempty"`
	MaxParamsBytes int `json:"maxParamsBytes,omitempty"`
	MaxInflight    int `json:"maxInflight,omitempty"`
	MaxEventQueue  int `json:"maxEventQueue,omitempty"`
	MaxConns       int `json:"maxConns,omitempty"`
}

// DialOptions 是客户端选项。
type DialOptions struct {
	Client  string        `json:"client,omitempty"`
	Auth    Auth          `json:"auth,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ServerOptions 是服务端选项。
type ServerOptions struct {
	Auth    Auth          `json:"auth,omitempty"`
	Limits  Limits        `json:"limits,omitempty"`
	PeerIDs []string      `json:"peerIds,omitempty"`
	Timeout time.Duration `json:"timeout,omitempty"` // 单次方法超时;0 = 不限
}

// Service 是协议入口;Chain 包裹它。
type Service interface {
	Dial(ctx context.Context, addr Address, opts DialOptions) (Peer, error)
	Listen(ctx context.Context, addr Address, opts ServerOptions) (Server, error)
}

// EventObserve 是 rpc 操作观察事件(观察不是门禁)。
const EventObserve = "rpc/observe"

// Invalid 构造参数错误。
func Invalid(msg string) error {
	return seamerr.New(seamerr.ErrInvalid, "rpc", msg, false)
}

// Denied 构造拒绝错误。
func Denied(msg string) error {
	return seamerr.New(seamerr.ErrDenied, "rpc", msg, false)
}

// Unavailable 构造不可达错误。
func Unavailable(msg string) error {
	return seamerr.New(seamerr.ErrUnavailable, "rpc", msg, true)
}

// AuthFailed 构造鉴权失败错误。
func AuthFailed(msg string) error {
	return seamerr.New(seamerr.ErrAuth, "rpc", msg, false)
}

// Unimplemented 构造「没有可用实现」错误。
func Unimplemented(msg string) error {
	return seamerr.Unimplemented("rpc", msg)
}

// Limit 构造超限错误。
func Limit(msg string) error {
	return seamerr.New(seamerr.ErrLimit, "rpc", msg, false)
}

// Internal 构造内部错误(细节不返回给对端)。
func Internal(msg string) error {
	return seamerr.New(seamerr.ErrInternal, "rpc", msg, false)
}
