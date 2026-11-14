//go:build windows

package localcell

import (
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestWindowsCheckpointInspectRecoveryAlias(t *testing.T) {
	c, _, options := windowsCheckpointFixture(t, testPrivateRoot(t))
	if c.Checkpoint(t.Context(), options) != nil {
		t.Fatal("cannot prepare the alias checkpoint")
	}
	alias := c.path("Recovery")
	if os.Rename(c.recoveryDir(), alias) != nil || os.Remove(c.path(".owner.lock")) != nil {
		t.Fatal("cannot prepare the case-only alias without its owner lock")
	}
	before := checkpointInspectInventory(t, c)
	for _, operation := range []func() error{
		func() error { _, err := c.Inspect(t.Context()); return err },
		func() error { return c.Checkpoint(t.Context(), options) },
	} {
		if !checkpointSameError(operation(), "cannot inspect private local cell layout") {
			t.Fatal("case-only recovery alias did not return the fixed refusal")
		}
		if !reflect.DeepEqual(before, checkpointInspectInventory(t, c)) {
			t.Fatal("alias refusal changed the root inventory")
		}
		if _, err := os.Lstat(c.path(".owner.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("alias refusal created an owner lock")
		}
	}
	if os.Rename(alias, c.recoveryDir()) != nil {
		t.Fatal("cannot restore the exact recovery spelling")
	}
	// Restore the removed lock so Inspect can acquire ownership for listing.
	lock, err := c.own(false)
	if err != nil || lock.Close() != nil {
		t.Fatal("cannot restore the checkpoint fixture owner lock")
	}
	inspection := checkpointInspectState(t, c, "listed")
	if len(inspection.Checkpoints) != 1 || !inspection.Checkpoints[0].Complete {
		t.Fatal("restored recovery spelling did not list the complete checkpoint")
	}
}

func checkpointInspectPoison(t *testing.T, name string) {
	t.Helper()
	if testWriteFile(name, nil, os.FileMode(0o600)) != nil {
		t.Fatal("cannot plant a validator-refused entry")
	}
	setTestDACL(t, name, "(A;;FR;;;WD)", false, false)
}

// These rows pause real Windows operations at existing publication and unseal
// boundaries; neither the lock nor the filesystem operation is replaced.
func TestWindowsCheckpointInspectOverlap(t *testing.T) {
	for _, operation := range []string{"publish", "delete"} {
		t.Run(operation, func(t *testing.T) {
			c, _, options := windowsCheckpointFixture(t, testPrivateRoot(t))
			observer, err := New(c.cfg)
			if err != nil {
				t.Fatal("cannot prepare the inspection observer")
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var pauseOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			pause := func() { pauseOnce.Do(func() { close(entered); <-release }) }
			run := func() error { return c.Checkpoint(t.Context(), options) }
			if operation == "publish" {
				c.checkpointTest.beforeStep = func(step, _ string) {
					if step == "publish" {
						pause()
					}
				}
			} else {
				if c.Checkpoint(t.Context(), options) != nil {
					t.Fatal("cannot prepare the deletion checkpoint")
				}
				ids := checkpointIDs(t, c)
				if len(ids) != 1 {
					t.Fatal("deletion fixture did not create one checkpoint")
				}
				c.cfg.Failpoint = func(operation, step string) error {
					if operation == "checkpoint_delete" && step == "unseal" {
						pause()
					}
					return nil
				}
				run = func() error {
					return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: ids[0]})
				}
			}
			result := StartJoinedTestRunForTest(t, unblock, reinstallContentionBound, run)
			select {
			case <-entered:
			case <-result:
				t.Fatal("operation returned before its overlap boundary")
			case <-time.After(reinstallContentionBound):
				t.Fatal("operation did not reach its overlap boundary")
			}
			inspection, err := observer.Inspect(t.Context())
			if err != nil || !inspection.Locked || inspection.CheckpointListing != "busy" || inspection.Checkpoints != nil {
				t.Fatal("inspection did not report the owned Windows operation as busy")
			}
			unblock()
			if <-result != nil {
				t.Fatal("overlapping Windows operation did not complete")
			}
			inspection, err = observer.Inspect(t.Context())
			if err != nil || inspection.Locked || inspection.CheckpointListing != "listed" {
				t.Fatal("inspection did not list after the Windows operation released ownership")
			}
			if operation == "publish" {
				if len(inspection.Checkpoints) != 1 || !inspection.Checkpoints[0].Complete {
					t.Fatal("publication did not leave one complete checkpoint")
				}
			} else if len(inspection.Checkpoints) != 0 {
				t.Fatal("deletion left a checkpoint listed")
			}
		})
	}
}
