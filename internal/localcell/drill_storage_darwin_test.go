package localcell

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func init() {
	drillStorageVolumeRows = append(drillStorageVolumeRows, func(t *testing.T) {
		t.Run("apfs_refused", func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.Open(dir)
			drillAssert(t, err == nil, "cannot open the drill storage volume fixture")
			defer func() { _ = f.Close() }()
			var stat unix.Statfs_t
			drillAssert(t, unix.Fstatfs(int(f.Fd()), &stat) == nil && unix.ByteSliceToString(stat.Fstypename[:]) == "apfs", "the runner volume is not an APFS volume")
			_, err = drillStorageOpened(t, dir)
			drillStorageReason(t, err, "unsupported_fs", "drill storage admitted a macOS volume")
		})
	})
}
