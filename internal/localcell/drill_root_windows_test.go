//go:build windows

package localcell

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func init() {
	drillRootSwapParent = func(t *testing.T, dir, aside, target string) bool {
		if os.Rename(dir, aside) != nil {
			return false
		}
		createTestJunction(t, dir, target)
		return true
	}
	drillRootLockPayload = func(t *testing.T, dir string) func() {
		pointer, err := windows.UTF16PtrFromString(filepath.Join(dir, "pg", "payload"))
		drillAssert(t, err == nil, "cannot prepare the drill root fixture")
		held, err := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
		drillAssert(t, err == nil, "cannot prepare the drill root fixture")
		t.Cleanup(func() {
			if held != windows.InvalidHandle {
				_ = windows.CloseHandle(held)
			}
		})
		return func() {
			drillAssert(t, windows.CloseHandle(held) == nil, "cannot prepare the drill root fixture")
			held = windows.InvalidHandle
		}
	}
	drillRootPlatformRows = func(t *testing.T) {
		t.Run("junction", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			target := c.path("outside")
			drillAssert(t, os.Rename(dir, target) == nil, "cannot prepare the drill root fixture")
			// Deliberately malformed boundary object: a Windows reparse point.
			createTestJunction(t, dir, target)
			before := checkpointTreeState(t, c)
			_, err := c.drillRootList()
			drillAssert(t, err == (drillRootError{reason: "boundary_refused"}), "drill delete followed a link")
			drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill delete followed a link")
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill delete followed a link")
		})
		t.Run("private_tree", func(t *testing.T) {
			c, v := drillRootFixture(t)
			drillRootPlant(t, c, v.DrillID, v)
			drillAssert(t, validatePrivateTree(c.cfg.Root) == nil, "drill root listing was not exact")
		})
	}
}
