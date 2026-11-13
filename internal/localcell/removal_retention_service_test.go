package localcell

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

// A removed (marked) cell keeps Installed so service removal and purge still
// work, but PrepareService refuses to enroll it, before any service state is
// written, with its incomplete-installation text (review LC-S7D2A-XR1 F6). The
// enrolled case reaches the idempotent re-preparation path.
func TestRetainedRemovalRefusesServiceEnrollment(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		name := "never_enrolled"
		if enrolled {
			name = "enrolled"
		}
		t.Run(name, func(t *testing.T) {
			f := retainedRemovalFixture(t, "legacy", enrolled)
			c := f.cell
			if c.Remove(t.Context(), RemoveOptions{}) != nil {
				t.Fatal("cannot prepare retained removal")
			}
			t.Setenv("WITSELF_RUNTIME", "local")
			host, err := os.Executable()
			if err != nil {
				t.Fatal("test executable unavailable")
			}
			before := retainedSnapshot(t, c)
			err = c.PrepareService(t.Context(), ServiceDescriptor{Adapter: "foreground", Payload: ServicePayload{Host: host, Args: []string{"run", "--root", c.cfg.Root}}})
			if err == nil || err.Error() != "service_installation_required" {
				t.Fatal("removed cell was enrolled as a service")
			}
			if !reflect.DeepEqual(before, retainedSnapshot(t, c)) {
				t.Fatal("refused enrollment wrote service state")
			}
			d, err := c.ServiceDescriptor()
			if enrolled && (err != nil || d.DesiredState != "stopped") || !enrolled && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused enrollment changed the service descriptor")
			}
			if enrolled {
				// The stopped service fence refuses Start before the marker.
				if c.Start(t.Context()) == nil || len(f.launches) != 0 {
					t.Fatal("removed enrolled cell became startable")
				}
				return
			}
			retainedAssertStartRefused(t, f)
		})
	}
}
