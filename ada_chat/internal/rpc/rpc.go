// Package rpc implements the client half of the seam/rpc P0–P2 protocol from
// docs/seam-rpc.md: address parsing, transport registry, the v1 JSON frame
// protocol (challenge/auth, calls, events, cancel, ping), and tcp/unix/pipe
// dialers. The server half lives in ada_go's rpc-json provider.
//
// ada_chat has no plugin kernel, so the seam/provider split is kept by file and
// type boundaries instead of separate plugin packages: this package holds the
// contract and protocol, and the transports live in transport_*.go. Policy
// (address allow lists, caps) is not implemented; hosts pass explicit addresses.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Code is a seam error code (docs/seam-api.md §1.2).
type Code string

const (
	ErrUnimplemented Code = "EUNIMPLEMENTED"
	ErrInvalid       Code = "EINVALID"
	ErrUnavailable   Code = "EUNAVAILABLE"
	ErrAuth          Code = "EAUTH"
	ErrRateLimit     Code = "ERATELIMIT"
	ErrContext       Code = "ECONTEXT"
	ErrCanceled      Code = "ECANCELED"
	ErrTimeout       Code = "ETIMEOUT"
	ErrDenied        Code = "EDENIED"
	ErrSchema        Code = "ESCHEMA"
	ErrInternal      Code = "EINTERNAL"
	ErrLimit         Code = "ELIMIT"
)

// Error is a seam error with a code and a retryable flag.
type Error struct {
	Code      Code
	Message   string
	Retryable bool
	Provider  string
	Err       error
}

func (e *Error) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("%s: %s: %s", e.Code, e.Provider, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a coded error.
func New(code Code, provider, message string, retryable bool) *Error {
	return &Error{Code: code, Provider: provider, Message: message, Retryable: retryable}
}

// Is reports whether err carries code.
func Is(err error, code Code) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Invalid builds an EINVALID error.
func Invalid(msg string) error { return New(ErrInvalid, "rpc", msg, false) }

// Denied builds an EDENIED error.
func Denied(msg string) error { return New(ErrDenied, "rpc", msg, false) }

// Unavailable builds an EUNAVAILABLE error.
func Unavailable(msg string) error { return New(ErrUnavailable, "rpc", msg, true) }

// AuthFailed builds an EAUTH error.
func AuthFailed(msg string) error { return New(ErrAuth, "rpc", msg, false) }

// Unimplemented builds an EUNIMPLEMENTED error.
func Unimplemented(msg string) error { return New(ErrUnimplemented, "rpc", msg, false) }

// Limit builds an ELIMIT error.
func Limit(msg string) error { return New(ErrLimit, "rpc", msg, false) }

// Internal builds an EINTERNAL error; the detail must not cross the wire boundary.
func Internal(msg string) error { return New(ErrInternal, "rpc", msg, false) }

// Network is a transport kind.
type Network string

const (
	TCP  Network = "tcp"
	Unix Network = "unix"
	Pipe Network = "pipe"
)

// Address is an RPC endpoint.
type Address struct {
	Network Network `json:"network"`
	Name    string  `json:"name"`
}

// Parse parses "tcp://host:port", "unix:///abs/path" and "pipe://name".
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
	case TCP:
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
		_ = host
	case Unix:
		if !strings.HasPrefix(name, "/") {
			return Address{}, Invalid("unix 地址必须是绝对路径: " + raw)
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

// String returns the canonical form.
func (a Address) String() string { return string(a.Network) + "://" + a.Name }

// Stream is a bidirectional byte stream. SetDeadline is best effort: named
// pipes return an error, and the protocol relies on ctx + Close there.
type Stream interface {
	io.ReadWriteCloser
	SetDeadline(t time.Time) error
}

// Transport is one transport implementation. The chat side is a client only:
// hosts dial the agent runtime, they never accept RPC connections.
type Transport interface {
	Dial(ctx context.Context, addr Address) (Stream, error)
}

// Event is a server-pushed event.
type Event struct {
	Topic string `json:"topic"`
	Seq   uint64 `json:"seq"`
	Data  []byte `json:"-"`
}

// Peer is a client connection.
type Peer interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error
	Subscribe(ctx context.Context, topics ...string) error
	Events() <-chan Event
	Remote() string
	Close() error
}

// Server is absent on purpose: the chat host is an RPC client. The desktop
// bridge in ada_go (cmd/ada-rpc-agent) owns the server half of the protocol.

// AuthMode selects how peers prove themselves.
type AuthMode string

const (
	AuthOS    AuthMode = "os"
	AuthToken AuthMode = "token"
	AuthNone  AuthMode = "none"
)

// Auth is authentication configuration. Token is supplied by the caller.
type Auth struct {
	Mode          AuthMode
	Token         []byte
	AllowInsecure bool
}

// DialOptions are client options.
type DialOptions struct {
	Client  string
	Auth    Auth
	Timeout time.Duration
}

// Service dials using registered transports.
type Service struct {
	transports map[Network]Transport
}

// NewService returns an empty service; register transports before use.
func NewService() *Service { return &Service{transports: map[Network]Transport{}} }

// Register adds a transport for n.
func (s *Service) Register(n Network, t Transport) error {
	if t == nil {
		return Invalid("rpc: 传输实现不能为 nil")
	}
	if n == "" {
		return Invalid("rpc: 传输类型不能为空")
	}
	if _, ok := s.transports[n]; ok {
		return Invalid("rpc: 传输已注册: " + string(n))
	}
	s.transports[n] = t
	return nil
}

// Transport returns the transport registered for n.
func (s *Service) Transport(n Network) (Transport, bool) {
	t, ok := s.transports[n]
	return t, ok
}
