//go:build windows

package rpc

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// RegisterPipe registers the Windows named-pipe dialer; the chat host is a
// client only, the listener half lives in ada_go's rpc-pipe provider.
func RegisterPipe(s *Service) error { return s.Register(Pipe, pipeTransport{}) }

type pipeTransport struct{}

var errPipeClosed = errors.New("rpc-pipe: 连接已关闭")

// pipeStream performs overlapped read/write with one event per operation.
// It deliberately does not wrap the handle in os.File: concurrent overlapped
// reads and writes on one synchronous os.File share an internal offset (and
// race), and a synchronous handle serializes a pending read with writes.
type pipeStream struct {
	h windows.Handle

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newPipeStream(h windows.Handle) *pipeStream { return &pipeStream{h: h} }

func (s *pipeStream) Read(p []byte) (int, error)  { return s.io(true, p) }
func (s *pipeStream) Write(p []byte) (int, error) { return s.io(false, p) }

func (s *pipeStream) io(read bool, p []byte) (int, error) {
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

func (s *pipeStream) Close() error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	s.mu.Unlock()
	// Cancel pending reads/writes, wait for their events, then close the handle.
	_ = windows.CancelIoEx(s.h, nil)
	s.wg.Wait()
	return windows.CloseHandle(s.h)
}

func (s *pipeStream) SetDeadline(time.Time) error {
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

func (pipeTransport) Dial(ctx context.Context, addr Address) (Stream, error) {
	select {
	case <-ctx.Done():
		return nil, New(ErrCanceled, "rpc-pipe", "拨号已取消", false)
	default:
	}
	h, err := windows.CreateFile(
		windows.StringToUTF16Ptr(fullPipeName(addr.Name)),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0,
	)
	if err != nil {
		return nil, Unavailable("打开命名管道失败: " + err.Error())
	}
	return newPipeStream(h), nil
}

func fullPipeName(name string) string {
	if len(name) >= 9 && name[:9] == `\\.\pipe\` {
		return name
	}
	return `\\.\pipe\` + name
}
