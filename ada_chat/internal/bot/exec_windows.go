//go:build windows

package bot

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps the bot child console out of sight.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
