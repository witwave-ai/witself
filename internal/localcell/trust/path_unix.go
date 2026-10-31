//go:build !windows

package trust

import (
	"os"
	"path/filepath"
	"syscall"
)

func openInventoryFile(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() {
		return nil, ExtractEntryType
	}
	// Root.OpenFile resolves symlinks itself even when O_NOFOLLOW is supplied.
	// Open the exact pathname with the actual OS flag, then bind that descriptor
	// back to the confined root before any file bytes are read.
	file, err := os.OpenFile(filepath.Join(root.Name(), name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, statErr := file.Stat()
	after, lstatErr := root.Lstat(name)
	if statErr != nil || lstatErr != nil || !actual.Mode().IsRegular() || !after.Mode().IsRegular() || !os.SameFile(before, actual) || !os.SameFile(after, actual) {
		_ = file.Close()
		return nil, ExtractEntryType
	}
	return file, nil
}
func inventoryModeMatches(actual os.FileMode, expected os.FileMode) bool {
	return actual.IsRegular() && actual.Perm() == expected && actual&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func inventoryInfoSafe(info os.FileInfo) bool { return info != nil && info.Mode()&os.ModeSymlink == 0 }

// A source archive must never block waiting for a FIFO/device producer.
func openArchiveFile(name string) (*os.File, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ExtractFilesystem
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, ExtractFilesystem
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ExtractEntryType
	}
	return file, nil
}
