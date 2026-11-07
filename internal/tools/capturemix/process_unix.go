//go:build !windows

package capturemix

import (
	"os"
	"syscall"
)

func pauseChild(p *os.Process) error  { return p.Signal(syscall.SIGSTOP) }
func resumeChild(p *os.Process) error { return p.Signal(syscall.SIGCONT) }
