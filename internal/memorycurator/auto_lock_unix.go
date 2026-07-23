//go:build !windows

package memorycurator

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openAutoLockFileNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errAutoLockFileHandle
	}
	return file, nil
}

func tryLockAutoFile(file *os.File) (bool, error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func unlockAutoFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
