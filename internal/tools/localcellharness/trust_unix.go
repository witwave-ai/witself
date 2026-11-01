//go:build !windows

package main

import (
	"os"
	"syscall"
)

func openHarnessTrustInput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
}

func harnessTrustInputSafe(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0
}
