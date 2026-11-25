//go:build !windows

package rpc

import "context"

// RegisterPipe registers a placeholder pipe transport that reports
// EUNIMPLEMENTED on other platforms, as docs/seam-rpc.md requires.
func RegisterPipe(s *Service) error { return s.Register(Pipe, pipeUnsupported{}) }

type pipeUnsupported struct{}

func (pipeUnsupported) Dial(context.Context, Address) (Stream, error) {
	return nil, Unimplemented("rpc-pipe 只在 Windows 上可用")
}
