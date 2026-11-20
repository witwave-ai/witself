//go:build linux || darwin

package localcell

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// At the witness step (the second checkpointTest.control call), the checkpoint directory or recovery/ moved aside and
// another object put at its name: the witness is written nowhere, so the moved original, every link target and a
// planted object stay as they were; the create fails as a copy failure, its cleanup incomplete, its journal closed.
func TestCheckpointTreeRecordsRefuse(t *testing.T) {
	for _, row := range []struct {
		name  string
		kinds []string
	}{{"checkpoint", []string{"dir", "link_cell", "link_out", "dangling", "file", "fifo"}}, {"recovery", []string{"dir", "link_out"}}} {
		for _, kind := range row.kinds {
			t.Run(row.name+"/"+kind, func(t *testing.T) {
				f, r := checkpointFixture(t, "legacy", false)
				c, kept, calls, reached := f.cell, heldTrees{}, 0, false
				at, want, target, linked := c.path("recovery", heldTarget), any(nil), "", any(nil)
				if r.nextID = func() (string, error) { return heldTarget, nil }; row.name == "recovery" {
					at = c.recoveryDir()
				}
				control := c.checkpointTest.control
				c.checkpointTest.control = func(ctx context.Context) (pgControlData, error) {
					reading, err := control(ctx)
					if calls++; calls == 2 {
						reached = true
						treePlant(t, c, at, kind, kept)
						want = treeAt(t, at)
						if target, _ = os.Readlink(at); target != "" {
							linked = treeAt(t, target)
						}
					}
					return reading, err
				}
				failure := ClassifyCheckpointFailure(c.Checkpoint(t.Context(), checkpointOptions(f)))
				if !reached {
					t.Fatal("the record row never reached the witness step")
				}
				if failure.Class != "local_checkpoint_copy_failed" {
					t.Fatalf("the record row ended in class %q", failure.Class)
				}
				if !failure.CleanupIncomplete {
					t.Fatal("the record row reported a complete cleanup")
				}
				heldSame(t, kept)
				if target != "" && !reflect.DeepEqual(treeAt(t, target), linked) {
					t.Fatal("the record row changed what the planted link names")
				}
				if !reflect.DeepEqual(treeAt(t, at), want) {
					t.Fatal("the record row changed the object planted at its name")
				}
				if !readers5JournalClosed(c) {
					t.Fatal("the record row left its journal open or torn")
				}
			})
		}
	}
}

// An unplanned private file or FIFO put in a directory the copy made (pg/zz-extra, at checkpointRecorder.after) is
// refused by the inventory's tree check by listing, which opens none: a witness mismatch, the call's own directory
// removed and no checkpoint left.
func TestCheckpointTreeInventoryExtra(t *testing.T) {
	for _, kind := range []string{"file", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c, reached, planted := f.cell, false, error(nil)
			r.after = func(dir string) {
				name := filepath.Join(dir, "pg", "zz-extra")
				reached, planted = true, map[string]func() error{"file": func() error { return testWriteFile(name, []byte("unplanned"), 0o600) },
					"fifo": func() error { return syscall.Mkfifo(name, 0o600) }}[kind]()
			}
			failure := ClassifyCheckpointFailure(readerWatchdog(t, func() error { return c.Checkpoint(t.Context(), checkpointOptions(f)) }, func() {}))
			if !reached || planted != nil {
				t.Fatal("the extra entry row never planted its unplanned entry")
			}
			if failure.Class != "local_checkpoint_witness_mismatch" {
				t.Fatalf("the extra entry row ended in class %q", failure.Class)
			}
			if failure.CleanupIncomplete {
				t.Fatal("the extra entry row reported an incomplete cleanup")
			}
			if len(checkpointIDs(t, c)) != 0 {
				t.Fatal("the extra entry row left a checkpoint directory")
			}
			if !readers5JournalClosed(c) {
				t.Fatal("the extra entry row left its journal open or torn")
			}
		})
	}
}
