package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
)

var errSpool = errors.New("WITSELF_LOCAL_SPOOL_DIR must be a private directory owned by the current user, distinct from the process temporary directory")

// ValidateSpoolDir refuses the process-wide temp directory, including aliases.
// It does not create, repair, or change ownership of any directory.
func ValidateSpoolDir(dir string) error {
	_, err := validatedSpoolDirectory(dir)
	return err
}

func validatedSpoolDirectory(dir string) (os.FileInfo, error) {
	if dir == "" {
		return nil, errSpool
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, errSpool
	}
	tempInfo, err := os.Stat(os.TempDir())
	if err != nil || os.SameFile(info, tempInfo) {
		return nil, errSpool
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != filepath.Clean(dir) || !privateSpoolOwner(resolved, info) {
		return nil, errSpool
	}
	return info, nil
}

// OpenSpoolRoot pins the validated directory for the lifetime of the process.
// Check the opened handle as well, so a replacement during validation cannot
// substitute a different directory before the handle is acquired.
func OpenSpoolRoot(dir string) (*os.Root, error) {
	before, err := validatedSpoolDirectory(dir)
	if err != nil {
		return nil, errSpool
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, errSpool
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, errSpool
	}
	return root, nil
}
