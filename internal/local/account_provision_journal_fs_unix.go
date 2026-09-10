//go:build !windows

package local

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// These journal-specific seams retain the existing POSIX operations.
type accountProvisionDirectoryPins struct{}

func (*accountProvisionDirectoryPins) close() error { return nil }
func pinAccountProvisionDirectories(_, _ string, _ bool) (*accountProvisionDirectoryPins, error) {
	return &accountProvisionDirectoryPins{}, nil
}
func pinAccountProvisionCredentialDirectories(_, _ string) (*accountProvisionDirectoryPins, error) {
	return &accountProvisionDirectoryPins{}, nil
}
func accountProvisionHome(home string) (string, error) { return home, nil }
func openAccountProvisionPrivateFile(path string, create bool) (*os.File, bool, error) {
	return openLocalLockFileNoFollow(path, create)
}
func createAccountProvisionPrivateTemp(directory, pattern string) (*os.File, error) {
	return os.CreateTemp(directory, pattern)
}
func prepareAccountProvisionPrivateTemp(file *os.File) error { return file.Chmod(0o600) }
func validateAccountProvisionPrivateFileHandle(_ *os.File, _ string, _ os.FileInfo) error {
	return nil
}
func validateAccountProvisionPrivatePath(_ string, _ os.FileInfo) error { return nil }
func removeAccountProvisionPrivateFile(path string, _ os.FileInfo) error {
	return os.Remove(path)
}

// AvailableAccountProvisionName keeps ordinary availability semantics on POSIX.
func AvailableAccountProvisionName(name string) error { return Available(name) }

func ensureAccountProvisionJournalDirectories(home, directory string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	for _, path := range accountProvisionJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return ErrAccountProvisionJournalStorage
			}
			info, err = os.Lstat(path)
			if err != nil {
				return ErrAccountProvisionJournalStorage
			}
		case err != nil:
			return ErrAccountProvisionJournalStorage
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func validateAccountProvisionJournalDirectories(home, directory string) error {
	for _, path := range accountProvisionJournalDirectories(home, directory) {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func privateAccountProvisionJournalDirectory(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm() == 0o700
}

func privateRegularAccountProvisionJournalFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 &&
		info.Mode().Perm() == 0o600
}

func syncAccountProvisionJournalDirectory(directory string) error {
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func ensurePrivateProvisionDirectory(home, directory string) error {
	relative, err := filepath.Rel(home, directory)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrAccountProvisionJournalUnsafe
	}
	current := home
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(current, 0o700); err != nil &&
				!errors.Is(err, os.ErrExist) {
				return ErrAccountProvisionJournalStorage
			}
			info, err = os.Lstat(current)
			if err != nil {
				return ErrAccountProvisionJournalStorage
			}
		case err != nil:
			return ErrAccountProvisionJournalStorage
		}
		if !privateAccountProvisionJournalDirectory(info) {
			return ErrAccountProvisionJournalUnsafe
		}
	}
	return nil
}

func syncAccountProvisionLockPublication(file *os.File, directory string, created bool) error {
	if !created {
		return nil
	}
	if err := file.Sync(); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	if err := syncAccountProvisionJournalDirectory(directory); err != nil {
		return ErrAccountProvisionJournalStorage
	}
	return nil
}
