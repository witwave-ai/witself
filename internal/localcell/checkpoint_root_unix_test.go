//go:build linux || darwin

package localcell

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// heldWatchdog runs the deletion; one still running after ten seconds has
// fifo opened read-write, is awaited at most ten more and fails the row.
func heldWatchdog(t *testing.T, c *Cell, fifo string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- heldDelete(t, c) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
	}
	if unblock, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
		defer func() { _ = unblock.Close() }()
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	t.Fatal("a held deletion waited on a planted fifo")
	return nil
}

func init() {
	heldPlatformRows = func(t *testing.T) {
		// A FIFO at recovery/ or at the name is refused at once, never opened.
		for _, row := range []struct {
			name  string
			moved []string
		}{{"fifo_recovery", []string{"recovery"}}, {"fifo_name", []string{"recovery", heldTarget}}} {
			t.Run(row.name, func(t *testing.T) {
				f, c := heldFixture(t)
				fifo, prepared := c.path(row.moved...), errors.New("stage not reached")
				heldAt(c, "hold", func() { prepared = errors.Join(os.Rename(fifo, c.path("moved")), syscall.Mkfifo(fifo, 0o600)) })
				err := heldWatchdog(t, c, fifo)
				if prepared != nil {
					t.Fatal("cannot plant a held deletion fifo")
				}
				heldRefused(t, f, err, "local_checkpoint_state_unsupported", nil)
			})
		}
		t.Run("fifo_inside", func(t *testing.T) {
			_, c := heldFixture(t)
			fifo, prepared := c.path("recovery", heldTarget, "pg", "fifo"), errors.New("stage not reached")
			heldAt(c, "sweep", func() { prepared = syscall.Mkfifo(fifo, 0o600) })
			if err := heldWatchdog(t, c, fifo); err != nil || prepared != nil || len(checkpointIDs(t, c)) != 1 {
				t.Fatal("a held deletion did not remove a fifo inside its checkpoint")
			}
		})
		// The name replaced by a link to the held directory, moved aside.
		t.Run("name_linked", func(t *testing.T) {
			f, c := heldFixture(t)
			target, aside := c.path("recovery", heldTarget), c.path("recovery", "aside")
			kept := heldTrees{aside: heldTree(t, target)}
			delete(kept[aside], checkpointWitnessName)
			heldUnsealed(c, func() { heldMove(t, target, aside); drillRootLink(t, target, "aside") })
			heldFailed(t, f, heldDelete(t, c), "remove")
			heldSame(t, kept)
		})
		// recovery/ moved and linked to an outside directory while held.
		t.Run("recovery_linked_held", func(t *testing.T) {
			f, c := heldFixture(t)
			outside, moved := heldOutside(t), c.path("moved")
			kept := heldTrees{moved: heldTree(t, c.recoveryDir()), outside: heldTree(t, outside)}
			delete(kept[moved], heldTarget+"/"+checkpointWitnessName)
			heldUnsealed(c, func() { heldMove(t, c.recoveryDir(), moved); drillRootLink(t, c.recoveryDir(), outside) })
			heldFailed(t, f, heldDelete(t, c), "remove")
			heldSame(t, kept)
		})
	}
}
