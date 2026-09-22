//go:build !windows

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lockRegistryClaim takes the exclusive advisory lock guarding one agent's
// registry slot, blocking until it is free (the critical section is a bounded
// liveness probe plus one atomic write). The lock file persists after release;
// it holds no state.
func lockRegistryClaim(path string) (func(), error) {
	return lockRegistryClaimContext(context.Background(), path)
}

func lockRegistryClaimContext(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("dashboard: adopt registry claim lock %s", path)
	}
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := validateLockedRegistryFile(path, file); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

// pidRunning uses signal zero without disturbing the target. Permission
// errors still prove that the process exists; an unusable PID remains unknown.
func pidRunning(pid int) (running, known bool) {
	if pid <= 0 {
		return false, false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, true
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || os.IsPermission(err), true
}
