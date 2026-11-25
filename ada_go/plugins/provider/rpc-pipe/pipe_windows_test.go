//go:build windows

package rpcpipe

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

func TestPipeEcho(t *testing.T) {
	name := fmt.Sprintf("ada-rpc-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	tr := newTransport()
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: name}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	done := make(chan error, 1)
	go func() {
		c, aerr := ln.Accept(context.Background())
		if aerr != nil {
			done <- aerr
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		if _, rerr := io.ReadFull(c, buf); rerr != nil {
			done <- rerr
			return
		}
		_, werr := c.Write(buf)
		done <- werr
	}()

	c, err := tr.Dial(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: name})
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显不正确: %q", buf)
	}
	if err := <-done; err != nil {
		t.Fatalf("服务端失败: %v", err)
	}
}

func TestPipeAcceptCancel(t *testing.T) {
	name := fmt.Sprintf("ada-rpc-cancel-%d-%d", os.Getpid(), time.Now().UnixNano())
	tr := newTransport()
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Pipe, Name: name}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ln.Accept(ctx); !seamerr.Is(err, seamerr.ErrCanceled) {
		t.Fatalf("取消应 ECANCELED: %v", err)
	}
}
