package jsonrpc2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func pipePair(t *testing.T, optsB ...Option) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return New(ctx, a), New(ctx, b, optsB...)
}

func TestCallAndBidirectional(t *testing.T) {
	a, b := pipePair(t)
	b.Handle("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		return map[string]string{"text": in.Text}, nil
	})
	a.Handle("ping", func(context.Context, json.RawMessage) (any, error) { return "pong", nil })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out struct {
		Text string `json:"text"`
	}
	if err := a.Call(ctx, "echo", map[string]string{"text": "hi"}, &out); err != nil {
		t.Fatalf("call 失败: %v", err)
	}
	if out.Text != "hi" {
		t.Fatalf("结果不符: %+v", out)
	}

	var pong string
	if err := b.Call(ctx, "ping", nil, &pong); err != nil || pong != "pong" {
		t.Fatalf("反向调用失败: %q err=%v", pong, err)
	}
}

func TestNotification(t *testing.T) {
	got := make(chan string, 1)
	a, b := pipePair(t, WithNotificationHandler(func(m Message) { got <- m.Method }))
	_ = b
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Notify(ctx, "note", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("notify 失败: %v", err)
	}
	select {
	case method := <-got:
		if method != "note" {
			t.Fatalf("通知方法不符: %s", method)
		}
	case <-ctx.Done():
		t.Fatal("没有收到通知")
	}
}

func TestUnknownMethod(t *testing.T) {
	a, _ := pipePair(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := a.Call(ctx, "missing", nil, nil)
	var rerr *Error
	if !errors.As(err, &rerr) || rerr.Code != CodeMethodNotFound {
		t.Fatalf("未知方法应返回 -32601，实际 %v", err)
	}
}

func TestReadMessageLimit(t *testing.T) {
	br := bufio.NewReader(bytes.NewBufferString(`{"jsonrpc":"2.0","method":"x"}` + "\n"))
	if _, err := readMessage(br, 8); err == nil {
		t.Fatal("超限应失败")
	}
	br = bufio.NewReader(bytes.NewBufferString(`{"jsonrpc":"2.0","method":"x"}` + "\n"))
	msg, err := readMessage(br, 128)
	if err != nil || msg.Method != "x" {
		t.Fatalf("正常读取失败: %+v %v", msg, err)
	}
}
