//go:build !windows

package transcriptcapture

import (
	"os"
	"syscall"
)

// trustedPathIdentity preserves the Unix owner-only path contract used for
// native transcript reads and pending-event rewrites.
func trustedPathIdentity(path string, info os.FileInfo) bool {
	linked, err := os.Lstat(path)
	if err != nil || linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, linked) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o022 == 0
}

// processRunning uses signal zero only as a liveness probe. Permission denial
// proves that the process exists, matching the previous inline behavior.
func processRunning(pid int) (running, known bool) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, true
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || os.IsPermission(err), true
}
