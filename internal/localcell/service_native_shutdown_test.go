package localcell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// serviceIdleShutdownContext cancels when Run first waits in its idle select,
// after tick has reset the phase and operation labels. No timer or goroutine
// races cancellation against the startup hooks.
type serviceIdleShutdownContext struct {
	context.Context
	cancel context.CancelFunc
}

func (c *serviceIdleShutdownContext) Done() <-chan struct{} {
	c.cancel()
	return c.Context.Done()
}

func TestServiceRunIdleShutdownReceipt(t *testing.T) {
	c, m := unitCell(t)
	if runtime.GOOS != "windows" {
		// Keep the control socket within the native Unix path limit.
		root, err := os.MkdirTemp("/tmp", "lc-idle-")
		if err != nil {
			t.Fatal("cannot create short private fixture")
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		c, err = New(Config{Root: root, Timeout: time.Second})
		if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
			t.Fatal("cannot prepare short private fixture")
		}
	}
	prepareSupervisorTest(t, c)
	if err := c.SetServiceDesiredState(t.Context(), "stopped"); err != nil {
		t.Fatal("cannot prepare idle foreground fixture")
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &serviceIdleShutdownContext{Context: parent, cancel: cancel}
	h, _, starts, stops, _ := supervisorTestHooks()
	if runtime.GOOS != "windows" {
		h.boot = func() (string, error) { return "synthetic-boot", nil }
	} // Windows endpoint publication must match the lease's native boot.
	h.ticks = make(chan time.Time)
	factories, repairs, canceled := 0, 0, false
	var shutdownCtx context.Context
	h.shutdownContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		factories++
		if factories != 1 || timeout != time.Second || parent.Err() != nil || parent.Done() != nil || ctx.Err() == nil {
			t.Fatal("idle shutdown did not create one fresh signal-independent budget")
		}
		var stopCancel context.CancelFunc
		shutdownCtx, stopCancel = context.WithTimeout(parent, timeout)
		return shutdownCtx, func() { canceled = true; stopCancel() }
	}
	h.repair = func(opCtx context.Context) error {
		repairs++
		if shutdownCtx != nil && (opCtx != shutdownCtx || opCtx.Err() != nil) {
			t.Fatal("idle shutdown repair did not receive the fresh budget")
		}
		return nil
	}
	stderr, err := os.CreateTemp(t.TempDir(), "idle-shutdown-stderr")
	if err != nil {
		t.Fatal("cannot capture idle shutdown receipt")
	}
	previous := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
	err = c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: time.Second})
	os.Stderr = previous
	if err != nil || factories != 1 || repairs != 2 || *starts != 0 || *stops != 2 || !canceled {
		t.Fatal("idle shutdown did not complete one clean startup and shutdown")
	}
	data, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal("cannot read idle shutdown receipt")
	}
	if len(data) == 0 {
		t.Fatal("idle shutdown receipt is missing")
	}
	if bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatal("idle shutdown emitted an unexpected receipt line count")
	}
	const receipt = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
	if string(data) != receipt {
		t.Fatal("idle shutdown receipt changed or contained extra bytes")
	}
}

