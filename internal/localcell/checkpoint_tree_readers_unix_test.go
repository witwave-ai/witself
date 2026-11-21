//go:build linux || darwin

package localcell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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
	if sums, unknown, err := c.listCheckpoints(context.Background()); err != nil || len(sums) != 0 || unknown != 0 {
		t.Fatal("the missing recovery boundary did not yield an empty listing")
	}
	if os.Symlink("missing", c.recoveryDir()) != nil {
		t.Fatal("cannot plant the dangling boundary link")
	}
	_, _, err := c.listCheckpoints(context.Background())
	if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "boundary_unreadable"}) {
		t.Fatal("the dangling recovery boundary was reported empty or with another refusal")
	}
	c.cfg.Root = filepath.Join(c.cfg.Root, "missing-cell")
	if _, _, err := c.listCheckpoints(context.Background()); ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: "boundary_unreadable"}) {
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

// checkpointSizedName constructs an absolute name of exactly size bytes without
// exceeding the component limit. The target is sufficiently long to add a component.
func checkpointSizedName(base string, size int) string {
	for size-len(base) > 250 {
		base = filepath.Join(base, strings.Repeat("p", 199))
	}
	return filepath.Join(base, strings.Repeat("p", size-len(base)-1))
}

func TestCheckpointTreePathLimit(t *testing.T) {
	const failed, state, incomplete = "local_checkpoint_operation_failed", "local_checkpoint_state_unsupported", "local_checkpoint_incomplete"
	const named = len("/recovery/") + len(heldTarget)
	const witnessed = named + len("/witness.json")
	const fence = "local_maintenance_fence_active"
	type answers struct{ before, create, delete, repair, after string }
	for _, row := range []struct {
		name, witness string
		suffix        int // absolute cell root length is PATH_MAX minus suffix
		want          answers
		failure       string
	}{
		{"deep_file", heldTarget, 0, answers{"complete", "", "", "", "copy"}, "deep private file changed the checkpoint sequence"},
		{"deep_directory", heldTarget, 0, answers{"complete", "", "", "", "copy"}, "deep private directory changed the checkpoint sequence"},
		{"witness_absent", "", witnessed, answers{"incomplete", incomplete, "", "", ""}, "absent long witness changed the checkpoint sequence"},
		{"witness_present", heldTarget, witnessed, answers{"complete", "", "", "", "copy"}, "present long witness changed the checkpoint sequence"},
		// delete reaches the checkpoint through the held recovery/, so an overlong own name is deleted (LC-CKPT-WALK-1C3B).
		{"checkpoint_absent_witness", "", named, answers{"incomplete", incomplete, "", "", ""}, "overlong checkpoint name with absent witness changed the sequence"},
		{"checkpoint_present_witness", heldTarget, named, answers{"complete", failed, "", "", ""}, "overlong checkpoint name with present witness changed the sequence"},
		{"maintenance_name", heldTarget, len("/journal/maintenance.json"), answers{failed, fence, fence, fence, failed}, "an overlong maintenance name changed the pre-admission refusal"},
		{"operation_lock_name", heldTarget, len("/run/operation.lock"), answers{failed, failed, failed, failed, failed}, "an overlong operation lock changed the pre-admission refusal"},
		{"empty_boundary_with_dot", "", len("/recovery") + 2, answers{failed, failed, failed, failed, failed}, "overlong directory-only boundary suffix changed the checkpoint sequence"},
		{"empty_boundary", "", len("/recovery") + 1, answers{failed, failed, failed, failed, failed}, "overlong empty recovery boundary changed the checkpoint sequence"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c := f.cell
			base, err := filepath.EvalSymlinks(testPrivateRoot(t))
			size := unix.PathMax - row.suffix
			if row.suffix == 0 {
				size = len(base) + 80
			}
			long := checkpointSizedName(base, size)
			if err != nil || os.MkdirAll(filepath.Dir(long), 0o700) != nil {
				t.Fatal("cannot prepare the long private cell parent")
			}
			if strings.HasPrefix(row.name, "empty_boundary") {
				if os.Mkdir(c.recoveryDir(), 0o700) != nil {
					t.Fatal("cannot prepare the empty recovery boundary")
				}
			} else {
				dir := checkpointPlant(t, c, heldTarget, row.witness)
				if row.suffix == 0 {
					// Lengthen the root only after building the fixture so the leaf is exactly PATH_MAX.
					parent := checkpointSizedName(dir, unix.PathMax-(len(long)-len(c.cfg.Root))-2)
					if err := os.MkdirAll(parent, 0o700); err != nil {
						t.Fatal("cannot prepare deep private directories")
					}
					if row.name == "deep_file" {
						err = os.WriteFile(filepath.Join(parent, "x"), nil, 0o600)
					} else {
						err = os.Mkdir(filepath.Join(parent, "x"), 0o700)
					}
					if err != nil {
						t.Fatal("cannot prepare the private leaf at the pathname limit")
					}
				}
			}
			heldMove(t, c.cfg.Root, long)
			c.cfg.Root = long
			if strings.HasPrefix(row.name, "empty_boundary") {
				checkpointDiagnosticReason(t, c.validateRecovery(), "boundary_unreadable")
				_, _, err := c.listCheckpoints(context.Background())
				checkpointDiagnosticReason(t, err, "boundary_unreadable")
			}
			read := func() string {
				i, err := c.Inspect(t.Context())
				if err != nil {
					if ClassifyCheckpointFailure(err) == (CheckpointFailure{Class: failed}) {
						return failed
					}
					return "unexpected inspection failure"
				}
				if i.CheckpointListing != "listed" || i.CheckpointUnknown != 0 || len(i.Checkpoints) > 1 {
					return "unexpected listing"
				}
				if len(i.Checkpoints) == 0 {
					return ""
				}
				s := i.Checkpoints[0]
				switch {
				case s.ID == heldTarget && s.Complete:
					return "complete"
				case s.ID == heldTarget:
					return "incomplete"
				case s.Complete:
					return "copy"
				}
				return "unexpected incomplete copy"
			}
			before := read()
			create := c.Checkpoint(t.Context(), checkpointOptions(f))
			deletion := c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: heldTarget})
			repair := c.Repair(t.Context())
			if read() != row.want.after || before != row.want.before ||
				ClassifyCheckpointFailure(create) != (CheckpointFailure{Class: row.want.create}) ||
				ClassifyCheckpointFailure(deletion) != (CheckpointFailure{Class: row.want.delete}) ||
				ClassifyCheckpointFailure(repair) != (CheckpointFailure{Class: row.want.repair}) {
				t.Fatal(row.failure)
			}
		})
	}
}
