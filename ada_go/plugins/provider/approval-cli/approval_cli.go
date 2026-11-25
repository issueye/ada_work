// Package approvalcli 是 `provider/approval-cli`:命令行交互式审批人。
//
// fail-closed:EOF、非 y/yes 输入、超时都拒绝。审批串行化(一次只处理一个请求),
// 避免多个插件同时弹窗互相覆盖。
package approvalcli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/issueye/ada_go_v2/platform/kernel"
	"github.com/issueye/ada_go_v2/plugins/seam/approval"
)

// ProviderName 是插件 id。
const ProviderName = "approval-cli"

// Name 是审批人名字。
const Name = "cli"

// Approver 从命令行读 y/n。
type Approver struct {
	reader *bufio.Reader
	out    io.Writer

	mu sync.Mutex
}

// NewApprover 构造审批人(测试/嵌入可注入 reader 与 writer)。
func NewApprover(r io.Reader, out io.Writer) *Approver {
	return &Approver{reader: bufio.NewReader(r), out: out}
}

// Name 实现 approval.Approver。
func (a *Approver) Name() string { return Name }

// Approve 展示请求并读取 yes/no;只接受 y/yes(大小写不敏感)。
func (a *Approver) Approve(ctx context.Context, req approval.Request) (approval.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fmt.Fprintf(a.out, "审批请求: %s %s %s (%s)\n允许吗? [y/N] ", req.Source, req.Op, approval.RedactTarget(req.Target), req.Reason)

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := a.reader.ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(a.out, "\n(审批超时)")
		return approval.Decision{Allow: false, Reason: "审批超时", Approver: Name}, ctx.Err()
	case rr := <-ch:
		if rr.err != nil && rr.line == "" {
			fmt.Fprintln(a.out, "(没有输入)")
			return approval.Decision{Allow: false, Reason: "没有输入", Approver: Name}, nil
		}
		answer := strings.ToLower(strings.TrimSpace(rr.line))
		if answer == "y" || answer == "yes" {
			return approval.Decision{Allow: true, Approver: Name}, nil
		}
		return approval.Decision{Allow: false, Reason: "用户拒绝", Approver: Name}, nil
	}
}

// PluginWithIO 返回使用指定 IO 的 approval-cli 插件。
func PluginWithIO(r io.Reader, out io.Writer) kernel.Plugin {
	return kernel.Func(ProviderName, func(ctx *kernel.Context, _ kernel.Config) (kernel.Dispose, error) {
		reg, err := kernel.Service[*approval.Registry](ctx, "approval")
		if err != nil {
			return nil, err
		}
		return reg.Register(NewApprover(r, out))
	}).WithInject("approval")
}

// Plugin 返回默认用 stdin/stdout 的 approval-cli 插件。
func Plugin() kernel.Plugin {
	return PluginWithIO(os.Stdin, os.Stdout)
}
