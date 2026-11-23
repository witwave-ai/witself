//go:build windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// The launched child's descriptor read joins the walkers; its refusal is today's fixed one.
func init() {
	treeWalkers = append(treeWalkers, treeWalker{"shutdown_descriptor", func(t *testing.T) (string, func() error) {
		c, _ := descriptorTestCell(t)
		d, path, err := c.prepareShutdownLaunch("server")
		d.PID, d.StartIdentity = os.Getpid(), "synthetic-start-identity"
		if err != nil || writeAtomic(path, d, nil) != nil {
			t.Fatal("cannot publish the shutdown descriptor fixture")
		}
		return c.cfg.Root, func() error {
			got, err := readShutdownDescriptor(c.cfg.Root, path)
			if err == nil && got != d {
				t.Fatal("the shutdown descriptor read returned another descriptor")
			}
			return err
		}
	}, func(err, last error) bool {
		return last == nil && err == nil || last != nil && errors.Is(err, errLocalShutdownRefused) && err.Error() == "service_shutdown_refused: shutdown:descriptor"
	}})
}

// A real directory delete handle below the tree, proved on the bare walk: released within the budget, the walker
// passes after it; kept past it (a fuse at three budgets), the walker ends after the budget in today's
// (unretried) refusal. One check per expectation (LC-TREEWALK-F1).
func TestWindowsPrivateTreeWalkersHolder(t *testing.T) {
	for _, walker := range treeWalkers {
		for _, hold := range []time.Duration{serviceLeasePublishBudget / 8, 3 * serviceLeasePublishBudget} {
			held := hold > serviceLeasePublishBudget
			t.Run(walker.name+map[bool]string{false: "/released", true: "/held"}[held], func(t *testing.T) {
				root, run := walker.setup(t)
				release, today, previous := holdTreeDelete(t, filepath.Join(root, "run")), error(nil), validatePrivateTreeRetried
				t.Cleanup(func() { validatePrivateTreeRetried = previous })
				if validatePrivateTree(root) == nil {
					t.Fatalf("cannot refuse the bare tree walk with the held delete handle: %s", walker.name)
				}
				if held {
					validatePrivateTreeRetried = validatePrivateTree
					today, validatePrivateTreeRetried = run(), previous
				}
				released := make(chan struct{})
				started, timer := time.Now(), time.AfterFunc(hold, func() {
					defer close(released)
					release()
				})
				err := run()
				elapsed := time.Since(started)
				if !timer.Stop() {
					<-released
				}
				release()
				switch {
				case !held && (walker.outcome == nil && err != nil || walker.outcome != nil && !walker.outcome(err, nil)):
					t.Fatalf("the tree walker did not survive a delete handle released within its budget: %s", walker.name)
				case !held && elapsed < hold:
					t.Fatalf("the tree walker passed a tree walk while the delete handle was held: %s", walker.name)
				case held && today == nil:
					t.Fatalf("the unretried tree walker passed a walk the held delete handle refused: %s", walker.name)
				case held && err == nil:
					t.Fatalf("a delete handle held past the budget let the tree walker pass: %s", walker.name)
				case held && runtimeconfig.WindowsPrivateFailureClass(err) != runtimeconfig.WindowsPrivateFailureClass(today):
					t.Fatalf("a delete handle held past the budget changed the tree walker refusal class: %s", walker.name)
				case held && err.Error() != today.Error():
					t.Fatalf("a delete handle held past the budget changed the tree walker refusal text: %s", walker.name)
				case held && elapsed < serviceLeasePublishBudget:
					t.Fatalf("the tree walker refused a held delete handle before its budget: %s", walker.name)
				}
			})
		}
	}
}
