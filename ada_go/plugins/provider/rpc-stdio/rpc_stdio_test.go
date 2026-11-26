package rpcstdio

import (
	"context"
	"io"
	"testing"
	"time"

	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

func TestStdioRoundTrip(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()

	tr := New(inR, outW)
	ln, err := tr.Listen(context.Background(), seamrpc.Address{Network: seamrpc.Stdio, Name: "local"}, seamrpc.ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stream, err := ln.Accept(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	go func() { _, _ = inW.Write([]byte("hi")) }()
	buf := make([]byte, 2)
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("读 stdio 失败: %q err=%v", buf, err)
	}

	done := make(chan string, 1)
	go func() {
		b := make([]byte, 2)
		_, _ = io.ReadFull(outR, b)
		done <- string(b)
	}()
	if _, err := stream.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "ok" {
			t.Fatalf("写 stdio 失败: %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("写 stdio 超时")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := ln.Accept(ctx); err == nil {
		t.Fatal("第二次 Accept 应阻塞到 ctx 取消")
	}
	if err := stream.SetDeadline(time.Now()); err == nil {
		t.Fatal("stdio 不应声称支持 deadline")
	}
}
