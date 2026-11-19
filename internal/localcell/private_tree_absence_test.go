package localcell

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// LC-TREEWALK-F1. A walk that met a vanished object (an error wrapping fs.ErrNotExist) is an incomplete
// pass: each walker's one crossing walks again through the seam, on the fixture's clock and budget, until
// a walk passes or the budget ends in the layout refusal; a walk refused for another reason is not repeated.
func TestPrivateTreeWalkersWalkAgain(t *testing.T) {
	transient, refusal := errors.New("synthetic transient tree refusal"), errors.New("synthetic tree validation refusal")
	vanished := fmt.Errorf("synthetic vanished tree object: %w", fs.ErrNotExist)
	for _, walker := range treeWalkers {
		for _, row := range []struct {
			name    string
			results []error // each walk's result in turn, the last one repeated
			walks   int     // 0: until the budget ends
			want    error   // what the boundary returns to the walker
		}{
			{"vanished_twice", []error{vanished, vanished, nil}, 3, nil},
			{"vanished_forever", []error{vanished}, 0, errPrivateLayout},
			{"refused_vanished_passed", []error{transient, vanished, nil}, 3, nil},
			{"other_refusal", []error{refusal, nil}, 1, refusal},
		} {
			t.Run(walker.name+"/"+row.name, func(t *testing.T) {
				root, run := walker.setup(t)
				calls, walks, now, sleeps, previous := 0, 0, time.Unix(0, 0), []time.Duration{}, validatePrivateTreeRetried
				t.Cleanup(func() { validatePrivateTreeRetried = previous })
				validatePrivateTreeRetried = func(walked string) error {
					calls++
					return validateRootTree(walked, func(walked string) error {
						if walks++; walks > 64 || walked != root { // a runaway retry, or another root
							return errors.New("synthetic tree walker fuse")
						}
						return row.results[min(walks, len(row.results))-1]
					}, func(err error) bool { return errors.Is(err, transient) || runtimeconfig.WindowsPrivateTransient(err) },
						leasePublishHooks{now: func() time.Time { return now }, sleep: func(delay time.Duration) { sleeps, now = append(sleeps, delay), now.Add(delay) }})
				}
				err := run()
				slept := now.Sub(time.Unix(0, 0)) // read after the walker ran: the injected sleeps advance now
				if calls != 1 {
					t.Errorf("a tree walker crossed its boundary other than once: %d", calls)
				}
				if len(sleeps) != walks-1 {
					t.Error("a tree walker did not sleep once between two walks")
				}
				if row.walks != 0 && walks != row.walks {
					t.Errorf("a tree walker walked another number of times: %d", walks)
				}
				if row.walks == 0 && slept < serviceLeasePublishBudget {
					t.Error("a vanished tree object was refused before the budget ended")
				}
				if row.walks == 0 && len(sleeps) != 0 && slept-sleeps[len(sleeps)-1] >= serviceLeasePublishBudget {
					t.Error("a vanished tree object was walked again after the budget ended")
				}
				if errors.Is(err, fs.ErrNotExist) {
					t.Error("a vanished tree object reached the tree walker caller as an absence")
				}
				if walker.outcome == nil && err != row.want || walker.outcome != nil && !walker.outcome(err, row.want) {
					t.Error("a tree walker changed what the boundary result maps to")
				}
			})
		}
	}
}

// LC-TREEWALK-R1 PB-6 (C2), every system: through the boundary's own composition on the fixture's clock,
// a walk that met a vanished directory is a layout refusal at every descriptor reader, never an absence.
func TestPrivateTreeWalkAbsenceIsNotDescriptorAbsence(t *testing.T) {
	c, d := descriptorTestCell(t) // WITSELF_RUNTIME=local, a prepared foreground descriptor
	if c.SetServiceDesiredState(t.Context(), "stopped") != nil || !errors.Is(c.serviceStartFence(), ErrServiceDesiredState) {
		t.Fatal("cannot fence the descriptor fixture with a stopped state")
	}
	before, err := os.ReadFile(c.path("service", "descriptor.json"))
	if err != nil {
		t.Fatal("cannot read the descriptor fixture")
	}
	crossings, from, now, previous := 0, 1, time.Unix(0, 0), validatePrivateTreeRetried
	t.Cleanup(func() { validatePrivateTreeRetried = previous })
	validatePrivateTreeRetried = func(root string) error {
		crossings++
		vanishing, walks := crossings >= from, 0
		return validateRootTree(root, func(walked string) error {
			if walks++; walks > 64 || walked != c.cfg.Root { // a runaway retry, or another root
				return errors.New("synthetic tree walker fuse")
			}
			if !vanishing {
				return nil
			}
			return fmt.Errorf("synthetic vanished directory: %w", fs.ErrNotExist)
		}, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{now: func() time.Time { return now }, sleep: func(delay time.Duration) { now = now.Add(delay) }})
	}
	if _, err := c.ServiceDescriptor(); !errors.Is(err, errPrivateLayout) || errors.Is(err, fs.ErrNotExist) {
		t.Error("the descriptor read took a vanished tree object for an absent descriptor")
	}
	if c.serviceStartFence() == nil {
		t.Error("the start fence opened on a vanished tree object")
	}
	if c.reinstallServiceFence() == nil {
		t.Error("the reinstall fence opened on a vanished tree object")
	}
	if c.bindServiceProcessBootWithReader("server", func() (string, error) { return "synthetic-boot", nil }) == nil {
		t.Error("the boot binding skipped its descriptor on a vanished tree object")
	}
	if c.exitServiceHostForRemoval(t.Context()) == nil {
		t.Error("the removal skipped its host exit on a vanished tree object")
	}
	if err := c.setServiceDesiredStateLocked("stopped"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Error("the desired-state write took a vanished tree object for an absent descriptor")
	}
	// The service preparation's descriptor read is its fourth crossing, after the three of own(false).
	crossings, from = 0, 1<<20
	lock, err := c.own(false)
	if err == nil {
		err = lock.Close()
	}
	if err != nil || crossings != 3 {
		t.Fatal("cannot count the three ownership crossings before the preparation reads its descriptor")
	}
	crossings, from = 0, 4
	_ = c.PrepareService(t.Context(), d)
	if after, err := os.ReadFile(c.path("service", "descriptor.json")); err != nil || !bytes.Equal(before, after) {
		t.Error("the service preparation overwrote a descriptor behind a vanished tree object")
	}
}
