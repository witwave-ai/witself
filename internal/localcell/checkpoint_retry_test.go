package localcell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var errCheckpointRetryRefusal = errors.New("synthetic transient refusal")

type checkpointRetryClock struct {
	slept time.Duration
	waits []time.Duration
}

func checkpointRetrySetup(c *Cell) *checkpointRetryClock {
	clock := &checkpointRetryClock{}
	c.checkpointTest.now = func() time.Time { return time.Unix(0, 0).Add(clock.slept) }
	c.checkpointTest.sleep = func(_ context.Context, d time.Duration) {
		clock.slept += d
		clock.waits = append(clock.waits, d)
	}
	c.checkpointTest.transient = func(err error) bool { return errors.Is(err, errCheckpointRetryRefusal) }
	return clock
}

func checkpointRetryInject(c *Cell, step string, refusal func(string) error) {
	if step == "publish" {
		c.checkpointTest.publish = func(old, target string) error {
			if err := refusal(target); err != nil {
				return err
			}
			return publishPrivateFile(old, target)
		}
	} else {
		c.checkpointTest.open = func(name string) (*os.File, error) {
			if err := refusal(name); err != nil {
				return nil, err
			}
			return openPrivateFile(name, os.O_RDONLY, 0)
		}
	}
}

func checkpointRetryDiscarded(t *testing.T, c *Cell) {
	t.Helper()
	if len(checkpointIDs(t, c)) != 0 {
		t.Fatal("failed retry left a checkpoint directory")
	}
	if filepath.WalkDir(c.cfg.Root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".pending-") {
			return errors.New("pending temporary remains")
		}
		return err
	}) != nil {
		t.Fatal("failed retry left a temporary or unreadable tree")
	}
}

// T1–T6: exact engine backoff, inventory identity, final codes and cleanup.
func TestCheckpointRetrySteps(t *testing.T) {
	for _, step := range []string{"publish", "open"} {
		for _, k := range []int{1, 4, -1, -2} {
			t.Run(fmt.Sprintf("%s/%d", step, k), func(t *testing.T) {
				f, r := checkpointFixture(t, "legacy", false)
				c, id := f.cell, "ckpt_aaaaaaaaaaaaaaaa"
				r.nextID = func() (string, error) { return id, nil }
				clock := checkpointRetrySetup(c)
				inventory := func() []byte {
					b, err := os.ReadFile(filepath.Join(c.recoveryDir(), id, checkpointInventoryName))
					if err != nil {
						t.Fatal("cannot read retry inventory")
					}
					return b
				}
				var baseline []byte
				if k > 0 {
					if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil {
						t.Fatal("fault-free checkpoint failed")
					}
					baseline = inventory()
					if c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: id}) != nil {
						t.Fatal("cannot reset fault-free checkpoint")
					}
				}
				chosen, attempts := "", 0
				checkpointRetryInject(c, step, func(name string) error {
					if chosen == "" {
						chosen = name
					}
					if name != chosen {
						return nil
					}
					attempts++
					if k == -2 {
						return fs.ErrInvalid
					}
					if k < 0 || attempts <= k {
						return errCheckpointRetryRefusal
					}
					return nil
				})
				err := c.Checkpoint(t.Context(), checkpointOptions(f))
				if k > 0 {
					sequence := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond}
					if err != nil || attempts != k+1 || !slices.Equal(clock.waits, sequence[:k]) {
						t.Fatal("retry success did not preserve attempts and exact backoff")
					}
					if !bytes.Equal(baseline, inventory()) {
						t.Fatal("retry changed inventory bytes")
					}
					return
				}
				want := "local_checkpoint_copy_failed"
				if step == "open" {
					want = "local_checkpoint_witness_mismatch"
				}
				if !checkpointSameError(err, want) {
					t.Fatal("retry exhaustion changed stable error")
				}
				if k == -2 && (attempts != 1 || len(clock.waits) != 0) {
					t.Fatal("non-transient error retried")
				}
				// The existing engine checks 2 s after each attempt: its sequence reaches 2.175 s.
				if k == -1 && (clock.slept != 2175*time.Millisecond || attempts != 14) {
					t.Fatal("step budget or backoff changed")
				}
				checkpointRetryDiscarded(t, c)
			})
		}
	}
}

