//go:build !windows

package localcell

import (
	"os"
	"os/exec"
	"syscall"
)

// checkpointSignalPrepare leaves the child as it is: Unix rows signal it alone.
func checkpointSignalPrepare(*exec.Cmd) {}

// checkpointSignalSend sends the row's signal: SIGINT, SIGTERM or SIGHUP.
func checkpointSignalSend(process *os.Process, name string) error {
	return process.Signal(map[string]syscall.Signal{"interrupt": syscall.SIGINT, "terminate": syscall.SIGTERM, "hangup": syscall.SIGHUP}[name])
}
