// Package controlrpc 是 control 与现有 seam/rpc 之间的 v1 适配器。
//
// 它不定义新协议,也不碰 rpc-json 的帧格式:
//   - Client 实现 control.Invoker,把 Invoke 映射成 rpc.Peer.Call;
//   - Register 把 control 方法注册到 rpc.Server,供 v1 对端调用。
//
// 这样现有 agent.session.* 消费者可以逐步切到控制层,而 wire 完全不变。
package controlrpc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/control"
	seamrpc "github.com/issueye/ada_go_v2/plugins/seam/rpc"
)

// Client 是 control.Invoker 的 RPC 客户端实现。
type Client struct {
	peer seamrpc.Peer
}

// New 包装一个 rpc.Peer。
func New(peer seamrpc.Peer) *Client { return &Client{peer: peer} }

// Invoke 实现 control.Invoker。
func (c *Client) Invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	if c == nil || c.peer == nil {
		return nil, control.Unimplemented("没有 rpc peer")
	}
	var out json.RawMessage
	if err := c.peer.Call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		out = json.RawMessage("null")
	}
	return out, nil
}

// Methods 返回 v1 适配器注册的完整方法集。
func Methods() []string {
	return []string{
		control.MethodSessionCreate,
		control.MethodSessionLoad,
		control.MethodSessionList,
		control.MethodSessionPrompt,
		control.MethodSessionHistory,
		control.MethodSessionCancel,
		control.MethodSessionPermission,
		control.MethodSessionClose,
	}
}

// Register 把控制层方法注册到 rpc.Server;返回的 Dispose 逐个注销。
func Register(srv seamrpc.Server, inv control.Invoker) ([]kernel.Dispose, error) {
	if srv == nil {
		return nil, fmt.Errorf("control-rpc: server 不能为 nil")
	}
	if inv == nil {
		return nil, fmt.Errorf("control-rpc: invoker 不能为 nil")
	}
	disposers := make([]kernel.Dispose, 0, len(Methods()))
	for _, method := range Methods() {
		m := method
		d, err := srv.Handle(m, func(ctx context.Context, params json.RawMessage) (any, error) {
			out, ierr := inv.Invoke(ctx, m, params)
			if ierr != nil {
				return nil, ierr
			}
			if len(out) == 0 {
				return json.RawMessage("null"), nil
			}
			return json.RawMessage(out), nil
		})
		if err != nil {
			for _, old := range disposers {
				_ = old(context.Background())
			}
			return nil, err
		}
		disposers = append(disposers, d)
	}
	return disposers, nil
}

// 编译期确认实现。
var _ control.Invoker = (*Client)(nil)
