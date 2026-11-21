//go:build linux

package localcell

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A write lease on a checkpoint's witness kept past the bound (300 ms here) is
// never read as incomplete: the released listing returns
// errCheckpointWitnessUnread, Inspect and Checkpoint fail with the
// operation-failed class and no reason, before any control run, the tree
// unchanged. A holder that lets go at the break is waited out.
func TestCheckpointWitnessLease(t *testing.T) {
	for _, holder := range []string{"kept", "retaken", "lets_go"} {
		t.Run(holder, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
				t.Fatal("cannot make the checkpoint whose witness is leased")
			}
			id := checkpointIDs(t, c)[0]
			dir, before, controls := c.path("recovery", id), checkpointTreeState(t, c), r.calls["control"]
			lease := leaseTaken(t, leaseHolder(t, holder, unix.F_WRLCK), filepath.Join(dir, checkpointWitnessName))
			var listed []CheckpointSummary
			started := time.Now()
			read := readerWatchdog(t, func() (err error) {
				listed, _, err = c.listCheckpoints(context.Background())
				return err
			}, lease.release)
			if holder != "lets_go" && time.Since(started) < leaseKeptAfter {
				t.Fatal("the witness listing ended before the lease wait bound")
			}
			if !lease.broken() {
				t.Fatal("the witness listing never broke the lease")
			}
			inspected := readerWatchdog(t, func() error { _, err := c.Inspect(t.Context()); return err }, lease.release)
			created := readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, lease.release)
			if holder == "lets_go" {
				if read != nil || len(listed) != 1 || listed[0].ID != id || !listed[0].Complete || inspected != nil || created != nil || len(checkpointIDs(t, c)) != 2 {
					t.Fatal("a witness whose lease holder let go at the break was not waited out and read complete")
				}
				return
			}
			want := CheckpointFailure{Class: "local_checkpoint_operation_failed"}
			if len(listed) != 0 || !errors.Is(read, errCheckpointWitnessUnread) || ClassifyCheckpointFailure(inspected) != want || ClassifyCheckpointFailure(created) != want {
				t.Fatal("a witness leased past the bound was read as incomplete or refused with another class")
			}
			if r.calls["control"] != controls || !reflect.DeepEqual(checkpointLeasedTree(t, c, lease.file), before) {
				t.Fatal("a refusal beside a leased witness ran the control or changed the tree")
			}
		})
	}
}
