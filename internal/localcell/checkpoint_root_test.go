package localcell

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const heldTarget, heldOther = "ckpt_aaaaaaaaaaaaaaaa", "ckpt_bbbbbbbbbbbbbbbb"

// heldPlatformRows are the per-OS rows. The link and lock fixtures are the
// drill root's per-OS drillRootLink (Unix symlink, Windows junction) and
// drillRootLock (Unix: a 0500 directory, skipped as root; Windows: the file
// held without delete sharing); their defaults skip.
var heldPlatformRows = func(*testing.T) {}

// heldIdentifierRow is the Windows held-root helpers' identifier row; Linux
// and macOS remove through the tree instead.
var heldIdentifierRow = func(*testing.T) {}

type heldTrees = map[string]map[string]retainedEntry

// heldFixture makes two real checkpoints, heldTarget first, then resets the
// journal prefix.
func heldFixture(t *testing.T) (*retainedFixture, *Cell) {
	t.Helper()
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return []string{heldTarget, heldOther}[r.calls["newID"]-1], nil }
	if errors.Join(f.cell.Checkpoint(t.Context(), checkpointOptions(f)), f.cell.Checkpoint(t.Context(), checkpointOptions(f))) != nil {
		t.Fatal("cannot prepare the held deletion checkpoints")
	}
	f.prefix = retainedRead(t, f.cell, "journal/operations.jsonl")
	return f, f.cell
}

func heldDelete(t *testing.T, c *Cell) error {
	return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: heldTarget})
}

// heldAt counts every stage call and runs act on the first call of stage.
func heldAt(c *Cell, stage string, act func()) map[string]int {
	calls := map[string]int{}
	c.checkpointTest.beforeStep = func(step, _ string) {
		if calls[step]++; step == stage && calls[step] == 1 {
			act()
		}
	}
	return calls
}

// heldUnsealed runs act once the unseal step has succeeded.
func heldUnsealed(c *Cell, act func()) {
	c.cfg.Failpoint = func(operation, step string) error {
		if operation == "checkpoint_delete" && step == "unseal" {
			act()
		}
		return nil
	}
}

// heldTree is the mode and byte state of the tree at dir; links are not followed.
func heldTree(t *testing.T, dir string) map[string]retainedEntry {
	return checkpointTreeState(t, &Cell{cfg: Config{Root: dir}})
}

// heldSame fails unless each directory's tree equals its kept state.
func heldSame(t *testing.T, kept heldTrees) {
	t.Helper()
	for dir, tree := range kept {
		if _, err := os.Lstat(dir); err != nil || !reflect.DeepEqual(tree, heldTree(t, dir)) {
			t.Fatal("a held deletion changed a kept tree")
		}
	}
}

func heldMove(t *testing.T, from, to string) {
	if os.Rename(from, to) != nil {
		t.Fatal("cannot move a held deletion fixture")
	}
}

// heldOutside is a private directory outside the cell holding heldTarget/sentinel.
func heldOutside(t *testing.T) string {
	t.Helper()
	outside := testPrivateRoot(t)
	if testMkdir(filepath.Join(outside, heldTarget), 0o700) != nil ||
		testWriteFile(filepath.Join(outside, heldTarget, "sentinel"), []byte("outside sentinel"), 0o600) != nil {
		t.Fatal("cannot prepare the outside directory")
	}
	return outside
}

// heldRefused requires err to be want, by fixed cause or exact text, exactly
// records after the prefix, and a closed journal.
func heldRefused(t *testing.T, f *retainedFixture, err error, want string, records []string) {
	t.Helper()
	retainedAssertRecords(t, f, records)
	if p, journalErr := f.cell.readJournal(context.Background()); !checkpointSameError(err, want) || journalErr != nil || p.pending != nil || p.tornTail {
		t.Fatal("a held deletion did not refuse with its fixed error and a closed journal")
	}
}

// heldFailed requires the fixed failure of one deletion step.
func heldFailed(t *testing.T, f *retainedFixture, err error, step string) {
	t.Helper()
	heldRefused(t, f, err, "local cell checkpoint_delete: "+step+" failed", slices.Concat(checkpointDeleteRecords[:map[string]int{"unseal": 2, "remove": 4}[step]],
		[]string{"checkpoint_delete/" + step + "/failed", "checkpoint_delete/operation/done"}))
}

