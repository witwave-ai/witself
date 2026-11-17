//go:build windows

package localcell

import (
	"os"
	"testing"
)

func init() {
	heldPlatformRows = func(t *testing.T) {
		// recovery\ is held without delete sharing: it cannot be renamed until
		// the deletion returns.
		t.Run("recovery_pinned", func(t *testing.T) {
			_, c := heldFixture(t)
			var renamed error
			heldUnsealed(c, func() { renamed = os.Rename(c.recoveryDir(), c.path("moved")) })
			if err := heldDelete(t, c); err != nil || renamed == nil || os.Rename(c.recoveryDir(), c.path("moved")) != nil {
				t.Fatal("a held recovery directory was renamed, or stayed held after the deletion")
			}
		})
	}
}
