//go:build !windows

package bot

import "os/exec"

func hideWindow(*exec.Cmd) {}
