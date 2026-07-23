//go:build !windows

package local

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openLocalLockFileNoFollow(path string, create bool) (*os.File, bool, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	created := false
	var fd int
	var err error
	if create {
		fd, err = unix.Open(path, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
		created = err == nil
		if errors.Is(err, unix.EEXIST) {
			fd, err = unix.Open(path, flags, 0)
		}
	} else {
		fd, err = unix.Open(path, flags, 0)
	}
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errLocalLockFileStorage
	}
	return file, created, nil
}

func lockLocalFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockLocalFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
