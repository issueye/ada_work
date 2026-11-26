package acp_test

import (
	"context"
	"testing"

	acpseam "github.com/issueye/ada_go_v2/plugins/seam/acp"
	"github.com/issueye/ada_go_v2/plugins/seam/seamerr"
)

type fakeClient struct{ caps acpseam.Capabilities }

func (f fakeClient) Capabilities() acpseam.Capabilities { return f.caps }

func (fakeClient) ReadTextFile(context.Context, string, string, int, int) (string, error) {
	return "", nil
}

func (fakeClient) WriteTextFile(context.Context, string, string, string) error { return nil }

func (fakeClient) CreateTerminal(context.Context, string, acpseam.TerminalRequest) (string, error) {
	return "", nil
}

func (fakeClient) TerminalOutput(context.Context, string, string) (acpseam.TerminalOutput, error) {
	return acpseam.TerminalOutput{}, nil
}

func (fakeClient) WaitForExit(context.Context, string, string) (acpseam.ExitStatus, error) {
	return acpseam.ExitStatus{}, nil
}

func (fakeClient) KillTerminal(context.Context, string, string) error    { return nil }
func (fakeClient) ReleaseTerminal(context.Context, string, string) error { return nil }

// 绑定/解绑语义:同一时刻只能有一个连接,解绑后回到 EUNIMPLEMENTED。
func TestRegistry(t *testing.T) {
	r := acpseam.NewRegistry()
	if r.Available() {
		t.Fatal("空注册表不应可用")
	}
	if _, err := r.Get(); !seamerr.Is(err, seamerr.ErrUnimplemented) {
		t.Fatalf("没有连接应 EUNIMPLEMENTED: %v", err)
	}
	if _, err := r.Set(nil); err == nil {
		t.Fatal("nil client 应拒绝")
	}

	c := fakeClient{caps: acpseam.Capabilities{ReadTextFile: true}}
	dispose, err := r.Set(c)
	if err != nil || !r.Available() {
		t.Fatalf("绑定失败: %v", err)
	}
	if _, err := r.Set(c); err == nil {
		t.Fatal("重复绑定应失败")
	}
	got, err := r.Get()
	if err != nil || !got.Capabilities().ReadTextFile {
		t.Fatalf("Get 不符: %+v err=%v", got, err)
	}
	if err := dispose(context.Background()); err != nil {
		t.Fatalf("解绑失败: %v", err)
	}
	if r.Available() {
		t.Fatal("解绑后不应可用")
	}
	// 二次解绑应幂等。
	if err := dispose(context.Background()); err != nil {
		t.Fatalf("二次解绑失败: %v", err)
	}
}