// heldDiscard runs a Checkpoint of heldTarget whose copy is byte-flipped after
// the copy, so it fails and discards its own directory; arm runs first. A step
// "copy" fails the copy step instead (a publish fails), "witness" the witness
// step (the second control reading moved).
func heldDiscard(t *testing.T, arm func(*Cell), step ...string) (*Cell, CheckpointFailure) {
	t.Helper()
	f, r := checkpointFixture(t, "legacy", false)
	r.nextID = func() (string, error) { return heldTarget, nil }
	r.after = func(dir string) { checkpointRewrite(t, filepath.Join(dir, "pg", "base", "1", "1259")) }
	want, moved := "local_checkpoint_witness_mismatch", checkpointSyntheticControl
	if moved.CheckpointLSN = "0/1548FC0"; slices.Contains(step, "copy") {
		want, f.cell.checkpointTest.publish = "local_checkpoint_copy_failed", func(string, string) error { return errors.New("synthetic publish failure") }
	} else if slices.Contains(step, "witness") {
		r.after, r.readings = func(string) {}, []pgControlData{checkpointSyntheticControl, moved}
	}
	arm(f.cell)
	err := f.cell.Checkpoint(t.Context(), checkpointOptions(f))
	if !checkpointSameError(err, want) {
		t.Fatal("the failed checkpoint did not report its fixed failure cause")
	}
	return f.cell, ClassifyCheckpointFailure(err)
}

