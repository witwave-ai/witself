package localcell

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func init() {
	drillStorageVolumeRows = append(drillStorageVolumeRows, func(t *testing.T) {
		t.Run("ext4_supported", func(t *testing.T) {
			volume, err := drillStorageOpened(t, t.TempDir())
			drillAssert(t, drillExt4Magic == unix.EXT4_SUPER_MAGIC && err == nil && volume.unit >= 4096 && volume.free > 0, "the runner volume is not a supported ext4 volume")
		})
		t.Run("tmpfs_refused", func(t *testing.T) {
			_, err := drillStorageOpened(t, "/dev/shm")
			drillStorageReason(t, err, "unsupported_fs", "drill storage admitted a tmpfs volume")
		})
	})
	drillStorageAdmissionRows = append(drillStorageAdmissionRows, func(t *testing.T) {
		t.Run("checkpoint_absent/alias", func(t *testing.T) {
			v := newCheckpointVerifyCase(t, false)
			drillAssert(t, privateDir(filepath.Join(v.c.recoveryDir(), strings.ToUpper(v.id)), true) == nil, "cannot prepare the drill storage fixture")
			_, err := drillStorageAdmit(t, v, drillStorageCall{t.Context(), v.id, 1, drillStorageAll})
			drillStorageReason(t, err, "checkpoint_absent", "drill storage admitted a checkpoint beside its case alias")
		})
	})
}
