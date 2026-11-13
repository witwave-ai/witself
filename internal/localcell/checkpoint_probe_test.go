//go:build linux || darwin || windows

package localcell

import "testing"

func TestCheckpointFreeSpaceProbe(t *testing.T) {
	free, total, err := checkpointFreeSpace(t.TempDir())
	if err != nil || free == 0 || free > total {
		t.Fatal("the free-space probe did not report a usable volume")
	}
}
