//go:build !windows

package rpcpipe

import (
	"context"
	"testing"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

func TestPipeUnsupported(t *testing.T) {
	tr := newTransport()
	if _, err := tr.Dial(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: "x"}); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("非 Windows 拨号应 EUNIMPLEMENTED: %v", err)
	}
	if _, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: "x"}, seamrpc.ListenOptions{}); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("非 Windows 监听应 EUNIMPLEMENTED: %v", err)
	}
}
