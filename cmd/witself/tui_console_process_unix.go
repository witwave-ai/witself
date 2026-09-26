//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// Only ESRCH proves absence; permission and unexpected errors never authorize removal.
func consolePIDDead(pid int) bool {
	return pid > 1 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
