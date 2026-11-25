//go:build windows

package rpcnet

import "os"

// Windows 的 AF_UNIX 没有可移植的 UID 视图；权限由 ACL 负责。
func ownedByCurrentUser(os.FileInfo) bool { return true }