func TestDeleteCheckpointHeld(t *testing.T) {
	t.Run("stages", func(t *testing.T) {
		f, c := heldFixture(t)
		calls, before := heldAt(c, "", nil), checkpointTreeState(t, c)
		err := heldDelete(t, c)
		retainedAssertRecords(t, f, checkpointDeleteRecords)
		if err != nil || !maps.Equal(calls, map[string]int{"hold": 1, "unseal": 1, "sweep": 1, "directory": 1}) || len(checkpointIDs(t, c)) != 1 ||
			!reflect.DeepEqual(checkpointWithout(before, heldTarget), checkpointWithout(checkpointTreeState(t, c), heldTarget)) {
			t.Fatal("a held deletion did not remove exactly its target through each stage once")
		}
	})
	// recovery/ moved and linked to an outside directory before the hold.
	t.Run("recovery_linked", func(t *testing.T) {
		f, c := heldFixture(t)
		outside, moved := heldOutside(t), c.path("moved")
		kept := heldTrees{moved: heldTree(t, c.recoveryDir()), outside: heldTree(t, outside)}
		heldAt(c, "hold", func() { heldMove(t, c.recoveryDir(), moved); drillRootLink(t, c.recoveryDir(), outside) })
		// Linux and macOS refuse at the hold, nothing journaled; Windows' held roots at the unseal.
		if err := heldDelete(t, c); runtime.GOOS == "windows" {
			heldFailed(t, f, err, "unseal")
		} else {
			heldRefused(t, f, err, "local_checkpoint_state_unsupported", nil)
		}
		heldSame(t, kept)
	})
	// The held target moved aside and another tree renamed to its name.
	for _, row := range []struct {
		name   string
		source []string
		at     string // a stage, or "" once unsealed
		step   string
	}{
		{"kept_checkpoint", []string{"recovery", heldOther}, "", "remove"},
		{"live_database", []string{"pg"}, "", "remove"},
		{"unseal_swap", []string{"recovery", heldOther}, "unseal", "unseal"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, c := heldFixture(t)
			swapped, target, aside := c.path(row.source...), c.path("recovery", heldTarget), c.path("aside")
			kept := heldTrees{aside: heldTree(t, target), target: heldTree(t, swapped)}
			if swap := func() { heldMove(t, target, aside); heldMove(t, swapped, target) }; row.at == "" {
				delete(kept[aside], checkpointWitnessName)
				heldUnsealed(c, swap)
			} else {
				heldAt(c, row.at, swap)
			}
			heldFailed(t, f, heldDelete(t, c), row.step)
			heldSame(t, kept)
		})
	}
	// A private file put at a name after moving its directory away.
	for _, row := range []struct {
		name, at, moved string
		step            string // "" when the hold refuses
	}{
		{"recovery_file", "hold", "recovery", ""},
		{"name_file", "hold", "recovery/" + heldTarget, ""},
		{"final_swap", "directory", "recovery/" + heldTarget, "remove"},
	} {
		t.Run(row.name, func(t *testing.T) {
			f, c := heldFixture(t)
			name, moved, other := c.path(filepath.FromSlash(row.moved)), c.path("moved"), c.path("recovery", heldOther)
			kept := heldTrees{moved: heldTree(t, name)}
			if row.step != "" {
				kept = heldTrees{other: heldTree(t, other)} // the sweep empties the moved directory
			}
			heldAt(c, row.at, func() { heldMove(t, name, moved); checkpointWrite(t, c, row.moved, "placed at the name") })
			if err := heldDelete(t, c); row.step == "" {
				heldRefused(t, f, err, "local_checkpoint_state_unsupported", nil)
				// The class and reason, read on Linux, of a delete whose directory is not a directory.
				if ClassifyCheckpointFailure(err) != (CheckpointFailure{Class: "local_checkpoint_state_unsupported", Reason: ""}) {
					t.Fatal("a held deletion refused a file at a name with another class or reason")
				}
			} else {
				heldFailed(t, f, err, row.step)
			}
			if heldSame(t, kept); string(retainedRead(t, c, row.moved)) != "placed at the name" {
				t.Fatal("a held deletion removed the file put at a name")
			}
		})
	}
	t.Run("late_entry", func(t *testing.T) {
		f, c := heldFixture(t)
		heldAt(c, "directory", func() { checkpointWrite(t, c, "recovery/"+heldTarget+"/late", "late entry") })
		heldFailed(t, f, heldDelete(t, c), "remove")
		if c.checkpointTest.beforeStep = nil; heldDelete(t, c) != nil || !slices.Equal(checkpointIDs(t, c), []string{heldOther}) {
			t.Fatal("a held deletion removed a late entry with its directory, or did not finish on retry")
		}
	})
	t.Run("witness_not_file", func(t *testing.T) {
		f, c := heldFixture(t)
		witness, kept := c.path("recovery", heldTarget, checkpointWitnessName), heldTrees{}
		heldAt(c, "unseal", func() {
			if os.Remove(witness) != nil || testMkdir(witness, 0o700) != nil {
				t.Fatal("cannot replace the witness")
			}
			checkpointWrite(t, c, "recovery/"+heldTarget+"/"+checkpointWitnessName+"/inner", "witness content")
			kept[c.recoveryDir()] = heldTree(t, c.recoveryDir())
		})
		heldFailed(t, f, heldDelete(t, c), "unseal")
		if heldSame(t, kept); len(checkpointIDs(t, c)) != 2 {
			t.Fatal("a held deletion removed a witness that is a directory with content")
		}
	})
	t.Run("locked_entry", func(t *testing.T) {
		f, c := heldFixture(t)
		release := func() {}
		calls := heldAt(c, "sweep", func() { release = drillRootLock(t, c.path("recovery", heldTarget, "pg", "base", "1", "1259")) })
		heldFailed(t, f, heldDelete(t, c), "remove")
		release()
		if c.checkpointTest.beforeStep = nil; calls["directory"] != 0 || heldDelete(t, c) != nil || len(checkpointIDs(t, c)) != 1 {
			t.Fatal("a held deletion hid a failed removal, or did not finish on retry")
		}
	})
	t.Run("link_inside", func(t *testing.T) {
		_, c := heldFixture(t)
		kept := heldTrees{c.path("pg"): heldTree(t, c.path("pg"))}
		heldAt(c, "sweep", func() { drillRootLink(t, c.path("recovery", heldTarget, "pg", "live"), c.path("pg")) })
		if err := heldDelete(t, c); err != nil || len(checkpointIDs(t, c)) != 1 {
			t.Fatal("a held deletion refused a link inside its checkpoint")
		}
		heldSame(t, kept)
	})
	t.Run("sync_failure", func(t *testing.T) {
		f, c := heldFixture(t)
		original, other := syncCheckpointRoot, c.path("recovery", heldOther)
		t.Cleanup(func() { syncCheckpointRoot = original })
		syncCheckpointRoot = func(held *os.Root) error {
			if filepath.Clean(held.Name()) == c.recoveryDir() {
				return errors.New("synthetic root sync failure")
			}
			return original(held)
		}
		kept := heldTrees{other: heldTree(t, other)}
		heldFailed(t, f, heldDelete(t, c), "remove")
		if heldSame(t, kept); !checkpointSameError(heldDelete(t, c), "local_checkpoint_identity_mismatch") {
			t.Fatal("a failed recovery sync did not leave its target absent")
		}
	})
	// The create's failure cleanup goes through the same held roots.
	t.Run("discard_plain", func(t *testing.T) {
		c, failure := heldDiscard(t, func(*Cell) {})
		if failure.CleanupIncomplete || len(checkpointIDs(t, c)) != 0 || os.Rename(c.recoveryDir(), c.path("moved")) != nil {
			t.Fatal("a failed checkpoint did not discard its directory, or kept its roots open")
		}
	})
	t.Run("discard_linked", func(t *testing.T) {
		outside := heldOutside(t)
		kept := heldTrees{outside: heldTree(t, outside)}
		_, failure := heldDiscard(t, func(c *Cell) {
			heldAt(c, "hold", func() { heldMove(t, c.recoveryDir(), c.path("moved")); drillRootLink(t, c.recoveryDir(), outside) })
		})
		if heldSame(t, kept); !failure.CleanupIncomplete {
			t.Fatal("a failed checkpoint reported a discard it refused")
		}
	})
	t.Run("discard_name_file", func(t *testing.T) {
		c, failure := heldDiscard(t, func(c *Cell) {
			heldAt(c, "hold", func() {
				heldMove(t, c.path("recovery", heldTarget), c.path("moved"))
				checkpointWrite(t, c, "recovery/"+heldTarget, "placed at the name")
			})
		})
		if !failure.CleanupIncomplete || string(retainedRead(t, c, "recovery/"+heldTarget)) != "placed at the name" {
			t.Fatal("a failed checkpoint removed the file put at its name, or reported the discard")
		}
	})
	// The cleanup removes once: the live database fixture renamed onto the name at
	// its sweep or after its removal and sync, or linked there then, stays; incomplete.
	// The same when the copy or the witness step fails.
	for _, row := range []string{"sweep", "synced", "linked", "sweep_copy", "synced_copy", "sweep_witness", "synced_witness"} {
		t.Run("discard_swap_"+row, func(t *testing.T) {
			at, step, _ := strings.Cut(row, "_")
			original, kept := syncCheckpointRoot, heldTrees{}
			t.Cleanup(func() { syncCheckpointRoot = original })
			_, failure := heldDiscard(t, func(c *Cell) {
				name := c.path("recovery", heldTarget)
				if kept[name] = heldTree(t, c.path("pg")); at == "sweep" {
					heldAt(c, at, func() { heldMove(t, name, c.path("aside")); heldMove(t, c.path("pg"), name) })
					return
				}
				syncCheckpointRoot = func(held *os.Root) error {
					if filepath.Clean(held.Name()) != c.recoveryDir() {
						return original(held)
					}
					err := original(held)
					if syncCheckpointRoot = original; err == nil && at == "synced" {
						heldMove(t, c.path("pg"), name)
					} else if err == nil {
						kept = heldTrees{c.path("pg"): kept[name]}
						drillRootLink(t, name, c.path("pg"))
					}
					return err
				}
			}, step)
			if heldSame(t, kept); !failure.CleanupIncomplete {
				t.Fatal("a failed checkpoint reported the discard of a directory renamed onto its name")
			}
		})
	}
	// recovery/ linked to the cell root, where the name is absent.
	t.Run("discard_recovery_root", func(t *testing.T) {
		kept := heldTrees{}
		_, failure := heldDiscard(t, func(c *Cell) {
			heldAt(c, "hold", func() {
				heldMove(t, c.recoveryDir(), c.path("moved"))
				kept[c.path("moved")] = heldTree(t, c.path("moved"))
				drillRootLink(t, c.recoveryDir(), c.cfg.Root)
			})
		})
		if heldSame(t, kept); !failure.CleanupIncomplete {
			t.Fatal("a failed checkpoint reported its discard through a recovery link to the cell root")
		}
	})
	// recovery/ moved away at the cleanup's hold, left absent or a private file put
	// at its name: not held, so incomplete, the moved tree kept.
	for _, put := range []string{"absent", "file"} {
		t.Run("discard_recovery_"+put, func(t *testing.T) {
			kept := heldTrees{}
			_, failure := heldDiscard(t, func(c *Cell) {
				heldAt(c, "hold", func() {
					heldMove(t, c.recoveryDir(), c.path("moved"))
					if kept[c.path("moved")] = heldTree(t, c.path("moved")); put == "file" {
						checkpointWrite(t, c, "recovery", "placed at the name")
					}
				})
			})
			if heldSame(t, kept); !failure.CleanupIncomplete {
				t.Fatal("a failed checkpoint reported its discard without holding its recovery directory")
			}
		})
	}
	heldIdentifierRow(t)
	heldPlatformRows(t)
}

func TestCheckpointRootSync(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil || root.Close() != nil {
		t.Fatal("cannot prepare a closed root")
	}
	if err := syncRootDirectory(root); (err == nil) != (runtime.GOOS == "windows") {
		t.Fatal("the held directory sync hid the failure of a closed root")
	}
}