func TestServiceRunInterruptedTickShutdownRepairsBeforeStop(t *testing.T) {
	for _, failStop := range []bool{false, true} {
		name := "clean"
		if failStop {
			name = "stop_error"
		}
		t.Run(name, func(t *testing.T) {
			c, m := unitCell(t)
			if runtime.GOOS != "windows" {
				// Long test names exceed the Unix socket path limit under TempDir.
				root, err := os.MkdirTemp("/tmp", "lc-shutdown-")
				if err != nil {
					t.Fatal("cannot create short private fixture")
				}
				t.Cleanup(func() { _ = os.RemoveAll(root) })
				c, err = New(Config{Root: root, Timeout: time.Second})
				if err != nil || c.layout() != nil || writeAtomic(c.path("manifest.json"), m, nil) != nil {
					t.Fatal("cannot prepare short private fixture")
				}
			}
			prepareSupervisorTest(t, c)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h, _, _, _, _ := supervisorTestHooks()
			if runtime.GOOS != "windows" {
				h.boot = func() (string, error) { return "synthetic-boot", nil }
			} // Windows endpoint publication must bind to the same native boot as the lease.
			ticks := make(chan time.Time, 2)
			h.ticks = ticks
			var events []string
			healths, stops, repairs := 0, 0, 0
			stopFailure := errors.New("synthetic shutdown stop failure")
			assertFresh := func(opCtx context.Context) {
				t.Helper()
				deadline, bounded := opCtx.Deadline()
				if opCtx.Err() != nil || !bounded || time.Until(deadline) > time.Second {
					t.Fatal("shutdown did not use a fresh bounded context")
				}
			}
			h.repair = func(opCtx context.Context) error {
				repairs++
				if ctx.Err() == nil {
					return c.Repair(opCtx)
				}
				assertFresh(opCtx)
				events = append(events, "repair")
				if repairs == 2 {
					return ErrLocked // Shutdown must retry operation contention.
				}
				return c.Repair(opCtx)
			}
			h.start = func(opCtx context.Context) error {
				if ctx.Err() != nil || opCtx.Err() != nil {
					t.Fatal("start began after cancellation")
				}
				return nil
			}
			h.health = func(opCtx context.Context) error {
				if ctx.Err() != nil || opCtx.Err() != nil {
					t.Fatal("health began after cancellation")
				}
				healths++
				if healths == 1 {
					ticks <- time.Now()
					return nil
				}
				return errors.New("synthetic unhealthy tree")
			}
			h.stop = func(opCtx context.Context) error {
				stops++
				if stops == 1 { // Startup stop, before the first start.
					return nil
				}
				if stops == 2 {
					// Exercise the real journal boundary with no live processes.
					if err := c.begin("stop"); err != nil {
						t.Fatal(err)
					}
					err := c.step("stop", "stop", func() error {
						cancel()
						ticks <- time.Now() // A ready tick races the canceled context.
						return opCtx.Err()
					})
					progress, journalErr := c.readJournal()
					if err == nil || journalErr != nil || progress.pending == nil || progress.pending.Operation != "stop" || progress.pending.Step != "stop" || progress.tornTail {
						t.Fatal("canceled stop did not leave the expected recoverable journal")
					}
					data, readErr := os.ReadFile(c.path("journal", "operations.jsonl"))
					if readErr != nil || !bytes.Contains(data, []byte(`"state":"failed"`)) {
						t.Fatal("canceled step did not record failure")
					}
					if refusal := c.begin("stop"); refusal == nil || refusal.Error() != "local cell operation interrupted; run repair first" {
						t.Fatal("pending journal did not reproduce the old shutdown refusal")
					}
					t.Log("reproduction: operation=stop step=stop state=failed pending=true torn_tail=false; direct stop begin refused")
					return err
				}
				assertFresh(opCtx)
				events = append(events, "stop")
				progress, err := c.readJournal()
				if err == nil && progress.pending != nil {
					// Exercise the production shutdown entry before reporting the
					// ordering failure. Emit only its validated, fixed refusal.
					const refusalText = "local cell operation interrupted; run repair first"
					if refusal := c.stopServiceChildren(opCtx); refusal == nil || refusal.Error() != refusalText {
						t.Fatal("pending shutdown did not reach the production stop refusal")
					}
					t.Fatal("shutdown stop preceded completed repair: " + refusalText)
				}
				if err != nil || progress.pending != nil || progress.tornTail {
					t.Fatal("shutdown stop preceded completed repair")
				}
				if stops == 3 {
					return ErrLocked
				}
				if failStop {
					return stopFailure
				}
				return c.stopServiceChildren(opCtx)
			}
			stderr, err := os.CreateTemp(t.TempDir(), "shutdown-stderr")
			if err != nil {
				t.Fatal("cannot capture shutdown diagnostic")
			}
			previous := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() { os.Stderr = previous; _ = stderr.Close() })
			err = c.Run(ctx, SupervisorPolicy{hooks: &h, ShutdownTimeout: time.Second})
			os.Stderr = previous
			if (!failStop && err != nil) || (failStop && (err == nil || err.Error() != stopFailure.Error())) {
				t.Fatal("Run retained cancellation or lost the shutdown result")
			}
			if !reflect.DeepEqual(events, []string{"repair", "repair", "stop", "stop"}) || healths != 2 {
				t.Fatal("shutdown order, contention retries, or cancellation fence changed")
			}
			data, err := os.ReadFile(stderr.Name())
			if err != nil || string(data) != "native service host site=supervisor_health\nnative service host site=supervisor_stop\nservice_shutdown phase=tick operation=stop journal_pending=true journal_read_ok=true\n" {
				t.Fatal("shutdown diagnostic lost the interrupted phase or leaked values")
			}
			before, err := os.ReadFile(c.path("journal", "operations.jsonl"))
			if err != nil || c.Repair(t.Context()) != nil {
				t.Fatal("repeated repair failed")
			}
			after, err := os.ReadFile(c.path("journal", "operations.jsonl"))
			if err != nil || !bytes.Equal(before, after) || !strings.Contains(string(after), `"step":"revert_stop"`) {
				t.Fatal("repair was not complete and idempotent")
			}
		})
	}
}

func TestServiceSupervisorCanceledTickDoesNotSchedule(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, starts, stops, _ := supervisorTestHooks()
	work := 0
	h.now = func() time.Time { work++; return time.Now() }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if err := s.load(d); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.tick(ctx); !errors.Is(err, context.Canceled) || work != 0 || *starts != 0 || *stops != 0 {
		t.Fatal("a queued tick scheduled work after cancellation")
	}
}

func TestServiceSupervisorCancellationBetweenTickHooks(t *testing.T) {
	for _, operation := range []string{"start", "health"} {
		t.Run(operation, func(t *testing.T) {
			c, _ := unitCell(t)
			d := prepareSupervisorTest(t, c)
			h, _, _, _, _ := supervisorTestHooks()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			starts, healths, stops := 0, 0, 0
			h.start = func(context.Context) error {
				starts++
				if operation == "start" {
					cancel()
				}
				return nil // Cancellation must fence the next hook even on success.
			}
			h.health = func(context.Context) error { healths++; cancel(); return context.Canceled }
			h.stop = func(context.Context) error { stops++; return nil }
			s := newSupervisor(c, SupervisorPolicy{hooks: &h})
			if err := s.load(d); err != nil {
				t.Fatal(err)
			}
			if err := s.tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal("tick returned an unrelated error")
			}
			wantHealths := 0
			if operation == "health" {
				wantHealths = 1
			}
			if starts != 1 || healths != wantHealths || stops != 0 || s.operation != operation || s.phase != "tick" {
				t.Fatal("cancellation scheduled another hook or lost the interrupted phase")
			}
		})
	}
}
