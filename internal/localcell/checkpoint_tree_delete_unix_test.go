//go:build linux || darwin

package localcell

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// treeDeletePlant moves the target to the cell's tmp/ and puts kind at its name:
// a link to the other checkpoint or to a directory in recovery/, a file, a FIFO,
// the other checkpoint renamed there; recovery moves recovery/ out and links it.
func treeDeletePlant(t *testing.T, c *Cell, kind string) {
	t.Helper()
	target, err := c.path("recovery", heldTarget), error(nil)
	if kind == "recovery" {
		moved := filepath.Join(testPrivateRoot(t), "recovery")
		heldMove(t, c.recoveryDir(), moved)
		drillRootLink(t, c.recoveryDir(), moved)
		return
	}
	heldMove(t, target, c.path("tmp", "moved"))
	switch kind {
	case "link_other":
		err = os.Symlink(heldOther, target)
	case "link_ckpt":
		err = errors.Join(os.Mkdir(c.path("recovery", "linked"), 0o700), os.Symlink("linked", target))
	case "file":
		err = os.WriteFile(target, []byte("placed at the name"), 0o600)
	case "fifo":
		err = syscall.Mkfifo(target, 0o600)
	case "dir":
		err = os.Rename(c.path("recovery", heldOther), target)
	}
	if err != nil {
		t.Fatal("cannot plant at the held deletion name")
	}
}

// treeDeleteKept is every directory a planted deletion must leave as it was.
func treeDeleteKept(t *testing.T, c *Cell) heldTrees {
	if moved, err := os.Readlink(c.recoveryDir()); err == nil {
		return heldTrees{moved: heldTree(t, moved)}
	}
	kept := heldTrees{}
	for _, dir := range []string{c.path("tmp", "moved"), c.path("recovery", heldOther), c.path("recovery", "linked"), c.path("recovery", heldTarget)} {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			kept[dir] = heldTree(t, dir)
		}
	}
	return kept
}

// At the hold a link, file or FIFO at the name refuses before anything is
// journaled; at the unseal, sweep and directory steps any plant (the other
// checkpoint renamed onto the name, recovery/ linked out too) fails that step.
func TestCheckpointTreeDeleteStages(t *testing.T) {
	for _, stage := range []string{"hold", "unseal", "sweep", "directory"} {
		kinds := []string{"link_other", "link_ckpt", "file", "fifo"}
		if stage != "hold" {
			kinds = append(kinds, "dir", "recovery")
		}
		for _, kind := range kinds {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				f, c := heldFixture(t)
				var kept heldTrees
				calls := heldAt(c, stage, func() { treeDeletePlant(t, c, kind); kept = treeDeleteKept(t, c) })
				err := heldDelete(t, c)
				if calls[stage] == 0 {
					t.Fatal("the held deletion row did not reach its stage")
				}
				if stage == "hold" {
					heldRefused(t, f, err, "local_checkpoint_state_unsupported", nil)
				} else {
					heldFailed(t, f, err, map[bool]string{true: "unseal", false: "remove"}[stage == "unseal"])
				}
				heldSame(t, kept)
			})
		}
	}
}

// A directory below the target on another device (the seam says so) stops the
// sweep before anything on it is removed (W5).
func TestCheckpointTreeDeleteSweepDevice(t *testing.T) {
	f, c := heldFixture(t)
	original, kept := privateBeneathSameDevice, heldTrees{}
	t.Cleanup(func() { privateBeneathSameDevice = original })
	for _, name := range []string{"pg", "run"} {
		kept[c.path("recovery", heldTarget, name)] = heldTree(t, c.path("recovery", heldTarget, name))
	}
	privateBeneathSameDevice = func(uint64, uint64) bool { return false }
	heldFailed(t, f, heldDelete(t, c), "remove")
	heldSame(t, kept)
}

func TestCheckpointTreeDeleteDescriptorLifetime(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // a finalizer must not conceal a leaked descriptor
	_, c := heldFixture(t)
	target := treeDirectoryStat(t, c.path("recovery", heldTarget))
	before, during := treeDescriptorCount(t, target), 0
	calls := heldAt(c, "unseal", func() { during = treeDescriptorCount(t, target) })
	err := heldDelete(t, c)
	after := treeDescriptorCount(t, target)

	t.Logf("%d/%d/%d", before, during, after)
	if err != nil {
		t.Error("the descriptor lifetime delete failed")
	}
	if calls["unseal"] != 1 {
		t.Error("the descriptor lifetime delete did not reach unseal exactly once")
	}
	if before != 0 {
		t.Error("a target descriptor was open before delete")
	}
	if during != 1 {
		t.Error("delete did not hold exactly one target descriptor at unseal")
	}
	if after != 0 {
		t.Error("a target descriptor remained open after delete")
	}
	inspection, err := c.Inspect(t.Context())
	if err != nil || len(inspection.Checkpoints) != 1 || inspection.Checkpoints[0].ID != heldOther || !inspection.Checkpoints[0].Complete {
		t.Error("the descriptor lifetime delete did not leave only the complete sibling")
	}
}

