//go:build windows

package rpcpipe

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type transport struct{}

func newTransport() seamrpc.Transport { return transport{} }

var errPipeClosed = errors.New("rpc-pipe: 连接已关闭")

// fileStream performs overlapped read/write with one event per operation. It
// deliberately does not wrap the handle in os.File: a pending read on a
// synchronous handle blocks writes on the same handle, which makes an RPC
// client (read loop + call write) deadlock, and concurrent use of os.File also
// races on its internal offset.
type fileStream struct {
	h windows.Handle

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newStream(h windows.Handle) *fileStream { return &fileStream{h: h} }

func (s *fileStream) Read(p []byte) (int, error)  { return s.io(true, p) }
func (s *fileStream) Write(p []byte) (int, error) { return s.io(false, p) }

func (s *fileStream) io(read bool, p []byte) (int, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return 0, errPipeClosed
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ev)
	var n uint32
	var ov windows.Overlapped
	ov.HEvent = ev
	if read {
		err = windows.ReadFile(s.h, p, &n, &ov)
	} else {
		err = windows.WriteFile(s.h, p, &n, &ov)
	}
	runtime.KeepAlive(p)
	if err == nil {
		if read && n == 0 {
			return 0, io.EOF
		}
		return int(n), nil
	}
	if !errors.Is(err, windows.ERROR_IO_PENDING) {
		return 0, pipeIOError(err, read)
	}
	if _, werr := windows.WaitForSingleObject(ev, windows.INFINITE); werr != nil {
		return 0, pipeIOError(werr, read)
	}
	var done uint32
	if gerr := windows.GetOverlappedResult(s.h, &ov, &done, false); gerr != nil {
		return 0, pipeIOError(gerr, read)
	}
	if read && done == 0 {
		return 0, io.EOF
	}
	return int(done), nil
}

func (s *fileStream) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	s.mu.Unlock()
	_ = windows.CancelIoEx(s.h, nil)
	s.wg.Wait()
	return windows.CloseHandle(s.h)
}

func (s *fileStream) SetDeadline(time.Time) error {
	return errors.New("rpc-pipe: 命名管道不支持 deadline")
}

func pipeIOError(err error, read bool) error {
	if errors.Is(err, windows.ERROR_OPERATION_ABORTED) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return errPipeClosed
	}
	if read && errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		return io.EOF
	}
	return err
}

func (transport) Dial(ctx context.Context, addr seamrpc.Address) (seamrpc.Stream, error) {
	select {
	case <-ctx.Done():
		return nil, seamerr.New(seamerr.ErrCanceled, "rpc-pipe", "拨号已取消", false)
	default:
	}
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(fullName(addr.Name)),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0,
	)
	if err != nil {
		return nil, seamrpc.Unavailable("打开命名管道失败: " + err.Error())
	}
	return newStream(h), nil
}

func (transport) Listen(ctx context.Context, addr seamrpc.Address, _ seamrpc.ListenOptions) (seamrpc.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, seamerr.New(seamerr.ErrCanceled, "rpc-pipe", "监听已取消", false)
	}
	if addr.Name == "" || len(addr.Name) > 128 {
		return nil, seamrpc.Invalid("pipe 名字为空或过长")
	}
	sd, err := pipeSecurity()
	if err != nil {
		return nil, seamrpc.Internal("构造管道 ACL 失败")
	}
	l := &pipeListener{
		name: fullName(addr.Name), sa: sd, first: true, addr: addr, closeCh: make(chan struct{}),
	}
	// 先建一个实例,避免「监听已返回但客户端还看不到管道」的窗口。
	h, err := l.instance()
	if err != nil {
		return nil, seamrpc.Unavailable("创建管道实例失败: " + err.Error())
	}
	l.next = h
	return l, nil
}

type pipeListener struct {
	name  string
	sa    *windows.SecurityAttributes
	addr  seamrpc.Address
	first bool

	mu        sync.Mutex
	next      windows.Handle
	pending   map[windows.Handle]struct{}
	closeCh   chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

func (l *pipeListener) Addr() seamrpc.Address { return l.addr }

func (l *pipeListener) Close() error {
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		close(l.closeCh)
		l.mu.Lock()
		if l.next != 0 {
			_ = windows.CloseHandle(l.next)
			l.next = 0
		}
		for h := range l.pending {
			_ = windows.CancelIoEx(h, nil)
			_ = windows.CloseHandle(h)
			delete(l.pending, h)
		}
		l.mu.Unlock()
	})
	return nil
}

