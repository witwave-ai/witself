//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"syscall"
	"testing"
)

// lcaPrivateDir and lcaPrivateFile create objects the local cell accepts as private.
func lcaPrivateDir(path string) error { return os.Mkdir(path, 0o700) }

func lcaPrivateFile(path string, data []byte) error { return os.WriteFile(path, data, 0o600) }

// lcaUnreadable makes name unreadable through the released route and returns
// the release, which a cleanup runs too. On macOS a deny-read ACL entry for the
// current user does it: the layout check sees only the owner and the mode
// bits, which stay as they were, and the open fails at once. Elsewhere it
// returns nil: a mode that refuses the open is refused first
// (descendant_refused) and a Linux lease is waited out for 60 seconds; the
// Linux proof is TestCheckpointWitnessLease/kept in internal/localcell.
func lcaUnreadable(t *testing.T, name string) func() {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return nil
	}
	current, err := user.Current()
	before, statErr := os.Lstat(name)
	if err != nil || statErr != nil {
		t.Fatal("cannot prepare the deny read entry for the witness")
	}
	released := false
	release := func() {
		if !released && exec.Command("/bin/chmod", "-N", name).Run() != nil {
			t.Error("cannot remove the deny read entry from the witness")
			return
		}
		released = true
	}
	t.Cleanup(release)
	if exec.Command("/bin/chmod", "+a", "user:"+current.Username+" deny read", name).Run() != nil {
		t.Fatal("cannot add the deny read entry to the witness")
	}
	if after, err := os.Lstat(name); err != nil || after.Mode() != before.Mode() {
		t.Fatal("the deny read entry changed the witness mode bits")
	}
	if f, err := os.Open(name); !errors.Is(err, os.ErrPermission) {
		_ = f.Close()
		t.Fatal("the deny read entry did not make the witness unopenable")
	}
	return release
}

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
