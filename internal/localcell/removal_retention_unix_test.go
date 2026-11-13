//go:build darwin || linux

package localcell

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A Remove that crashed after begin, while its stop was still running, leaves
// recorded components alive and the removal open. Repair must stop every one of
// them before the retained marker exists; otherwise it closes the removal around
// a live component that nothing prompts anyone to stop (review LC-S7D2A-XR1 F2,
// mutant M4). The order is observed when the first manifest bytes carrying the
// marker are synced, before they are published. The components are disposable
// helper processes stopped by SIGTERM; Windows stops through launch shutdown
// descriptors and is not covered here.
func TestRetainedRemovalRepairStopsRecordedTreeBeforeMarker(t *testing.T) {
	f := retainedRemovalFixture(t, "legacy", false)
	c := f.cell
	components := []string{"server", "worker"}
	for _, component := range components {
		record := signalSafetyChild(t)
		record.Root, record.Component = c.cfg.Root, component
		if writeAtomic(c.path("run", component+".json"), record, nil) != nil {
			t.Fatal("cannot record a live synthetic component")
		}
		if state, err := c.processState(component); err != nil || !state.Alive || !state.Matches {
			t.Fatal("synthetic component is not a live owned record")
		}
	}
	if c.begin("remove") != nil || c.journal("remove", "stop", "started") != nil {
		t.Fatal("cannot prepare a removal interrupted during its stop")
	}
	original := syncFile
	t.Cleanup(func() { syncFile = original })
	// One entry per marker publication: whether any recorded component was
	// still live (or unobservable) when the marker bytes were synced.
	var published []bool
	syncFile = func(file *os.File) error {
		if len(published) == 0 && filepath.Dir(file.Name()) == c.cfg.Root {
			if data, err := os.ReadFile(file.Name()); err == nil && bytes.Contains(data, []byte(`"removal_retained": true`)) {
				live := false
				for _, component := range components {
					state, err := c.processState(component)
					live = live || err != nil || state.Alive
				}
				published = append(published, live)
			}
		}
		return original(file)
	}
	err := c.Repair(t.Context())
	syncFile = original
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0] {
		t.Fatal("Repair did not publish the retained marker once, after every recorded component stopped")
	}
	retainedAssertRecords(t, f, retainedStopCutRecords)
	retainedAssertMaterial(t, f)
	retainedAssertSettled(t, c)
	retainedAssertStartRefused(t, f)
}
