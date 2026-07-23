//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openIntegrationOperationLockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("integration lock file handle is unavailable")
	}
	return file, nil
}

func validateIntegrationOperationLockIdentity(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Nlink != 1 {
		return errors.New("integration lock file must have exactly one hard link")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("integration lock file is owned by uid %d", stat.Uid)
	}
	return nil
}

func secureIntegrationOperationLockFile(file *os.File) error {
	return unix.Fchmod(int(file.Fd()), 0o600)
}

func tryIntegrationOperationLock(file *os.File) (bool, error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func unlockIntegrationOperationLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
