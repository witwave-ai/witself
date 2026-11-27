//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openPasswordFile opens the password file for passwordConfig. O_NOFOLLOW closes the symlink race after the caller's
// Lstat, and O_NONBLOCK prevents a swapped special file from blocking before its descriptor can be checked.
func openPasswordFile(filename string) (*os.File, error) {
	return os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
