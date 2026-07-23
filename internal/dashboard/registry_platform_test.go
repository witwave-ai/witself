package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryClaimLockBlocksUntilRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.lock")
	unlockFirst, err := lockRegistryClaim(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	firstReleased := false
	t.Cleanup(func() {
		if !firstReleased {
			unlockFirst()
		}
	})

	attempted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(attempted)
		unlockSecond, err := lockRegistryClaim(path)
		if err == nil {
			unlockSecond()
		}
		secondResult <- err
	}()
	<-attempted
	select {
	case err := <-secondResult:
		t.Fatalf("second lock returned before release: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	unlockFirst()
	firstReleased = true
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second lock after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second lock remained blocked after release")
	}
}

func TestRegistryClaimLockRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	path := filepath.Join(dir, "registry.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable on this host: %v", err)
	}
	unlock, err := lockRegistryClaim(path)
	if err == nil {
		unlock()
		t.Fatal("registry lock followed a symlink")
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != "foreign" {
		t.Fatalf("symlink target changed: raw=%q err=%v", raw, readErr)
	}
}

func TestPIDRunningRecognizesCurrentAndInvalidPIDs(t *testing.T) {
	if running, known := pidRunning(os.Getpid()); !running || !known {
		t.Fatalf("current pid: running=%v known=%v, want true true", running, known)
	}
	for _, pid := range []int{0, -1} {
		if running, known := pidRunning(pid); running || known {
			t.Fatalf("pid %d: running=%v known=%v, want false false", pid, running, known)
		}
	}
}
