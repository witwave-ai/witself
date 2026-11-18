//go:build !linux

package localcell

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"syscall"
	"testing"
)

// Outside Linux the reservation layer refuses unsupported_fs and writes nothing (macOS and every other system;
// Windows until its own platform layer).
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
		for _, err := range []error{drillPlatformMkdir(c, boundary, nil, "drill_aaaaaaaaaaaaaaaa", ""), drillPlatformMkdir(c, boundary, boundary, "drill_aaaaaaaaaaaaaaaa", "sub"),
			createErr, drillPlatformAllocate(held, 0, 4096), drillPlatformVerify(held, 0)} {
			drillStorageReason(t, err, "unsupported_fs", "the reservation layer did not refuse outside its platforms")
		}
		for _, err := range []error{nil, errors.New("other"), fs.ErrExist, syscall.ENOSPC, syscall.Errno(112), drillStorageError{reason: "storage_exhausted"}} {
			drillAssert(t, drillPlatformRefusal(err) == "", "the platform refusal named a reason outside its platforms")
		}
		drillAssert(t, created == nil && reflect.DeepEqual(before, checkpointTreeState(t, c)), "the reservation layer wrote outside its platforms")
	})
}
