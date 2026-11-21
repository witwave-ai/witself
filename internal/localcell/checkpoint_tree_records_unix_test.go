//go:build linux || darwin

package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// One plant at one existing seam per row pins one behaviour of the tree's open, check or record (MINOR-2 of
// LC-CKPT-WALK-1C1R): recovery/ moved out of the cell and linked back at the first inventory open (open binds first), or
// at the witness temporary's file sync (record binds before its rename); an empty planned directory replaced after the
// copy (check lists each directory as the one it recorded, and a refused listing refuses); a link put at witness.json at
// the witness step, and a directory there at that sync (record refuses all but a regular file or nothing, and returns a
// refused rename); the checkpoint directory replaced at that sync, the call's temporary moved into the replacement
// (record renames only in the directory it recorded). record_synced plants nothing: each record syncs its temporary.
func TestCheckpointTreeRecordSteps(t *testing.T) {
	for _, row := range []string{"open_bound", "rename_bound", "check_identity", "record_regular", "rename_refused", "rename_identity", "record_synced"} {
		t.Run(row, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c, kept, controls, copied, synced, reached := f.cell, heldTrees{}, 0, false, 0, false
			checkpoint := c.path("recovery", heldTarget)
			witness, temporary := filepath.Join(checkpoint, checkpointWitnessName), filepath.Join(checkpoint, ".pending-*")
			r.nextID = func() (string, error) { return heldTarget, nil }
			plant := func(at, kind string) {
				if !reached {
					reached = true
					treePlant(t, c, at, kind, kept)
				}
			}
			c.checkpointTest.beforeStep = func(step, _ string) {
				if step == "open" && row == "open_bound" {
					plant(c.recoveryDir(), "link_out")
				}
			}
			r.after = func(dir string) {
				if copied = true; row == "check_identity" {
					plant(filepath.Join(dir, "pg", "pg_notify"), "dir")
				}
			}
			control := c.checkpointTest.control
			c.checkpointTest.control = func(ctx context.Context) (pgControlData, error) {
				if controls++; controls == 2 && row == "record_regular" {
					plant(witness, "link_out")
				}
				return control(ctx)
			}
			original := syncFile
			t.Cleanup(func() { syncFile = original })
			syncFile = func(file *os.File) error {
				if file.Name() == temporary && copied {
					synced++
				}
				switch {
				case controls != 2 || file.Name() != temporary || reached:
				case row == "rename_bound":
					plant(c.recoveryDir(), "link_out")
				case row == "rename_refused":
					plant(witness, "dir")
				case row == "rename_identity":
					plant(checkpoint, "dir")
					entries, err := os.ReadDir(c.path("tmp", "moved"))
					for _, entry := range entries {
						if strings.HasPrefix(entry.Name(), ".pending-") {
							heldMove(t, filepath.Join(c.path("tmp", "moved"), entry.Name()), filepath.Join(checkpoint, entry.Name()))
						}
					}
					if err != nil {
						t.Fatal("cannot list the moved checkpoint directory")
					}
				}
				return original(file)
			}
			failure := ClassifyCheckpointFailure(c.Checkpoint(t.Context(), checkpointOptions(f)))
			if !reached && row != "record_synced" {
				t.Fatal("the record step row never reached its plant")
			}
			moved := ""
			for dir := range kept {
				moved = dir
			}
			switch row {
			case "open_bound":
				if failure.Class != "local_checkpoint_witness_mismatch" {
					t.Fatalf("the open bound row ended in class %q", failure.Class)
				}
			case "rename_bound":
				if failure.Class != "local_checkpoint_copy_failed" {
					t.Fatalf("the rename bound row ended in class %q", failure.Class)
				}
				if _, err := os.Lstat(filepath.Join(moved, heldTarget, checkpointWitnessName)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("the rename bound row published its witness in the moved recovery directory")
				}
			case "check_identity":
				if failure.Class != "local_checkpoint_witness_mismatch" {
					t.Fatalf("the check identity row ended in class %q", failure.Class)
				}
			case "record_regular":
				if failure.Class != "local_checkpoint_copy_failed" {
					t.Fatalf("the record kind row ended in class %q", failure.Class)
				}
			case "rename_refused":
				if failure.Class != "local_checkpoint_copy_failed" {
					t.Fatalf("the rename refusal row ended in class %q", failure.Class)
				}
			case "rename_identity":
				if failure.Class != "local_checkpoint_copy_failed" {
					t.Fatalf("the rename identity row ended in class %q", failure.Class)
				}
				if treeAt(t, witness) != nil {
					t.Fatal("the rename identity row published its witness in the replacement directory")
				}
			case "record_synced":
				if failure.Class != "" {
					t.Fatalf("the record sync row ended in class %q", failure.Class)
				}
				if synced != 2 {
					t.Fatalf("the record sync row synced %d record temporaries", synced)
				}
			}
		})
	}
}

// A created witness is today's encoding: the two-space indented document and one trailing newline (N9 of 1C1R).
func TestCheckpointTreeWitnessEncoding(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return heldTarget, nil }
	if f.cell.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
		t.Fatal("cannot create the witness encoding checkpoint")
	}
	data, err := os.ReadFile(f.cell.path("recovery", heldTarget, checkpointWitnessName))
	var witness checkpointWitness
	if err != nil || json.Unmarshal(data, &witness) != nil {
		t.Fatal("cannot read the witness encoding checkpoint")
	}
	if encoded, err := json.MarshalIndent(witness, "", "  "); err != nil || !bytes.Equal(data, append(encoded, '\n')) {
		t.Fatal("the witness encoding row found bytes other than the indented document and its newline")
	}
}
