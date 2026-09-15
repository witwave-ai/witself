//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func detachCaptureFlush(cmd *exec.Cmd) {
	// A new session also creates a process group and drops the controlling
	// terminal, so runtime process-group cleanup cannot reap the flusher.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
