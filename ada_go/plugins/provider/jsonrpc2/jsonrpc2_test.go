package jsonrpc2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
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

func TestNotificationWithObserverAndHandler(t *testing.T) {
	// 观察器存在时,已注册的 Handler 仍必须收到通知(ACP session/cancel 是通知)。
	observed := make(chan Message, 1)
	a, b := pipePair(t, WithNotificationHandler(func(m Message) { observed <- m }))
	handled := make(chan json.RawMessage, 1)
	b.Handle("session/cancel", func(_ context.Context, params json.RawMessage) (any, error) {
		handled <- params
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Notify(ctx, "session/cancel", map[string]string{"sessionId": "s1"}); err != nil {
		t.Fatalf("notify 失败: %v", err)
	}
	select {
	case params := <-handled:
		if !bytes.Contains(params, []byte("s1")) {
			t.Fatalf("cancel 参数不符: %s", params)
		}
	case <-ctx.Done():
		t.Fatal("handler 被观察器吞掉了")
	}
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("观察器没有收到通知")
	}
}

func TestReadMessageSkipsBlankLines(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("\r\n\n" + `{"jsonrpc":"2.0","method":"x"}` + "\n"))
	msg, err := readMessage(br, 128)
	if err != nil || msg.Method != "x" {
		t.Fatalf("空行应被跳过: %+v %v", msg, err)
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

func TestHandlerContextCanceledOnClose(t *testing.T) {
	a, b := pipePair(t)
	got := make(chan context.Context, 1)
	b.Handle("wait", func(ctx context.Context, _ json.RawMessage) (any, error) {
		got <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	})
	go func() { _ = a.Call(context.Background(), "wait", nil, nil) }()

	var hctx context.Context
	select {
	case hctx = <-got:
	case <-time.After(time.Second):
		t.Fatal("handler 没有收到请求")
	}
	_ = b.Close()
	select {
	case <-hctx.Done():
	case <-time.After(time.Second):
		t.Fatal("连接关闭后 handler ctx 未取消")
	}
}

func TestDone(t *testing.T) {
	a, _ := pipePair(t)
	select {
	case <-a.Done():
		t.Fatal("未关闭时 Done 不应关闭")
	default:
	}
	_ = a.Close()
	select {
	case <-a.Done():
	case <-time.After(time.Second):
		t.Fatal("Close 后 Done 应关闭")
	}
}

// TestDrainsInFlightDispatchOnReadEnd 对端发完请求立即关闭输入(EOF)时,
// 服务端仍须把 in-flight handler 的响应写出,而不是被 EOF 抢先关闭连接丢掉响应。
func TestDrainsInFlightDispatchOnReadEnd(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"echo","params":{}}` + "\n"
	w := &scriptRWC{in: strings.NewReader(msg)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := New(ctx, w)
	conn.Handle("echo", func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("读侧 EOF 后连接应关闭")
	}
	got := w.out.String()
	if !strings.Contains(got, `"id":1`) || !strings.Contains(got, `"ok":true`) {
		t.Fatalf("响应在 EOF 前未写出: %q", got)
	}
}

type scriptRWC struct {
	in  io.Reader
	out bytes.Buffer
}

func (s *scriptRWC) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *scriptRWC) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *scriptRWC) Close() error                { return nil }

func TestNullResult(t *testing.T) {
	a, b := pipePair(t)
	b.Handle("nothing", func(context.Context, json.RawMessage) (any, error) { return nil, nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out any
	if err := a.Call(ctx, "nothing", nil, &out); err != nil {
		t.Fatalf("null 结果调用失败: %v", err)
	}
	if out != nil {
		t.Fatalf("null 结果应解码为 nil: %+v", out)
	}
}
