//go:build linux || darwin

package localcell

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func init() {
	drillRootLockPayload = func(t *testing.T, dir string) func() {
		if os.Geteuid() == 0 {
			t.Skip("root deletes from a non writable directory")
		}
		path := filepath.Join(dir, "pg")
		drillAssert(t, os.Chmod(path, 0o500) == nil, "cannot prepare the drill root fixture")
		t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
		return func() { drillAssert(t, os.Chmod(path, 0o700) == nil, "cannot prepare the drill root fixture") }
	}
	drillRootPlatformRows = func(t *testing.T) {
		t.Run("link", func(t *testing.T) {
			c, v := drillRootFixture(t)
			dir := drillRootPlant(t, c, v.DrillID, v)
			target := c.path("outside")
			// Deliberately malformed boundary object: a Unix symbolic link.
			drillAssert(t, os.Rename(dir, target) == nil && os.Symlink(target, dir) == nil, "cannot prepare the drill root fixture")
			before := checkpointTreeState(t, c)
			_, err := c.drillRootList()
			drillAssert(t, err == (drillRootError{reason: "boundary_refused"}), "drill delete followed a link")
			drillRootRefused(t, c, v.DrillID, "boundary_refused", "drill delete followed a link")
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill delete followed a link")
		})
	}
}
