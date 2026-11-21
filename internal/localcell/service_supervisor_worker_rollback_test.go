package localcell

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func serviceWorkerCheckControl(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		return
	}
	step, site := "none", "none"
	var refusal *serviceControlRefusal
	if errors.As(err, &refusal) {
		step = refusal.step
	}
	var located *serviceFailure
	if errors.As(err, &located) {
		site = serviceFailureSite(located.site)
	}
	t.Fatalf("%s (type=%T step=%s site=%s)", message, err, step, site)
}

// On Windows a client that asks while the host publishes its descriptor and its endpoint record is refused at the
// endpoint record before it reaches the host. That is the same on main, is pinned by the endpoint_refresh row and is
// recorded as the follow-up LC-HOST-ENDPOINT-1. On other systems the refusal cannot occur here and stays fatal.
func serviceWorkerEndpointGap(err error) bool {
	var refusal *serviceControlRefusal
	return runtime.GOOS == "windows" && errors.As(err, &refusal) && refusal.step == "endpoint:record"
}

// A client that waits for the host to settle asks again after the Windows publication gap, as this helper does.
func serviceWorkerSettled(ctx context.Context, t *testing.T, c *Cell) ServiceStatus {
	t.Helper()
	for {
		v, err := c.Control(ctx, "observe")
		gap := serviceWorkerEndpointGap(err)
		if !gap {
			serviceWorkerCheckControl(t, err, "could not obtain a settled control observation")
			if !v.WorkInFlight {
				return v
			}
		}
		select {
		case <-ctx.Done():
			if gap {
				t.Fatal("host stayed refused at the endpoint record until the deadline")
			}
			t.Fatal("worker did not settle before its deadline")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestServiceWorkerCanceledQueryFault(t *testing.T)    { serviceWorkerCanceledQueries(t, true) }
func TestServiceWorkerCanceledQueryRollback(t *testing.T) { serviceWorkerCanceledQueries(t, false) }

func serviceWorkerCanceledQueries(t *testing.T, refusal bool) {
	for _, verb := range []string{"health", "status"} {
		for _, prior := range []string{"running", "prior_hold"} {
			t.Run(verb+"/"+prior, func(t *testing.T) {
				c := serviceCancellationReceiptCell(t)
				prepareSupervisorTest(t, c)
				h, _, _, _, _ := supervisorTestHooks()
				h.windowsShutdownPolicy = true
				h.ticks = make(chan time.Time)
				started, entered := make(chan struct{}), make(chan struct{})
				var healths, stops atomic.Int32
				var armed atomic.Bool
				block := func(ctx context.Context) error {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
				h.health = func(ctx context.Context) error {
					if healths.Add(1) == 1 {
						close(started)
					}
					if armed.Load() {
						if refusal {
							return errors.New("unhealthy")
						}
						return block(ctx)
					}
					return nil
				}
				h.stop = func(ctx context.Context) error {
					stops.Add(1)
					if refusal && armed.Load() {
						return block(ctx)
					}
					return nil
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				hostCtx, stop := context.WithCancel(ctx)
				done := StartJoinedTestRunForTest(t, stop, 2*c.cfg.Timeout, func() error {
					return c.Run(hostCtx, SupervisorPolicy{hooks: &h, ShutdownTimeout: time.Second})
				})
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("startup health was not reached")
				}
				before := serviceWorkerSettled(ctx, t, c)
				if before.State != "running" || !before.Healthy {
					t.Fatal("expected healthy startup")
				}
				if prior == "prior_hold" {
					serviceMaintenanceHostPlant(t, c, "Valid")
					var err error
					before, err = c.Control(ctx, "status")
					if err != nil || before.State != "maintenance" || before.WorkInFlight {
						t.Fatal("could not publish initial maintenance hold")
					}
					serviceMaintenanceHostClear(t, c)
				}
				armed.Store(true)
				requestCtx, abandon := context.WithCancel(ctx)
				request := StartJoinedTestRunForTest(t, abandon, c.cfg.Timeout, func() error { _, err := c.Control(requestCtx, verb); return err })
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("command did not reach cancellation hook")
				}
				abandon()
				if err := <-request; err == nil {
					t.Fatal("canceled command succeeded")
				}
				after := serviceWorkerSettled(ctx, t, c)
				want := before
				text := "canceled query changed its earlier observation"
				if refusal {
					want.State, want.Reason, want.Healthy = "faulted", "service_stop_refused", false
					text = "canceled query erased its new stop refusal"
				}
				armed.Store(false)
				stop()
				hostErr := <-done
				d, err := c.ServiceDescriptor()
				if err != nil || hostErr != nil || d.Faulted != refusal || !reflect.DeepEqual(after, want) || stops.Load() != 2 {
					t.Fatal(text)
				}
			})
		}
	}
}

func TestServiceWorkerExistingHoldRollback(t *testing.T) {
	f := newServiceMaintenanceHostFixture(t)
	s := f.s
	s.hooks.windowsShutdownPolicy = true
	if err := s.step(t.Context()); err != nil || !s.running {
		t.Fatal("startup did not reach running")
	}
	serviceMaintenanceHostPlant(t, f.cell, "Valid")
	hold, err := s.command(t.Context(), "status")
	if err != nil || hold.State != "maintenance" {
		t.Fatal("could not establish maintenance hold")
	}
	stopCtx, cancel := context.WithCancel(t.Context())
	stops := 0
	s.hooks.stop = func(context.Context) error { stops++; cancel(); return context.Canceled }
	_, err = s.command(stopCtx, "stop")
	cancel()
	if !errors.Is(err, context.Canceled) || !s.shutdownBlocked {
		t.Fatal("explicit canceled stop did not latch refusal")
	}
	afterStop, err := s.command(t.Context(), "observe")
	if err != nil || !reflect.DeepEqual(afterStop, hold) {
		t.Fatal("rollback displaced a confirmed maintenance hold")
	}
	if _, err := s.command(t.Context(), "stop"); !errors.Is(err, ErrServiceControlRefused) || stops != 1 {
		t.Fatal("unsafe stop was repeated")
	}
	before, err := s.command(t.Context(), "status")
	if err != nil || before.State != "maintenance" {
		t.Fatal("could not publish existing held fault")
	}
	ctx, abandon := context.WithCancel(t.Context())
	now := s.hooks.now
	s.hooks.now = func() time.Time { abandon(); return now() }
	_, err = s.command(ctx, "status")
	abandon()
	if !errors.Is(err, context.Canceled) {
		t.Fatal("existing held query escaped cancellation")
	}
	after, err := s.command(t.Context(), "observe")
	d, readErr := f.cell.ServiceDescriptor()
	if err != nil || readErr != nil || !d.Faulted || !reflect.DeepEqual(before, after) {
		t.Fatal("rollback displaced a confirmed maintenance hold")
	}
}
