package main

import (
	"os"
	"syscall"
)

func openHarnessTrustInput(path string) (*os.File, error) { return os.Open(path) }

func harnessTrustInputSafe(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes != nil && attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0
}
