package localcell

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOperationLockHelperProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "localcell-lock-helper" {
		return
	}
	mode, root := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	lock, err := acquireLock(root, true)
	if mode == "contend" {
		if !errors.Is(err, ErrLocked) || err.Error() != "localcell: another operation owns this cell" {
			if lock != nil {
				_ = lock.Close()
			}
			t.Fatal("contender did not fail with the explicit ownership error")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	_, _ = fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestOperationLockAcrossProcessesAndCrashRelease(t *testing.T) {
	root := testPrivateRoot(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holder := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOperationLockHelperProcess$", "--", "localcell-lock-helper", "hold", root)
	holder.Env = []string{}
	input, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	output, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("holder did not acquire lock: %v", err)
	}
	contenderCtx, contenderCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer contenderCancel()
	contender := exec.CommandContext(contenderCtx, os.Args[0], "-test.run=^TestOperationLockHelperProcess$", "--", "localcell-lock-helper", "contend", root)
	contender.Env = []string{}
	if err := contender.Run(); err != nil {
		t.Fatalf("concurrent operation did not fail fast with ErrLocked: %v", err)
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	lock, err := acquireLock(root, true)
	if err != nil {
		t.Fatalf("OS did not release crashed holder's lock: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockProtectsRunRemoval(t *testing.T) {
	root := testPrivateRoot(t)
	lock, err := acquireLock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := os.RemoveAll(filepath.Join(root, "run")); err != nil {
		t.Fatal(err)
	}
	other, err := acquireLock(root, true)
	if other != nil {
		_ = other.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("removing run bypassed stable ownership: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("contender mutated run before acquiring ownership")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	other, err = acquireLock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockReadOnlyAndModes(t *testing.T) {
	root := testPrivateRoot(t)
	lock, err := acquireLock(root, false)
	if err != nil || lock != nil {
		t.Fatalf("empty read-only probe: lock=%t err=%v", lock != nil, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("read-only lock probe created filesystem state")
	}
	lock, err = acquireLock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".owner.lock", "run", "run/operation.lock"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if name == "run" {
			want = 0o700
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != want {
			t.Fatalf("%s permissions = %o, want %o", name, info.Mode().Perm(), want)
		}
	}
	lock, err = acquireLock(root, false)
	if err != nil || lock == nil {
		t.Fatalf("existing read-only lock probe: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLockRefusesSymlinks(t *testing.T) {
	for _, target := range []string{"root", "run", ".owner.lock", "run/operation.lock"} {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			root, outside := testPrivateRoot(t), t.TempDir()
			path := filepath.Join(root, target)
			if target == "root" {
				path = filepath.Join(root, "linked-root")
				root = path
			} else if strings.HasSuffix(target, ".lock") {
				if err := testWriteFile(filepath.Join(outside, "target"), []byte("unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
				outside = filepath.Join(outside, "target")
			}
			if target == "run/operation.lock" {
				if err := testMkdir(filepath.Join(root, "run"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, path); err != nil {
				if runtime.GOOS == "windows" {
					t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable")
				}
				t.Fatal(err)
			}
			lock, err := acquireLock(root, true)
			if lock != nil {
				_ = lock.Close()
			}
			if err == nil {
				t.Fatal("lock followed a symlink")
			}
		})
	}
}
