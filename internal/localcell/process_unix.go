//go:build darwin || linux

package localcell

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func signalTerminate(pid int) error {
	if pid <= 0 {
		return errors.New("localcell: invalid process ID")
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

func configureProcessCommand(*exec.Cmd) {}

func privateOwnedDir(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm() == 0o700
}
