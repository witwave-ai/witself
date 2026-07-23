//go:build windows

package transcriptcapture

import (
	"os"

	"golang.org/x/sys/windows"
)

func replaceFileAtomic(source, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}

	// MoveFileEx is the Win32 atomic same-volume replacement primitive. The
	// staged file lives beside the destination, so it inherits that directory's
	// ACL; REPLACE_EXISTING gives reinstall the same overwrite behavior as a
	// POSIX rename, and WRITE_THROUGH waits for the move to reach storage.
	return windows.MoveFileEx(
		sourcePointer,
		destinationPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func replacementCommitIdentityMatches(staged, previous, committed os.FileInfo) bool {
	// MOVEFILE_REPLACE_EXISTING replaces the contents of an existing destination
	// and preserves that destination's file identity. When there was no
	// destination, the staged file itself is moved into place.
	if previous != nil {
		return os.SameFile(previous, committed)
	}
	return os.SameFile(staged, committed)
}
