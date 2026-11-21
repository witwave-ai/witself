//go:build linux || darwin

package localcell

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Each replacement happens in the listed stage of a single owned Inspect, with two complete checkpoints.
func TestCheckpointTreeListingSwapped(t *testing.T) {
	for _, kind := range []string{"link_out", "file", "fifo", "dangling", "absent", "dir", "other_witness"} {
		t.Run(kind, func(t *testing.T) {
			_, c := heldFixture(t)
			name, kept := c.path("recovery", heldTarget), heldTrees{}
			calls := heldAt(c, "listed", func() {
				switch kind {
				case "absent":
					moved := filepath.Join(testPrivateRoot(t), "moved")
					heldMove(t, name, moved)
					kept[moved] = heldTree(t, moved)
				case "other_witness":
					treePlant(t, c, name, "dir", kept)
					checkpointPlant(t, c, heldTarget, heldTarget)
				default:
					treePlant(t, c, name, kind, kept)
				}
			})
			var inspection Inspection
			err := readerWatchdog(t, func() (err error) {
				inspection, err = c.Inspect(t.Context())
				return err
			}, func() {})
			if calls["listed"] != 1 {
				t.Fatal("the inspection did not reach the listed stage exactly once")
			}
			if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) {
				t.Fatal("the inspection accepted a changed checkpoint name or returned another class")
			}
			if len(inspection.Checkpoints) != 0 {
				t.Fatal("the refused listing exposed a checkpoint summary")
			}
			heldSame(t, kept)
		})
	}
}

// Leaf absence and irregularity inside the same listed directory still mean incomplete.
func TestCheckpointTreeWitnessLeaf(t *testing.T) {
	for _, kind := range []string{"absent", "directory", "link"} {
		t.Run(kind, func(t *testing.T) {
			_, c := heldFixture(t)
			name := c.path("recovery", heldTarget, checkpointWitnessName)
			calls := heldAt(c, "listed", func() {
				if os.Remove(name) != nil {
					t.Fatal("cannot remove the listed witness")
				}
				if kind == "directory" && os.Mkdir(name, 0o700) != nil || kind == "link" && os.Symlink("missing", name) != nil {
					t.Fatal("cannot prepare the irregular witness")
				}
			})
			inspection, err := c.Inspect(t.Context())
			if calls["listed"] != 1 {
				t.Fatal("the witness fixture missed the listed stage")
			}
			if err != nil || inspection.CheckpointListing != "listed" || len(inspection.Checkpoints) != 2 {
				t.Fatal("the observation lost the listing for the examined witness")
			}
			first, second := inspection.Checkpoints[0], inspection.Checkpoints[1]
			if first.ID != heldTarget || first.Complete {
				t.Fatal("the examined witness did not produce the first incomplete summary")
			}
			if second.ID != heldOther || !second.Complete {
				t.Fatal("the unaffected witness did not produce the second complete summary")
			}
		})
	}
}

func TestCheckpointTreeListingBoundary(t *testing.T) {
	c := checkpointInspectCase(t).c
	if sums, unknown, err := c.listCheckpoints(); err != nil || len(sums) != 0 || unknown != 0 {
		t.Fatal("the missing recovery boundary did not yield an empty listing")
	}
	if os.Symlink("missing", c.recoveryDir()) != nil {
		t.Fatal("cannot plant the dangling boundary link")
	}
	_, _, err := c.listCheckpoints()
	if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "boundary_unreadable"}) {
		t.Fatal("the dangling recovery boundary was reported empty or with another refusal")
	}
	c.cfg.Root = filepath.Join(c.cfg.Root, "missing-cell")
	if _, _, err := c.listCheckpoints(); ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "boundary_unreadable"}) {
		t.Fatal("the missing cell root was reported empty or with another refusal")
	}
	checkpointDiagnosticReason(t, c.validateRecovery(), "boundary_unreadable")
}

func TestCheckpointTreeSnapshotVanished(t *testing.T) {
	for _, row := range []string{"once", "each_pass", "directory", "moved_boundary"} {
		t.Run(row, func(t *testing.T) {
			k := checkpointInspectCase(t)
			leaves := []string{"first", "second", "third"}
			dir := checkpointPlant(t, k.c, heldTarget, "", leaves...)
			if row == "directory" && os.Mkdir(filepath.Join(dir, "child"), 0o700) != nil {
				t.Fatal("cannot prepare the vanishing directory")
			}
			passes, original := 0, checkpointTreeListed
			t.Cleanup(func() { checkpointTreeListed = original })
			checkpointTreeListed = func(name string) {
				if row == "moved_boundary" && name == "." {
					passes++
					heldMove(t, k.c.recoveryDir(), filepath.Join(k.c.cfg.Root, "moved"))
					return
				}
				if name != heldTarget {
					return
				}
				passes++
				switch {
				case row == "directory" && passes == 1:
					if os.Remove(filepath.Join(dir, "child")) != nil {
						t.Fatal("cannot remove the listed empty directory")
					}
				case row == "once" && passes == 1 || row == "each_pass" && passes <= len(leaves):
					if os.Remove(filepath.Join(dir, leaves[passes-1])) != nil {
						t.Fatal("cannot remove the listed regular file")
					}
				}
			}
			err := k.c.validateRecovery()
			switch row {
			case "each_pass":
				checkpointDiagnosticReason(t, err, "tree_unreadable")
				if passes != 3 {
					t.Fatal("recurring disappearance did not exhaust exactly three snapshot passes")
				}
			case "moved_boundary":
				checkpointDiagnosticReason(t, err, "tree_unreadable")
				if passes != 1 {
					t.Fatal("an unreadable held boundary incorrectly retried the snapshot")
				}
			default:
				if err != nil {
					t.Fatal("the snapshot refused a single disappearing entry")
				}
				if passes != 2 {
					t.Fatal("the snapshot did not retry a single disappearing entry exactly once")
				}
			}
		})
	}
}

func TestCheckpointTreeSnapshotKinds(t *testing.T) {
	for _, kind := range []string{"link", "fifo", "file_mode", "directory_mode"} {
		t.Run(kind, func(t *testing.T) {
			k := checkpointInspectCase(t)
			dir := checkpointPlant(t, k.c, heldTarget, heldTarget)
			name := filepath.Join(dir, "object")
			var err error
			switch kind {
			case "link":
				err = os.Symlink("missing", name)
			case "fifo":
				err = syscall.Mkfifo(name, 0o600)
			case "file_mode":
				err = os.WriteFile(name, nil, 0o600)
				if err == nil {
					err = os.Chmod(name, 0o644)
				}
			case "directory_mode":
				err = os.Mkdir(name, 0o700)
				if err == nil {
					err = os.Chmod(name, 0o755)
				}
			}
			if err != nil {
				t.Fatal("cannot prepare the refused snapshot entry")
			}
			checkpointDiagnosticPublic(t, k, "descendant_refused")
		})
	}
}
