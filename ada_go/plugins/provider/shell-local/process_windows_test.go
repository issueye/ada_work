//go:build windows

package shelllocal

import (
	"os/exec"
	"testing"
)

// Job Object 分配成功后,terminate 能终止进程并清理句柄。
func TestJobObjectTerminate(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "Start-Sleep -Seconds 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if err := afterStart(cmd); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Job Object 分配失败: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	if _, ok := jobs.Load(pid); !ok {
		t.Fatalf("Job 句柄未登记")
	}
	terminate(cmd)
	cleanup(cmd)
	if _, ok := jobs.Load(pid); ok {
		t.Fatalf("Job 句柄未清理")
	}
	_ = cmd.Wait()
}
