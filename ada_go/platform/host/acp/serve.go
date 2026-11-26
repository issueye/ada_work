// Package hostacp 是 ACP 的宿主入口:把已装配的 app 接到 stdin/stdout 上。
//
// 它只做平台映射:取 control 服务、构造 JSON-RPC 连接、绑定 approval 与 seam/acp;
// 协议翻译在 provider/control-acp。
package hostacp

import (
	"context"
	"fmt"
	"io"

	"github.com/issueye/ada_go_v2/platform/kernel"
	controlacp "github.com/issueye/ada_go_v2/plugins/provider/control-acp"
	"github.com/issueye/ada_go_v2/plugins/provider/jsonrpc2"
	seamacp "github.com/issueye/ada_go_v2/plugins/seam/acp"
	seamcontrol "github.com/issueye/ada_go_v2/plugins/seam/control"
)

// Serve 在 in/out 上跑 ACP,直到对端关闭输入或 ctx 取消。
//
// stdout 只写协议消息;调用方负责把诊断写到 stderr。
func Serve(ctx context.Context, app *kernel.App, in io.Reader, out io.Writer) error {
	raw, err := app.Root().Service(seamcontrol.ServiceName)
	if err != nil {
		return fmt.Errorf("没有 control 服务(需要在 preset 里启用 agent-control): %w", err)
	}
	inv, ok := raw.(seamcontrol.Invoker)
	if !ok {
		return fmt.Errorf("control 服务类型不正确: %T", raw)
	}

	conn := jsonrpc2.New(ctx, &stdioRWC{in: in, out: out})
	ep := controlacp.New(conn, seamcontrol.New(inv))
	defer func() { _ = ep.Close() }()
	disposeUpdates := ep.BindUpdates(app.Root())
	defer func() { _ = disposeUpdates(ctx) }()

	// 有 approval 服务时把权限交给 ACP client;没有就保持「未实现」,不静默放行。
	if _, ok := app.Root().TryService("approval"); ok {
		disposeApproval, err := ep.BindApproval(app.Root())
		if err != nil {
			return fmt.Errorf("注册 ACP 审批人失败: %w", err)
		}
		defer func() { _ = disposeApproval(ctx) }()
	}
	// 有 seam/acp 时把当前连接暴露成服务,供工具/插件按会话调用编辑器能力。
	if raw, ok := app.Root().TryService(seamacp.ServiceName); ok {
		reg, ok := raw.(*seamacp.Registry)
		if !ok {
			return fmt.Errorf("acp 服务类型不正确: %T", raw)
		}
		dispose, err := reg.Set(ep)
		if err != nil {
			return fmt.Errorf("绑定 ACP client 能力失败: %w", err)
		}
		defer func() { _ = dispose(ctx) }()
	}

	<-conn.Done()
	return nil
}

// stdioRWC 把 stdin/stdout 合成双向流;Close 不关闭真实 stdio,
// 所有权仍属于宿主进程。
type stdioRWC struct {
	in  io.Reader
	out io.Writer
}

func (s *stdioRWC) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *stdioRWC) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *stdioRWC) Close() error                { return nil }
