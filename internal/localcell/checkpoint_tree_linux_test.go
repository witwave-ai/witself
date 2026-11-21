//go:build linux

package localcell

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A write lease on a copy (pg/base/1/1259), taken once the copy is done (checkpointRecorder.after), is waited out by
// the inventory's read through the tree as openPrivateReader waits: a holder that lets go at the break costs a moment
// and create makes one complete checkpoint; one that lets go and takes it again is refused when the wait's ceiling
// (300 ms here) ends, as a witness mismatch with the call's own directory removed.
func TestCheckpointTreeInventoryLease(t *testing.T) {
	for _, holder := range []string{"lets_go", "retaken"} {
		t.Run(holder, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c, lease, taken, planted := f.cell, leaseHolder(t, holder, unix.F_WRLCK), false, time.Time{}
			r.after = func(dir string) {
				lease.take(filepath.Join(dir, "pg", "base", "1", "1259"))
				taken, planted = lease.do(func(uintptr) {}), time.Now()
			}
			err := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, lease.release)
			returned, failure, ids := time.Now(), ClassifyCheckpointFailure(err), checkpointIDs(t, c)
			if planted.IsZero() {
				t.Fatal("the inventory lease row never reached its planting stage")
			}
			if !taken {
				t.Fatal("this kernel or sandbox refused the write lease the inventory row needs")
			}
			if !lease.broken() {
				t.Fatal("the inventory lease row never broke the lease")
			}
			if !readers5JournalClosed(c) {
				t.Fatal("the inventory lease row left its journal open or torn")
			}
			if holder == "lets_go" {
				if err != nil {
					t.Fatalf("the inventory lease row whose holder let go ended in class %q", failure.Class)
				}
				if len(ids) != 1 || !checkpointSummaryOf(t, c, ids[0]).Complete {
					t.Fatal("the inventory lease row whose holder let go left no single complete checkpoint")
				}
				return
			}
			if returned.Sub(planted) < leaseKeptAfter {
				t.Fatal("the inventory lease row was refused before the wait could end")
			}
			if failure.Class != "local_checkpoint_witness_mismatch" {
				t.Fatalf("the inventory lease row taken again ended in class %q", failure.Class)
			}
			if failure.CleanupIncomplete {
				t.Fatal("the inventory lease row taken again reported an incomplete cleanup")
			}
			if len(ids) != 0 {
				t.Fatal("the inventory lease row taken again left a checkpoint directory")
			}
		})
	}
}
