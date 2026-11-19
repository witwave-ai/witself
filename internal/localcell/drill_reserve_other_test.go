//go:build !linux && !windows

package localcell

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"syscall"
	"testing"
)

// Outside Linux and Windows the reservation layer refuses unsupported_fs and writes nothing (macOS and every
// other system).
func TestDrillReservePlatformOther(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		c, _ := unitCell(t)
		boundary, err := os.OpenRoot(c.cfg.Root)
		drillAssert(t, err == nil, "cannot hold the drill reserve fixture root")
		defer func() { _ = boundary.Close() }()
		held, err := boundary.Open(".")
		drillAssert(t, err == nil, "cannot open the drill reserve fixture handle")
		defer func() { _ = held.Close() }()
		before := checkpointTreeState(t, c)
		created, createErr := drillPlatformCreate(c, boundary, boundary, "drill_aaaaaaaaaaaaaaaa", "clone")
		opened, openErr := drillPlatformOpen(boundary, "drill_aaaaaaaaaaaaaaaa", true)
		anchored, anchorErr := drillReserveRoot(boundary, "drill_aaaaaaaaaaaaaaaa")
		for _, err := range []error{drillPlatformMkdir(c, boundary, nil, "drill_aaaaaaaaaaaaaaaa", ""), drillPlatformMkdir(c, boundary, boundary, "drill_aaaaaaaaaaaaaaaa", "sub"),
			createErr, openErr, anchorErr, drillPlatformAllocate(held, 0, 4096), drillPlatformVerify(held, 0)} {
			drillStorageReason(t, err, "unsupported_fs", "the reservation layer did not refuse outside its platforms")
		}
		for _, err := range []error{nil, errors.New("other"), fs.ErrExist, syscall.ENOSPC, syscall.Errno(112), drillStorageError{reason: "storage_exhausted"}} {
			drillAssert(t, drillPlatformRefusal(err) == "", "the platform refusal named a reason outside its platforms")
		}
		drillAssert(t, created == nil && opened == nil && anchored == nil && reflect.DeepEqual(before, checkpointTreeState(t, c)), "the reservation layer wrote outside its platforms")
	})
}

// The reservation outside Linux and Windows: admission refuses the real volume before any write, and with the
// volume faked supported the platform's mkdir refuses; the allocator is fenced (R7) and never reached.
func TestDrillReservationOther(t *testing.T) {
	t.Run("refused_before_write", func(t *testing.T) {
		v := newCheckpointVerifyCase(t, false)
		allocate := drillReserveAllocate
		t.Cleanup(func() { drillReserveAllocate = allocate })
		drillReserveAllocate = func(*os.File, uint64, uint64) error {
			t.Fatal("the reservation allocated outside its platforms")
			return nil
		}
		stages, before := drillRootStages(v.c, func(string, int) {}), checkpointTreeState(t, v.c)
		for _, faked := range []bool{false, true} {
			if faked {
				drillStorageSeam(t, &drillStorageVolume, drillStorageAll)
			}
			drillID, err := v.c.reserveDrillLocked(t.Context(), v.id, "op_reserve", 1)
			drillStorageReason(t, err, "unsupported_fs", "the reservation did not refuse outside its platforms")
			drillAssert(t, drillID == "" && stages["reserve_mkdir"] == map[bool]int{true: 1}[faked] && reflect.DeepEqual(before, checkpointTreeState(t, v.c)),
				"the reservation wrote outside its platforms or refused at another step")
		}
	})
}