func TestCheckpointTreeSweepUnrecorded(t *testing.T) {
	_, c := heldFixture(t)
	tree := c.checkpointTree(heldTarget)
	defer tree.close()
	if tree.hold() != nil {
		t.Fatal("cannot hold the recovery directory for the unrecorded sweep")
	}
	before := checkpointTreeState(t, c)
	if !errors.Is(tree.sweep(), errPrivateBeneathMoved) {
		t.Error("the unrecorded sweep did not refuse the missing identity")
	}
	if !reflect.DeepEqual(before, checkpointTreeState(t, c)) {
		t.Error("the unrecorded sweep changed the checkpoint tree")
	}
}

// An identifier that is not a checkpoint's adopts and removes nothing.
func TestCheckpointTreeDeleteIdentifier(t *testing.T) {
	_, c := heldFixture(t)
	before, tree := checkpointTreeState(t, c), c.checkpointTree(".")
	defer tree.close()
	if !errors.Is(tree.adopt(), syscall.EINVAL) || tree.sweep() == nil || !reflect.DeepEqual(before, checkpointTreeState(t, c)) {
		t.Fatal("a delete tree accepted an identifier that is not a checkpoint")
	}
}

// delete's admission reads its target through the held recovery/: gone, made
// non-private, or recovery/ linked to a tree without it after the snapshot
// listed its last directory, or present only under another spelling (a
// case-insensitive volume), it refuses unjournaled.
func TestCheckpointTreeDeleteTarget(t *testing.T) {
	for _, row := range []struct{ name, want string }{
		{"absent", "local_checkpoint_identity_mismatch"},
		{"not_private", "local_checkpoint_state_unsupported"},
		{"other_spelling", "local_checkpoint_identity_mismatch"},
		{"recovery_linked", "local_checkpoint_state_unsupported"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, c := heldFixture(t)
			target, kept, directories, seen := c.path("recovery", heldTarget), heldTrees{}, 0, 0
			act := func(moved string) {
				if row.name == "recovery_linked" {
					outside := testPrivateRoot(t)
					heldMove(t, c.recoveryDir(), filepath.Join(outside, "moved"))
					drillRootLink(t, c.recoveryDir(), outside)
					target = filepath.Join(outside, "moved")
				} else if moved != "" {
					heldMove(t, target, moved)
					target = moved
				} else if os.Chmod(target, 0o755) != nil {
					t.Fatal("cannot loosen the target fixture")
				}
				kept[target] = heldTree(t, target)
			}
			if row.name == "other_spelling" {
				act(c.path("recovery", strings.ToUpper(heldTarget)))
			}
			_ = filepath.WalkDir(c.recoveryDir(), func(_ string, entry fs.DirEntry, err error) error {
				directories += map[bool]int{true: 1}[err == nil && entry.IsDir()]
				return nil
			})
			original := checkpointTreeListed
			t.Cleanup(func() { checkpointTreeListed = original })
			checkpointTreeListed = func(string) {
				if seen++; seen == directories && row.name != "other_spelling" {
					act(map[bool]string{true: c.path("tmp", "moved")}[row.name == "absent"])
				}
			}
			err := heldDelete(t, c)
			if seen < directories || len(kept) == 0 {
				t.Fatal("the target row did not reach the last listing")
			}
			heldRefused(t, f, err, row.want, nil)
			heldSame(t, kept)
		})
	}
}

// treeWitnessPlant replaces what stands at the witness name with kind.
func treeWitnessPlant(t *testing.T, witness, kind, outside string) {
	t.Helper()
	err := os.Remove(witness)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	err = errors.Join(err, map[string]func() error{"dir": func() error { return os.Mkdir(witness, 0o700) },
		"link": func() error { return os.Symlink(outside, witness) }, "dangling": func() error { return os.Symlink("missing", witness) },
		"fifo": func() error { return syscall.Mkfifo(witness, 0o600) }}[kind]())
	if err != nil {
		t.Fatal("cannot plant at the witness name")
	}
}

