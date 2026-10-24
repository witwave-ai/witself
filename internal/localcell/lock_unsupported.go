//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package localcell

import (
	"errors"
	"os"
)

func openLockFile(string, bool) (*os.File, error) {
	return nil, errors.New("localcell: advisory locks are unsupported on this platform")
}

func lockFile(*os.File) error {
	return errors.New("localcell: advisory locks are unsupported on this platform")
}

func unlockFile(*os.File) error { return nil }
