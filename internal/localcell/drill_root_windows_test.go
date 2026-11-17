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
	drillRootLink = createTestJunction
	drillRootLock = func(t *testing.T, file string) func() {
		pointer, err := windows.UTF16PtrFromString(file)
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
	drillRootPlatformDeleteRows = func(t *testing.T) {
		// Where the Unix row name_linked plants a link: the drill's name replaced at
		// the first sweep by a junction to a directory holding a sentinel.
		t.Run("name_junction", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir, aside, outside := drillRootPlant(t, c, v.DrillID, v), filepath.Join(c.recoveryDir(), "aside"), testPrivateRoot(t)
			drillAssert(t, testWriteFile(filepath.Join(outside, "sentinel"), []byte("outside sentinel"), 0o600) == nil, "cannot prepare the drill root fixture")
			drillRootAt(c, "sweep", 1, func() {
				drillAssert(t, os.Rename(dir, aside) == nil, "cannot prepare the drill root fixture")
				createTestJunction(t, dir, outside)
			})
			err := c.deleteDrillLocked(v.DrillID)
			sentinel, readErr := os.ReadFile(filepath.Join(outside, "sentinel"))
			_, nameErr := os.Lstat(dir)
			drillAssert(t, err == (drillRootError{reason: "boundary_refused"}) && drillRootKept(t, aside, v) && readErr == nil &&
				string(sentinel) == "outside sentinel" && nameErr == nil, "drill delete followed a junction at the drill name")
		})
	}
}
