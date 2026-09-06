//go:build !windows

package memoryhydration

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func lockObservationFile(file *os.File) error {
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if !time.Now().Before(deadline) {
			return errors.New("hydration ledger is busy")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
