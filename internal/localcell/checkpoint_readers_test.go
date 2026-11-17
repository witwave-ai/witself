package localcell

import (
	"crypto/sha256"
	"os"
	"strings"
	"testing"
)

// The create path's readers still copy every regular file byte for byte, the
// ones this slice changed included: PG_VERSION, the manifest, custody and
// global/pg_control. Digests are compared in memory and never printed.
func TestCheckpointReadersStillCopy(t *testing.T) {
	for _, layout := range []string{"legacy", "selected"} {
		t.Run(layout, func(t *testing.T) {
			f, r := checkpointFixture(t, layout, false)
			c := f.cell
			if err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, func() {}); err != nil {
				t.Fatal("checkpoint refused a regular copy set")
			}
			ids := checkpointIDs(t, c)
			if len(ids) != 1 || !checkpointSummaryOf(t, c, ids[0]).Complete || r.calls["control"] != 2 {
				t.Fatal("checkpoint did not complete exactly one witnessed copy")
			}
			copied, want := retainedSnapshot(t, &Cell{cfg: Config{Root: c.path("recovery", ids[0])}}), checkpointCopySet(f)
			for _, name := range []string{"pg/PG_VERSION", "manifest.json", "run/postgres.password", "pg/global/pg_control"} {
				if entry, planned := want[name]; !planned || !entry.mode.IsRegular() {
					t.Fatal("a changed reader's file is missing from the copy set")
				}
			}
			for name, source := range want {
				got, found := copied[name]
				if !found || got.mode.IsDir() != source.mode.IsDir() || got.size != source.size || sha256.Sum256([]byte(got.data)) != sha256.Sum256([]byte(source.data)) {
					t.Fatal("a copied entry differs from its source")
				}
			}
		})
	}
}

// checkpointReaderVersion plants a PG_VERSION state before the call and
// requires the refusal it always had: an unsupported source, before the plan
// and the control boundary, with the tree unchanged.
func checkpointReaderVersion(t *testing.T, plant func(*testing.T, *Cell)) {
	t.Helper()
	f, r := checkpointFixture(t, "legacy", false)
	plant(t, f.cell)
	before := checkpointTreeState(t, f.cell)
	err := readerWatchdog(t, func() error { return f.cell.Checkpoint(t.Context(), checkpointOptions(f)) }, func() {})
	if checkpointUnchanged(t, f.cell, before); !checkpointSameError(err, "local_checkpoint_source_unsupported") || r.calls["control"] != 0 {
		t.Fatal("PG_VERSION was not refused as an unsupported source before the plan")
	}
}

// An empty regular PG_VERSION is an unsupported source, as before (R7).
func TestCheckpointReadersEmptyVersion(t *testing.T) {
	checkpointReaderVersion(t, func(t *testing.T, c *Cell) { checkpointWrite(t, c, "pg/PG_VERSION", "") })
}

// An absent or oversized PG_VERSION present at admission keeps its outcome too.
func TestCheckpointReadersVersionUnchanged(t *testing.T) {
	for _, row := range []struct {
		name  string
		plant func(*testing.T, *Cell)
	}{
		{"absent", func(t *testing.T, c *Cell) {
			if os.Remove(c.path("pg", "PG_VERSION")) != nil {
				t.Fatal("cannot remove the fixture PG_VERSION")
			}
		}},
		{"oversized", func(t *testing.T, c *Cell) { checkpointWrite(t, c, "pg/PG_VERSION", "16\n"+strings.Repeat(" ", 62)) }},
	} {
		t.Run(row.name, func(t *testing.T) { checkpointReaderVersion(t, row.plant) })
	}
}
