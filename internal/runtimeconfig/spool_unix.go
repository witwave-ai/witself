//go:build !windows

package runtimeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var (
	errSpool = errors.New("WITSELF_LOCAL_SPOOL_DIR must be a private directory owned by the current user, distinct from the process temporary directory")
	// Preserve the existing Unix diagnostic for every refusal.
	errSpoolTemporary = errSpool
	errSpoolIdentity  = errSpool
)

func privateSpoolTempPath() (string, error) { return os.TempDir(), nil }

func privateSpoolFailure(_ error) error { return errSpool }

func privateSpoolResolvedPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != filepath.Clean(path) {
		return "", errSpool
	}
	return resolved, nil
}

func privateSpoolPath(_ string) bool { return true }

func privateSpoolOwner(_ string, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}

func privateSpoolRoot(_ *os.Root) bool { return true }

func openPrivateSpoolRoot(path string) (*os.Root, error) { return os.OpenRoot(path) }
