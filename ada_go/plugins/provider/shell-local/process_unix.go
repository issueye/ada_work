//go:build !windows

package shelllocal

import (
	"os/exec"
	"syscall"
)

// configure 让子进程进入独立进程组,便于超时时杀掉整棵树。
func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// afterStart 在 POSIX 上无需额外动作(进程组已在 configure 里设置)。
func afterStart(*exec.Cmd) error { return nil }

// cleanup 在 POSIX 上无需额外动作。
func cleanup(*exec.Cmd) {}

// terminate 终止整个进程组(POSIX)。
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
