//go:build windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
)

var (
	errSpool          = errors.New("WITSELF_LOCAL_SPOOL_DIR is not private")
	errSpoolTemporary = errors.New("WITSELF_LOCAL_SPOOL_DIR is the temporary directory")
	errSpoolIdentity  = errors.New("WITSELF_LOCAL_SPOOL_DIR identity changed")
)

func privateSpoolTempPath() (string, error) { return filepath.EvalSymlinks(os.TempDir()) }

func privateSpoolFailure(err error) error {
	switch {
	case errors.Is(err, errSpoolTemporary):
		return errSpoolTemporary
	case errors.Is(err, errSpoolIdentity), errors.Is(err, errWindowsPrivatePathChanged):
		return errSpoolIdentity
	default:
		return errSpool
	}
}

func privateSpoolResolvedPath(path string) (string, error) {
	// EvalSymlinks also restores component case and expands short names on
	// Windows. Its spelling need not match Clean even without a reparse point.
	// privateSpoolPath checks every component for reparse points independently.
	// The temp path uses the same canonicalization; SameFile compares directory
	// identity rather than rejecting children or relying on spelling equality.
	return filepath.EvalSymlinks(path)
}

func privateSpoolPath(path string) bool { return WindowsPrivateValidatePath(path) == nil }

func openPrivateSpoolRoot(path string) (*os.Root, error) { return WindowsPrivateOpenRoot(path) }

func privateSpoolOwner(path string, _ os.FileInfo) bool {
	return WindowsPrivateValidate(path, true) == nil
}

func privateSpoolRoot(root *os.Root) bool {
	directory, err := root.Open(".")
	if err != nil {
		return false
	}
	defer func() { _ = directory.Close() }()
	return WindowsPrivateValidateFile(directory, true) == nil
}
