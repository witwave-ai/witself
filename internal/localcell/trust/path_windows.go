package trust

import (
	"os"
	"syscall"
)

// Windows has no POSIX O_NOFOLLOW; reject reparse/symlink mode bits with Lstat,
// and bind the opened handle to both observations before reading any bytes.
func openInventoryFile(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || !inventoryInfoSafe(before) {
		return nil, ExtractEntryType
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	actual, statErr := file.Stat()
	after, lstatErr := root.Lstat(name)
	if statErr != nil || lstatErr != nil || !actual.Mode().IsRegular() || !after.Mode().IsRegular() || !inventoryInfoSafe(actual) || !inventoryInfoSafe(after) || !os.SameFile(before, actual) || !os.SameFile(after, actual) {
		_ = file.Close()
		return nil, ExtractEntryType
	}
	return file, nil
}

// Windows exposes writable/read-only permissions, not POSIX executable bits.
func inventoryModeMatches(actual os.FileMode, expected os.FileMode) bool {
	return actual.IsRegular() && actual.Perm()&0200 == expected&0200
}

// Mode bits do not identify all Windows reparse tags (for example DEDUP).
func inventoryInfoSafe(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes != nil && attributes.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func openArchiveFile(name string) (*os.File, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, ExtractFilesystem
	}
	if !before.Mode().IsRegular() || !inventoryInfoSafe(before) {
		return nil, ExtractEntryType
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, ExtractFilesystem
	}
	actual, statErr := file.Stat()
	after, lstatErr := os.Lstat(name)
	if statErr != nil || lstatErr != nil || !actual.Mode().IsRegular() || !after.Mode().IsRegular() || !inventoryInfoSafe(actual) || !inventoryInfoSafe(after) || !os.SameFile(before, actual) || !os.SameFile(after, actual) {
		_ = file.Close()
		return nil, ExtractEntryType
	}
	return file, nil
}
