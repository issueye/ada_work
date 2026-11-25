package approvalcli_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/issueye/ada_go_v2/platform/kernel"
	approvalcli "github.com/issueye/ada_go_v2/plugins/provider/approval-cli"
	seamapproval "github.com/issueye/ada_go_v2/plugins/seam/approval"
)

func approve(t *testing.T, input string) seamapproval.Decision {
	t.Helper()
	var out bytes.Buffer
	a := approvalcli.NewApprover(strings.NewReader(input), &out)
	dec, err := a.Approve(context.Background(), seamapproval.Request{ID: "r1", Source: "fs", Op: "write", Target: "/workspace/a"})
	if err != nil {
		t.Fatalf("审批失败: %v", err)
	}
	return dec
}

// 1. y / yes 放行。
func TestAllow(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n"} {
		if dec := approve(t, input); !dec.Allow || dec.Approver != "cli" {
			t.Fatalf("输入 %q 应放行: %+v", input, dec)
		}
	}
}

// 2. n / 其它输入拒绝。
func TestDeny(t *testing.T) {
	for _, input := range []string{"n\n", "no\n", "maybe\n"} {
		if dec := approve(t, input); dec.Allow {
			t.Fatalf("输入 %q 应拒绝: %+v", input, dec)
		}
	}
}

// 3. EOF(没有输入)拒绝。
func TestEOFDeny(t *testing.T) {
	if dec := approve(t, ""); dec.Allow || dec.Reason != "没有输入" {
		t.Fatalf("EOF 应拒绝: %+v", dec)
	}
}

// 4. 超时拒绝并返回 ctx 错误。
func TestTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	a := approvalcli.NewApprover(pr, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	dec, err := a.Approve(ctx, seamapproval.Request{ID: "r1"})
	if err == nil {
		t.Fatalf("超时应返回错误")
	}
	if dec.Allow {
		t.Fatalf("超时应拒绝: %+v", dec)
	}
}

// 5.1 提示中的 URL target 去掉 query/userinfo。
func TestPromptRedacted(t *testing.T) {
	var out bytes.Buffer
	a := approvalcli.NewApprover(strings.NewReader("n\n"), &out)
	if _, err := a.Approve(context.Background(), seamapproval.Request{
		ID: "r1", Source: "net", Op: "GET",
		Target: "https://user:pass@example.com/x?token=SECRET",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(out.String(), "pass@") {
		t.Fatalf("审批提示未脱敏: %s", out.String())
	}
	if !strings.Contains(out.String(), "REDACTED") {
		t.Fatalf("审批提示缺少 REDACTED: %s", out.String())
	}
}

// 6. 插件注册到 approval 服务。
func TestPluginRegistration(t *testing.T) {
	app := kernel.New(kernel.Options{})
	t.Cleanup(func() { _ = app.Stop(context.Background()) })
	if _, err := app.Plugin(seamapproval.Plugin(), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 seam/approval 失败: %v", err)
	}
	if _, err := app.Plugin(approvalcli.PluginWithIO(strings.NewReader("y\n"), io.Discard), kernel.NewConfig(nil)); err != nil {
		t.Fatalf("挂载 approval-cli 失败: %v", err)
	}
	reg, err := kernel.Service[*seamapproval.Registry](app.Root(), "approval")
	if err != nil {
		t.Fatalf("取服务失败: %v", err)
	}
	names := reg.Names()
	if len(names) != 1 || names[0] != "cli" {
		t.Fatalf("审批人未注册: %v", names)
	}
}
