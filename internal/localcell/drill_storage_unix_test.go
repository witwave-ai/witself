//go:build linux || darwin

package localcell

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func init() {
	drillStorageAdmissionRows = append(drillStorageAdmissionRows, func(t *testing.T) {
		t.Run("boundary_refused/fifo", func(t *testing.T) {
			v := newCheckpointVerifyCase(t, false)
			fifo := filepath.Join(v.c.recoveryDir(), "pipe")
			drillAssert(t, syscall.Mkfifo(fifo, 0o600) == nil, "cannot prepare the drill storage fifo")
			drillStorageSeam(t, &drillStorageVolume, drillStorageAll)
			before := drillStorageSnapshot(t, v.c)
			err := readerWatchdog(t, func() error {
				_, err := v.c.admitDrillStorageLocked(t.Context(), v.id, 1)
				return err
			}, func() { writer, _ := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); _ = writer.Close() })
			drillStorageReason(t, err, "boundary_refused", "drill storage did not refuse a planted fifo")
			drillAssert(t, reflect.DeepEqual(before, drillStorageSnapshot(t, v.c)), "drill storage admission changed the cell tree")
		})
	})
}
