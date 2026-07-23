//go:build windows

package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRegistryClaimLockPreventsPathReplacementOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.lock")
	unlock, err := lockRegistryClaim(path)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer unlock()

	if err := os.Rename(path, path+".moved"); err == nil {
		t.Fatal("locked registry path was renameable despite stable-handle contract")
	}
}

func TestPIDRunningRecognizesExitedProcessOnWindows(t *testing.T) {
	shell := os.Getenv("ComSpec")
	if shell == "" {
		shell = "cmd.exe"
	}
	command := exec.Command(shell, "/d", "/c", "exit", "0")
	if err := command.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := command.Process.Pid
	if err := command.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	if running, known := pidRunning(pid); running || !known {
		t.Fatalf("exited pid %d: running=%v known=%v, want false true", pid, running, known)
	}
}