// A witness name holding a directory, link, dangling link or FIFO after the
// admission is removed as itself, its target kept, and the checkpoint deleted.
func TestCheckpointTreeDeleteWitness(t *testing.T) {
	for _, kind := range []string{"dir", "link", "dangling", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f, c := heldFixture(t)
			outside, other := heldOutside(t), c.path("recovery", heldOther)
			kept := heldTrees{outside: heldTree(t, outside), other: heldTree(t, other)}
			calls := heldAt(c, "hold", func() {
				treeWitnessPlant(t, c.path("recovery", heldTarget, checkpointWitnessName), kind, filepath.Join(outside, heldTarget, "sentinel"))
			})
			if err := heldDelete(t, c); calls["hold"] == 0 || err != nil || !slices.Equal(checkpointIDs(t, c), []string{heldOther}) {
				t.Fatal("a deletion did not delete a checkpoint whose witness is not a regular file")
			}
			retainedAssertRecords(t, f, checkpointDeleteRecords)
			heldSame(t, kept)
		})
	}
}

// The failed create's discard: a regular witness it published, still bound, is
// not a failed discard (own: the sync of the witness step's done record fails);
// a directory, link, dangling link or FIFO there is no witness and goes with the
// call's directory, cleanup complete.
func TestCheckpointTreeDiscardWitness(t *testing.T) {
	for _, kind := range []string{"own", "dir", "link", "dangling", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f, r := checkpointFixture(t, "legacy", false)
			c, outside := f.cell, heldOutside(t)
			r.nextID = func() (string, error) { return heldTarget, nil }
			kept, armed, reached := heldTrees{outside: heldTree(t, outside)}, false, false
			original, control := syncFile, c.checkpointTest.control
			t.Cleanup(func() { syncFile = original })
			syncFile = func(file *os.File) error {
				if armed && kind == "own" && filepath.Base(file.Name()) == "operations.jsonl" {
					armed, reached = false, true
					return errors.New("synthetic journal sync failure")
				}
				return original(file)
			}
			c.checkpointTest.control = func(ctx context.Context) (pgControlData, error) {
				if armed = r.calls["control"] == 1; armed && kind != "own" {
					reached = true
					treeWitnessPlant(t, c.path("recovery", heldTarget, checkpointWitnessName), kind, filepath.Join(outside, heldTarget, "sentinel"))
				}
				return control(ctx)
			}
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			_, nameErr := os.Lstat(c.path("recovery", heldTarget))
			if !reached || !readers5JournalClosed(c) {
				t.Fatal("the discard witness row did not reach its plant or left its journal open")
			}
			if kind == "own" {
				if !checkpointSameError(err, "cannot sync local cell journal") || ClassifyCheckpointFailure(err).CleanupIncomplete || !checkpointSummaryOf(t, c, heldTarget).Complete {
					t.Fatal("a failed checkpoint discarded or reported its own witnessed checkpoint")
				}
			} else if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_copy_failed"}) || !errors.Is(nameErr, fs.ErrNotExist) {
				t.Fatal("a failed checkpoint kept its directory for an object at its witness name")
			}
			heldSame(t, kept)
		})
	}
}

// A directory this call recorded, moved out of the cell at the third
// publication with nothing put at its name, is not cleaned up: it survives.
func TestCheckpointTreeDiscardMoved(t *testing.T) {
	f, r := checkpointFixture(t, "legacy", false)
	c := f.cell
	r.nextID = func() (string, error) { return heldTarget, nil }
	moved, kept, calls := filepath.Join(testPrivateRoot(t), "moved"), heldTrees{}, map[string]int{}
	c.checkpointTest.beforeStep = func(step, _ string) {
		if calls[step]++; step == "publish" && calls[step] == 3 {
			heldMove(t, c.path("recovery", heldTarget), moved)
			kept[moved] = heldTree(t, moved)
		}
	}
	err := c.Checkpoint(t.Context(), checkpointOptions(f))
	if calls["publish"] < 3 {
		t.Fatal("the moved discard row did not reach its publication")
	}
	if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_copy_failed", CleanupIncomplete: true}) {
		t.Fatal("a failed checkpoint reported the cleanup of a directory that left its name")
	}
	if _, nameErr := os.Lstat(c.path("recovery", heldTarget)); !errors.Is(nameErr, fs.ErrNotExist) || !readers5JournalClosed(c) {
		t.Fatal("the moved discard row left something at the name or its journal open")
	}
	heldSame(t, kept)
}

