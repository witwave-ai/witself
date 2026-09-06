//go:build windows

package memoryhydration

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockObservationFile(file *os.File) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("hydration ledger is busy")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
