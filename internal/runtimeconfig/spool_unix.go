//go:build !windows

package runtimeconfig

import (
	"os"
	"syscall"
)

func privateSpoolOwner(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}