// A copy that made nothing at its name (recovery/ made read-only right after the
// copy created it, so the mkdir is refused) is cleaned up once recovery/ is
// synced through the held root; a failed sync is incomplete.
func TestCheckpointTreeDiscardEmpty(t *testing.T) {
	for _, row := range []string{"synced", "sync_fails"} {
		t.Run(row, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root makes directories in a read-only directory")
			}
			f, r := checkpointFixture(t, "legacy", false)
			c := f.cell
			r.nextID = func() (string, error) { return heldTarget, nil }
			original, originalRoot, restricted, synced := syncPrivateDirParent, syncCheckpointRoot, false, 0
			t.Cleanup(func() {
				syncPrivateDirParent, syncCheckpointRoot = original, originalRoot
				_ = os.Chmod(c.recoveryDir(), 0o700)
			})
			syncPrivateDirParent = func(dir string) error {
				if _, err := os.Lstat(c.recoveryDir()); err == nil && dir == c.cfg.Root && !restricted {
					restricted = os.Chmod(c.recoveryDir(), 0o500) == nil
				}
				return original(dir)
			}
			syncCheckpointRoot = func(held *os.Root) error {
				if filepath.Clean(held.Name()) != c.recoveryDir() {
					return originalRoot(held)
				} else if synced++; row == "sync_fails" {
					return errors.New("synthetic root sync failure")
				}
				return originalRoot(held)
			}
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			if !restricted {
				t.Fatal("the empty discard row did not restrict its recovery directory")
			}
			if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_copy_failed", CleanupIncomplete: row == "sync_fails"}) || synced != 1 {
				t.Fatal("an empty cleanup name was not synced once through the held root, or misreported")
			}
			if _, nameErr := os.Lstat(c.path("recovery", heldTarget)); !errors.Is(nameErr, fs.ErrNotExist) {
				t.Fatal("the empty discard row found something at the name")
			}
		})
	}
}

// The readers' binds and descriptors no other row observes (1C2R N1): the
// witness fallback binds before it says incomplete (Y05), the listing binds
// what it holds (Y21), and each listed checkpoint stays open for its witness
// read (Y11).
func TestCheckpointTreeReaderBinds(t *testing.T) {
	t.Run("witness_fallback", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		c := f.cell
		if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || os.Remove(c.path("recovery", checkpointIDs(t, c)[0], checkpointWitnessName)) != nil {
			t.Fatal("cannot prepare the witness fallback row")
		}
		calls := heldAt(c, "listed", func() { heldMove(t, c.recoveryDir(), c.path("moved")) })
		inspection, err := c.Inspect(t.Context())
		if calls["listed"] == 0 || ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_operation_failed"}) || len(inspection.Checkpoints) != 0 {
			t.Fatal("a moved recovery directory listed an incomplete checkpoint")
		}
	})
	t.Run("listing_hold", func(t *testing.T) {
		f, _ := checkpointFixture(t, "legacy", false)
		c, original, swapped := f.cell, checkpointTreeListed, false
		t.Cleanup(func() { checkpointTreeListed = original })
		checkpointTreeListed = func(name string) {
			if name == "." && !swapped {
				swapped = true
				heldMove(t, c.recoveryDir(), c.path("moved"))
				drillRootLink(t, c.recoveryDir(), c.path("moved"))
			}
		}
		if privateDir(c.recoveryDir(), true) != nil {
			t.Fatal("cannot prepare the empty recovery directory")
		}
		_, err := c.Inspect(t.Context())
		if !swapped {
			t.Fatal("the listing hold row did not reach the snapshot listing")
		}
		checkpointDiagnosticReason(t, err, "boundary_unreadable")
	})
	t.Run("listed_retained", func(t *testing.T) {
		_, c := heldFixture(t)
		held := map[string]bool{}
		calls := heldAt(c, "listed", func() {
			for _, id := range []string{heldTarget, heldOther} {
				held[id] = treeDescriptorCount(t, treeDirectoryStat(t, c.path("recovery", id))) > 0
			}
		})
		if _, err := c.Inspect(t.Context()); err != nil || calls["listed"] == 0 || !held[heldTarget] || !held[heldOther] {
			t.Fatal("a listed checkpoint directory was not held open for its witness read")
		}
	})
}

func treeDirectoryStat(t *testing.T, dir string) *syscall.Stat_t {
	t.Helper()
	var stat syscall.Stat_t
	if syscall.Lstat(dir, &stat) != nil {
		t.Fatal("cannot identify the checkpoint directory for the descriptor count")
	}
	return &stat
}

// Count descriptors by the original identity, even after the name is removed.
func treeDescriptorCount(t *testing.T, want *syscall.Stat_t) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal("cannot list this process's descriptors")
	}
	count := 0
	for _, entry := range entries {
		var stat syscall.Stat_t
		fd, err := strconv.Atoi(entry.Name())
		if err == nil && syscall.Fstat(fd, &stat) == nil && stat.Dev == want.Dev && stat.Ino == want.Ino {
			count++
		}
	}
	return count
}
