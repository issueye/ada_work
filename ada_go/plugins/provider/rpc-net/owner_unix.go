//go:build !windows

package rpcnet

import (
	"os"
	"syscall"
)

// ownedByCurrentUser 用于 Unix socket 的 stale 清理与目录属主校验。
func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
