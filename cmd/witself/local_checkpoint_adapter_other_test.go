//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// lcaPrivateDir and lcaPrivateFile create objects the local cell accepts as private.
func lcaPrivateDir(path string) error { return os.Mkdir(path, 0o700) }

func lcaPrivateFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// lcsNames is the signal rows' OS set: SIGINT, SIGTERM and SIGHUP.
var lcsNames = []string{"interrupt", "terminate", "hangup"}

func lcsSignal(name string) syscall.Signal {
	return map[string]syscall.Signal{"interrupt": syscall.SIGINT, "terminate": syscall.SIGTERM, "hangup": syscall.SIGHUP}[name]
}

// lcsStart gives the child the signal's default action even if this process
// inherited it ignored (nohup): Notify installs a handler, which exec resets.
// It is unconditional: after one Notify and Stop, Ignored no longer reports an
// inherited ignore that Stop has restored.
func lcsStart(child *exec.Cmd, name string) error {
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, lcsSignal(name))
	defer signal.Stop(caught)
	return child.Start()
}

func lcsSend(process *os.Process, name string) error { return process.Signal(lcsSignal(name)) }

// lcsEndedBy reports whether the child was ended by the row's signal.
func lcsEndedBy(state *os.ProcessState, name string) bool {
	status, ok := state.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == lcsSignal(name)
}
