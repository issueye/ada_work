// Package rpcnet 是 `provider/rpc-net`:tcp 与 unix 传输。
//
// 只做字节流与监听,不含协议语义、地址允许表或审批。
package rpcnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

// ProviderName 是插件 id。
const ProviderName = "rpc-net"

type transport struct{}

func (transport) Dial(ctx context.Context, addr seamrpc.Address) (seamrpc.Stream, error) {
	if addr.Network != seamrpc.TCP && addr.Network != seamrpc.Unix {
		return nil, seamrpc.Unimplemented("rpc-net 不支持传输 " + string(addr.Network))
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, string(addr.Network), addr.Name)
	if err != nil {
		return nil, seamrpc.Unavailable("拨号 " + addr.String() + " 失败: " + err.Error())
	}
	return c, nil
}

func (transport) Listen(ctx context.Context, addr seamrpc.Address, opts seamrpc.ListenOptions) (seamrpc.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.New(seamerr.ErrCanceled, "rpc-net", "监听已取消", false)
	}
	if addr.Network != seamrpc.TCP && addr.Network != seamrpc.Unix {
		return nil, seamrpc.Unimplemented("rpc-net 不支持传输 " + string(addr.Network))
	}
	path := ""
	if addr.Network == seamrpc.Unix {
		if err := prepareUnix(addr.Name, opts); err != nil {
			return nil, err
		}
		path = addr.Name
	}
	ln, err := net.Listen(string(addr.Network), addr.Name)
	if err != nil {
		return nil, seamrpc.Unavailable("监听 " + addr.String() + " 失败: " + err.Error())
	}
	if path != "" && runtime.GOOS != "windows" {
		perm := opts.Perm
		if perm == 0 {
			perm = 0o600
		}
		if err := os.Chmod(path, perm); err != nil {
			_ = ln.Close()
			_ = os.Remove(path)
			return nil, seamrpc.Unavailable("设置 socket 权限失败: " + err.Error())
		}
	}
	actual := addr
	if addr.Network == seamrpc.TCP {
		actual = seamrpc.Address{Network: seamrpc.TCP, Name: ln.Addr().String()}
	}
	return &listener{ln: ln, addr: actual, path: path}, nil
}

// prepareUnix 检查父目录与 stale socket。
//
// 已存在路径默认拒绝,避免误删别人的 socket;RemoveStale 只删确认是 socket 的文件。
func prepareUnix(path string, opts seamrpc.ListenOptions) error {
	dir := dirOf(path)
	info, err := os.Stat(dir)
	if err != nil {
		return seamrpc.Invalid("unix socket 目录不存在: " + dir)
	}
	if !info.IsDir() {
		return seamrpc.Invalid("unix socket 父路径不是目录: " + dir)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return seamrpc.Denied("unix socket 目录权限必须 ≤ 0700: " + dir)
	}
	if li, err := os.Lstat(path); err == nil {
		if li.Mode()&os.ModeSymlink != 0 || li.Mode()&os.ModeSocket == 0 {
			return seamrpc.Invalid("路径已存在且不是 socket: " + path)
		}
		if !opts.RemoveStale {
			return seamrpc.Invalid("socket 已存在(未允许清理): " + path)
		}
		if err := os.Remove(path); err != nil {
			return seamrpc.Unavailable("清理 stale socket 失败: " + err.Error())
		}
	} else if !os.IsNotExist(err) {
		return seamrpc.Unavailable("检查 socket 失败: " + err.Error())
	}
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			if i == 0 {
				return string(path[0])
			}
			return path[:i]
		}
	}
	return "."
}

type listener struct {
	ln   net.Listener
	addr seamrpc.Address
	path string

	closeOnce sync.Once
}

func (l *listener) Accept(ctx context.Context) (seamrpc.Stream, error) {
	dl, ok := l.ln.(interface{ SetDeadline(time.Time) error })
	if !ok {
		return nil, seamrpc.Internal("监听器不支持 deadline")
	}
	stop := context.AfterFunc(ctx, func() { _ = dl.SetDeadline(time.Now()) })
	c, err := l.ln.Accept()
	stop()
	_ = dl.SetDeadline(time.Time{})
	if err != nil {
		if ctx.Err() != nil {
			return nil, seamerr.New(seamerr.ErrCanceled, "rpc-net", "accept 已取消", false)
		}
		return nil, err
	}
	return c, nil
}

func (l *listener) Addr() seamrpc.Address { return l.addr }

func (l *listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		err = l.ln.Close()
		if l.path != "" {
			_ = os.Remove(l.path)
		}
	})
	if err != nil {
		return fmt.Errorf("rpc-net: 关闭监听器失败: %w", err)
	}
	return nil
}

const pluginID = ProviderName

// Plugin 返回 rpc-net 插件:注册 tcp 与 unix 两种传输。
func Plugin() kernel.Plugin {
	return kernel.Func(pluginID, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*seamrpc.Registry](ctx, "rpc-transport")
		if err != nil {
			return nil, err
		}
		t := transport{}
		d1, err := reg.Register(seamrpc.TCP, t)
		if err != nil {
			return nil, err
		}
		d2, err := reg.Register(seamrpc.Unix, t)
		if err != nil {
			_ = d1(ctx.Context())
			return nil, err
		}
		return func(c context.Context) error {
			_ = d2(c)
			return d1(c)
		}, nil
	}).WithInject("rpc-transport")
}