// T7: both individual phases and their shared budget, including just below it.
func TestCheckpointRetryCumulative(t *testing.T) {
	for _, step := range []string{"publish", "open", "shared"} {
		for _, boundary := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", step, boundary), func(t *testing.T) {
				f, _ := checkpointFixture(t, "legacy", false)
				c := f.cell
				for i := range 20 {
					checkpointWrite(t, c, fmt.Sprintf("pg/base/1/retry%d", i), "synthetic retry file")
				}
				plan, err := c.admitCheckpoint(t.Context(), checkpointOptions(f), c.checkpointOps())
				if err != nil {
					t.Fatal("cannot plan retry fixture")
				}
				limit := checkpointRetryFloor
				for _, entry := range plan.entries {
					if !entry.directory {
						limit += checkpointRetryPerFile
					}
				}
				clock, afterBound, stop := checkpointRetrySetup(c), 0, false
				for _, phase := range []string{"publish", "open"} {
					if step != "shared" && step != phase {
						continue
					}
					counts := map[string]int{}
					checkpointRetryInject(c, phase, func(name string) error {
						counts[name]++
						k := 10
						if step == "shared" {
							k = 4
						}
						if counts[name] > k || stop {
							return nil
						}
						next := min(25*time.Millisecond<<(counts[name]-1), 200*time.Millisecond)
						if boundary && clock.slept+next >= limit {
							stop = true
							return nil
						}
						if clock.slept >= limit {
							afterBound++
							if afterBound != 1 {
								t.Fatal("first refusal after bound was not a single attempt")
							}
						}
						return errCheckpointRetryRefusal
					})
				}
				err = c.Checkpoint(t.Context(), checkpointOptions(f))
				if boundary {
					if err != nil || clock.slept >= limit || limit-clock.slept > 200*time.Millisecond {
						t.Fatal("just-under-bound retries did not succeed")
					}
				} else {
					want := "local_checkpoint_copy_failed"
					if step != "publish" {
						want = "local_checkpoint_witness_mismatch"
					}
					if !checkpointSameError(err, want) || clock.slept != limit || afterBound != 1 {
						t.Fatal("cumulative bound did not stop the next refusal")
					}
					checkpointRetryDiscarded(t, c)
				}
			})
		}
	}
}

// T8 and predicate cancellation: no operation after cancellation, no sleep
// when cancellation precedes the predicate, and context reaches the sleep hook.
func TestCheckpointRetryCancellation(t *testing.T) {
	for _, step := range []string{"publish", "open"} {
		for _, duringSleep := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", step, duringSleep), func(t *testing.T) {
				f, _ := checkpointFixture(t, "legacy", false)
				c, attempts, sleeps := f.cell, 0, 0
				checkpointRetrySetup(c)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				checkpointRetryInject(c, step, func(string) error {
					attempts++
					if !duringSleep {
						cancel()
					}
					return errCheckpointRetryRefusal
				})
				c.checkpointTest.sleep = func(got context.Context, _ time.Duration) {
					if got != ctx {
						t.Fatal("backoff lost caller context")
					}
					sleeps++
					cancel()
				}
				err := c.Checkpoint(ctx, checkpointOptions(f))
				wantSleeps := 0
				if duringSleep {
					wantSleeps = 1
				}
				if !checkpointSameError(err, "local_checkpoint_copy_failed") || attempts != 1 || sleeps != wantSleeps {
					t.Fatal("cancellation allowed another attempt or sleep")
				}
				checkpointRetryDiscarded(t, c)
			})
		}
	}
}

func TestCheckpointRetrySleepCancellation(t *testing.T) {
	ops := (&Cell{}).checkpointOps()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() { ops.sleep(ctx, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("production backoff ignored context cancellation")
	}
}

func TestCheckpointRetryPostMove(t *testing.T) {
	for _, target := range []string{"regular", "absent", "directory"} {
		t.Run(target, func(t *testing.T) {
			f, _ := checkpointFixture(t, "legacy", false)
			c, attempts := f.cell, 0
			checkpointRetrySetup(c)
			c.checkpointTest.publish = func(old, name string) error {
				attempts++
				if err := publishPrivateFile(old, name); err != nil {
					return err
				}
				if attempts != 1 {
					return nil
				}
				if target != "regular" {
					if os.Remove(name) != nil {
						t.Fatal("cannot prepare post-move refusal")
					}
					if target == "directory" && privateDir(name, true) != nil {
						t.Fatal("cannot prepare nonregular target")
					}
				}
				return errCheckpointRetryRefusal
			}
			err := c.Checkpoint(t.Context(), checkpointOptions(f))
			if target == "regular" {
				if err != nil {
					t.Fatal("complete post-move publication retried rename")
				}
			} else {
				if !checkpointSameError(err, "local_checkpoint_copy_failed") {
					t.Fatal("post-move completion accepted a missing or nonregular target")
				}
				checkpointRetryDiscarded(t, c)
			}
		})
	}
}
