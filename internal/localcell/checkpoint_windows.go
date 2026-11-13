//go:build windows

package localcell

import (
	"os"
	"path/filepath"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

// checkpointFreeSpace returns the bytes available to the caller and the volume size.
func checkpointFreeSpace(path string) (free, total uint64, err error) {
	pointer, err := windows.UTF16PtrFromString(path)
	var available, size, all uint64
	if err != nil || windows.GetDiskFreeSpaceEx(pointer, &available, &size, &all) != nil {
		return 0, 0, errCheckpointState
	}
	return available, size, nil
}

// openCheckpointSource opens PGDATA files inside the exact pg/ boundary without
// following a reparse point, validating each opened handle.
func (c *Cell) openCheckpointSource() (func(string) (*os.File, error), func(), error) {
	return func(rel string) (*os.File, error) {
		return runtimeconfig.WindowsPrivatePostgresOpen(c.path("pg"), filepath.Join(c.path("pg"), filepath.FromSlash(rel)))
	}, func() {}, nil
}

// validateRecoveryObject applies the strict private-object rule by handle.
func validateRecoveryObject(path string, directory bool) error {
	return validatePrivate(path, directory)
}
