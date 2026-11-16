//go:build windows

package localcell

import (
	"os"
	"reflect"
	"testing"
)

func init() {
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
			drillAssert(t, reflect.DeepEqual(before, checkpointTreeState(t, c)), "drill delete followed a link")
		})
		t.Run("private_tree", func(t *testing.T) {
			c, v := drillRootFixture(t)
			drillRootPlant(t, c, v.DrillID, v)
			drillAssert(t, validatePrivateTree(c.cfg.Root) == nil, "drill root listing was not exact")
		})
	}
}
