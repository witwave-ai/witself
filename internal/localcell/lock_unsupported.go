//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package localcell

import (
	"context"
	"errors"
	"os"
)

func openLockFile(context.Context, string, bool) (*os.File, error) {
	return nil, errors.New("localcell: advisory locks are unsupported on this platform")
}

func lockFile(*os.File) error {
	return errors.New("localcell: advisory locks are unsupported on this platform")
}

func unlockFile(*os.File) error { return nil }
