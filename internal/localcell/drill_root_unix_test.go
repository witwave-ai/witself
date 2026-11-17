//go:build linux || darwin

package localcell

import (
	"os"
	"reflect"
	"testing"
)

func init() {
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
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill delete followed a link")
		})
	}
}
