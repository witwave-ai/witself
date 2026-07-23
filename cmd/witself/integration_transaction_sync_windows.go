//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func openIntegrationTransactionFileForSync(path string) (*os.File, error) {
	// FlushFileBuffers, which backs os.File.Sync on Windows, requires a handle
	// opened with write access even when the file bytes are not being changed.
	return os.OpenFile(path, os.O_RDWR, 0)
}

func syncIntegrationTransactionDirectory(path string) error {
	directory, err := openManagedInstructionWindowsDirectory(
		path,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return err
	}
	// Windows exposes no portable directory equivalent of fsync, and
	// FlushFileBuffers rejects directory handles on common local filesystems.
	// Pin and validate the exact non-reparse directory through this boundary.
	return directory.Close()
}