func (l *pipeListener) Accept(ctx context.Context) (seamrpc.Stream, error) {
	if l.closed.Load() {
		return nil, seamrpc.Unavailable("管道监听器已关闭")
	}
	l.mu.Lock()
	h := l.next
	l.next = 0
	l.mu.Unlock()
	if h == 0 {
		var err error
		h, err = l.instance()
		if err != nil {
			return nil, seamrpc.Unavailable("创建管道实例失败: " + err.Error())
		}
	}
	l.track(h)
	if l.closed.Load() {
		l.untrack(h)
		_ = windows.CloseHandle(h)
		return nil, seamrpc.Unavailable("管道监听器已关闭")
	}
	done := make(chan error, 1)
	go func() { done <- connectNamedPipe(h) }()
	select {
	case err := <-done:
		l.untrack(h)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			_ = windows.CloseHandle(h)
			if l.closed.Load() {
				return nil, seamrpc.Unavailable("管道监听器已关闭")
			}
			return nil, seamrpc.Unavailable("等待客户端失败: " + err.Error())
		}
	case <-ctx.Done():
		// 自连接唤醒阻塞中的 ConnectNamedPipe,再关闭本实例。
		l.wake()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		l.untrack(h)
		_ = windows.CloseHandle(h)
		return nil, seamerr.New(seamerr.ErrCanceled, "rpc-pipe", "accept 已取消", false)
	case <-l.closeCh:
		_ = windows.CancelIoEx(h, nil)
		l.untrack(h)
		_ = windows.CloseHandle(h)
		return nil, seamrpc.Unavailable("管道监听器已关闭")
	}
	if ctx.Err() != nil || l.closed.Load() {
		l.untrack(h)
		_ = windows.CloseHandle(h)
		return nil, seamerr.New(seamerr.ErrCanceled, "rpc-pipe", "accept 已取消", false)
	}
	// 立刻补上下一个实例,让下一个客户端在 Accept 之前也能打开管道。
	l.refill()
	return newStream(h), nil
}

func (l *pipeListener) track(h windows.Handle) {
	l.mu.Lock()
	if l.pending == nil {
		l.pending = make(map[windows.Handle]struct{})
	}
	l.pending[h] = struct{}{}
	l.mu.Unlock()
}

func (l *pipeListener) untrack(h windows.Handle) {
	l.mu.Lock()
	delete(l.pending, h)
	l.mu.Unlock()
}

// refill 预建下一个实例;失败时留给下一次 Accept 重试。
func (l *pipeListener) refill() {
	if l.closed.Load() {
		return
	}
	h, err := l.instance()
	if err != nil {
		return
	}
	l.mu.Lock()
	if l.closed.Load() || l.next != 0 {
		l.mu.Unlock()
		_ = windows.CloseHandle(h)
		return
	}
	l.next = h
	l.mu.Unlock()
}

func (l *pipeListener) instance() (windows.Handle, error) {
	l.mu.Lock()
	first := l.first
	l.first = false
	l.mu.Unlock()
	flags := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		flags |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	mode := uint32(windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS)
	// The protocol caps connections at 64; the instance count must leave room
	// for the pre-created next instance while one connection is live.
	return windows.CreateNamedPipe(
		windows.StringToUTF16Ptr(l.name), flags, mode, 64, 4096, 4096, 0, l.sa,
	)
}

// connectNamedPipe waits for a client on an overlapped instance. Accept runs it
// in a goroutine so ctx/Close can self-connect to wake it.
func connectNamedPipe(h windows.Handle) error {
	ev, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ev)
	var ov windows.Overlapped
	ov.HEvent = ev
	err = windows.ConnectNamedPipe(h, &ov)
	switch err {
	case nil, windows.ERROR_PIPE_CONNECTED:
		return nil
	case windows.ERROR_IO_PENDING:
		if _, err := windows.WaitForSingleObject(ev, windows.INFINITE); err != nil {
			return err
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return err
		}
		return nil
	default:
		return err
	}
}

// wake 用一次短连接让阻塞的 ConnectNamedPipe 返回 ERROR_PIPE_CONNECTED。
func (l *pipeListener) wake() {
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(l.name),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, 0, 0,
	)
	if err == nil {
		_ = windows.CloseHandle(h)
	}
}

func fullName(name string) string {
	if len(name) >= 9 && name[:9] == `\\.\pipe\` {
		return name
	}
	return `\\.\pipe\` + name
}

// pipeSecurity 构造只授予当前用户与 SYSTEM 完全控制的 DACL。
func pipeSecurity() (*windows.SecurityAttributes, error) {
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:(A;;FA;;;SY)(A;;FA;;;" + user.User.Sid.String() + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}, nil
}
