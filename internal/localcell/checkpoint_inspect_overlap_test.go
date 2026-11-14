package localcell

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func checkpointInspectCase(t *testing.T) *checkpointCase {
	t.Helper()
	f, r := checkpointFixture(t, "legacy", false)
	options := checkpointOptions(f)
	return &checkpointCase{t, f, f.cell, r, &options}
}

func checkpointInspectState(t *testing.T, c *Cell, want string) Inspection {
	t.Helper()
	inspection, err := c.Inspect(t.Context())
	if err != nil || inspection.CheckpointListing != want || inspection.Locked != (want == "busy") ||
		(want != "listed" && inspection.Checkpoints != nil) {
		t.Fatal("inspection did not return the expected ownership and listing state")
	}
	data, err := json.Marshal(inspection)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &fields) != nil || string(fields["checkpoint_listing"]) != `"`+want+`"` {
		t.Fatal("inspection JSON omitted or changed the listing state")
	}
	return inspection
}

// Inventory every path, type/mode, size and regular-file byte without logging
// any of them. Empty lock files have no bytes to read (Windows locks them).
func checkpointInspectInventory(t *testing.T, c *Cell) map[string]retainedEntry {
	t.Helper()
	entries := map[string]retainedEntry{}
	err := filepath.WalkDir(c.cfg.Root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := retainedEntry{mode: info.Mode(), size: info.Size()}
		if info.Mode().IsRegular() && info.Size() != 0 {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			value.data = string(data)
		}
		entries[name] = value
		return nil
	})
	if err != nil {
		t.Fatal("cannot inventory the inspection fixture")
	}
	return entries
}

func checkpointInspectBarrier(t *testing.T) (func(), func(), func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	pause := func() { enterOnce.Do(func() { close(entered); <-release }) }
	wait := func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(reinstallContentionBound):
			t.Fatal("inspection overlap did not reach its barrier")
		}
	}
	return pause, wait, unblock
}

func TestCheckpointInspectOverlap(t *testing.T) {
	for _, operation := range []string{"copy", "delete"} {
		t.Run(operation, func(t *testing.T) {
			k := checkpointInspectCase(t)
			observer, err := New(k.c.cfg)
			if err != nil {
				t.Fatal("cannot create the inspection observer")
			}
			pause, wait, unblock := checkpointInspectBarrier(t)
			run := func() error { return k.c.Checkpoint(t.Context(), *k.options) }
			var dir string
			if operation == "copy" {
				k.c.checkpointTest.beforeStep = func(step, name string) {
					if step == "publish" {
						// The first file's parent is inside the new checkpoint.
						dir = filepath.Dir(name)
						pause()
					}
				}
			} else {
				if run() != nil {
					t.Fatal("cannot create the deletion fixture")
				}
				checkpointID := checkpointIDs(t, k.c)[0]
				dir, err = k.c.checkpointDir(checkpointID)
				if err != nil {
					t.Fatal("cannot locate the deletion fixture")
				}
				k.c.cfg.Failpoint = func(operation, step string) error {
					if operation == "checkpoint_delete" && step == "unseal" {
						pause() // The witness has actually been removed.
					}
					return nil
				}
				run = func() error {
					return k.c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: checkpointID})
				}
			}
			done := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, run)
			wait()
			checkpointInspectPoison(t, filepath.Join(dir, "refused"))
			if k.c.validateRecovery() == nil {
				t.Fatal("planted entry did not refuse recovery validation")
			}
			checkpointInspectState(t, observer, "busy")
			unblock()
			err = <-done
			if (operation == "copy" && !checkpointSameError(err, "local_checkpoint_witness_mismatch")) || (operation == "delete" && err != nil) {
				t.Fatal("operation did not finish as the planted entry dictates")
			}
			if got := checkpointInspectState(t, observer, "listed"); len(got.Checkpoints) != 0 {
				t.Fatal("operation did not remove the invalid checkpoint")
			}
		})
	}
}

func TestCheckpointInspectOwnership(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		k := checkpointInspectCase(t)
		if k.c.Checkpoint(t.Context(), *k.options) != nil {
			t.Fatal("cannot create the owned inspection fixture")
		}
		checkpointID := checkpointIDs(t, k.c)[0]
		contender, err := New(k.c.cfg)
		if err != nil {
			t.Fatal("cannot create the inspection contender")
		}
		k.r.install(contender)
		pause, wait, unblock := checkpointInspectBarrier(t)
		k.c.inspectCheckpointListing = func(state string) {
			if state == "owned" {
				pause()
			}
		}
		var inspection Inspection
		done := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, func() error {
			var err error
			inspection, err = k.c.Inspect(t.Context())
			return err
		})
		wait()
		before := checkpointInspectInventory(t, k.c)
		operations := []func() error{
			func() error { return contender.Checkpoint(t.Context(), *k.options) },
			func() error {
				return contender.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: checkpointID})
			},
		}
		for _, operation := range operations {
			if !errors.Is(operation(), ErrLocked) {
				t.Fatal("Inspect did not retain ownership through its recovery read")
			}
			if !reflect.DeepEqual(before, checkpointInspectInventory(t, k.c)) {
				t.Fatal("operation contending with Inspect changed the root inventory")
			}
		}
		unblock()
		if <-done != nil || inspection.CheckpointListing != "listed" || len(inspection.Checkpoints) != 1 || !inspection.Checkpoints[0].Complete {
			t.Fatal("owned inspection did not list the complete checkpoint")
		}
		for _, operation := range operations {
			if operation() != nil {
				t.Fatal("operation did not succeed after Inspect released ownership")
			}
		}
	})
	t.Run("missing-lock", func(t *testing.T) {
		k := checkpointInspectCase(t)
		if k.c.Checkpoint(t.Context(), *k.options) != nil || os.Remove(k.c.path(".owner.lock")) != nil {
			t.Fatal("cannot prepare a complete checkpoint without its owner lock")
		}
		checkpointInspectPoison(t, filepath.Join(k.c.recoveryDir(), "refused"))
		if k.c.validateRecovery() == nil {
			t.Fatal("missing-lock fixture did not refuse validation")
		}
		before := checkpointInspectInventory(t, k.c)
		checkpointInspectState(t, k.c, "unowned")
		if !reflect.DeepEqual(before, checkpointInspectInventory(t, k.c)) {
			t.Fatal("unowned inspection changed the root or created a lock")
		}
	})
	for _, row := range []string{"empty-root", "absent-then-created"} {
		t.Run(row, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			c, err := New(Config{Root: testPrivateRoot(t)})
			if err != nil {
				t.Fatal("cannot create the empty inspection fixture")
			}
			before := checkpointInspectInventory(t, c)
			created := false
			if row == "absent-then-created" {
				c.inspectCheckpointListing = func(state string) {
					if state != "absent" {
						return
					}
					if privateDir(c.recoveryDir(), true) != nil {
						t.Fatal("absence observation did not precede boundary creation")
					}
					checkpointInspectPoison(t, filepath.Join(c.recoveryDir(), "refused"))
					created = true
				}
			}
			if got := checkpointInspectState(t, c, "listed"); len(got.Checkpoints) != 0 {
				t.Fatal("absent boundary did not produce an empty listing")
			}
			if row == "empty-root" && !reflect.DeepEqual(before, checkpointInspectInventory(t, c)) {
				t.Fatal("empty-root inspection created state")
			} else if row == "absent-then-created" && (!created || c.validateRecovery() == nil) {
				t.Fatal("absence race did not create a validator-refused tree")
			}
		})
	}
}
