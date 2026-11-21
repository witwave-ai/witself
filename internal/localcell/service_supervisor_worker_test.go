package localcell

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// serviceWorkerHost runs Run over supervisorTestHooks with manual ticks. After
// arm, the next call of its block hook waits until free, blind to cancellation
// like a lease wait; every call of its gated hook waits for gate or its context.
type serviceWorkerHost struct {
	cell                   *Cell
	ticks                  chan time.Time
	done                   <-chan error
	cancel                 context.CancelFunc
	mu                     sync.Mutex
	block, gated           string
	armed, freed           bool
	entered, release, gate chan struct{}
	events                 []string
}

func serviceWorkerStart(t *testing.T, block string, armed bool) *serviceWorkerHost {
	cell := serviceCancellationReceiptCell(t)
	prepareSupervisorTest(t, cell)
	w := &serviceWorkerHost{cell: cell, ticks: make(chan time.Time), block: block}
	if armed {
		w.arm()
	}
	hooks, _, _, _, _ := supervisorTestHooks()
	hooks.ticks = w.ticks
	hook := func(name string) func(context.Context) error {
		return func(ctx context.Context) error {
			w.mu.Lock()
			blocked, entered, release, gate := name == w.block && w.armed, w.entered, w.release, map[bool]chan struct{}{true: w.gate}[name == w.gated]
			w.armed, w.events = w.armed && !blocked, append(w.events, name)
			w.mu.Unlock()
			if blocked {
				close(entered)
				<-release
				w.mu.Lock()
				w.events = append(w.events, name+"_returned")
				w.mu.Unlock()
			}
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
				}
			}
			return nil
		}
	}
	hooks.start, hooks.stop, hooks.repair, hooks.health = hook("start"), hook("stop"), hook("repair"), hook("health")
	ctx, cancel := context.WithCancel(t.Context())
	w.cancel = cancel
	w.done = StartJoinedTestRunForTest(t, cancel, serviceControlBudget(cell.cfg.Timeout, 4), func() error {
		return cell.Run(ctx, SupervisorPolicy{hooks: &hooks, ShutdownTimeout: cell.cfg.Timeout})
	})
	t.Cleanup(w.free) // Before the join: a failed row never leaves the hook blocked.
	return w
}

func (w *serviceWorkerHost) recorded() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

func (w *serviceWorkerHost) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed, w.freed, w.entered, w.release = true, false, make(chan struct{}), make(chan struct{})
}

func (w *serviceWorkerHost) free() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.release != nil && !w.freed {
		w.freed = true
		close(w.release)
	}
}

func (w *serviceWorkerHost) within(t *testing.T, ready <-chan struct{}, ticks chan<- time.Time, text string) {
	select {
	case <-ready:
	case ticks <- time.Now():
	case <-time.After(w.cell.cfg.Timeout):
		t.Fatal(text)
	}
}

func (w *serviceWorkerHost) blockTick(t *testing.T) {
	w.arm()
	w.within(t, nil, w.ticks, "the loop did not take a tick within the row bound")
	w.within(t, w.entered, nil, "the armed hook was not reached within the row bound")
}

// ready polls observe until the host reads running; a host doing work reads busy.
func (w *serviceWorkerHost) ready(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(w.cell.cfg.Timeout, 3))
	defer cancel()
	for {
		if status, err := w.cell.Control(ctx, "observe"); err == nil && status.State == "running" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("the worker host did not read running within the row bound")
		case <-time.After(time.Millisecond):
		}
	}
}

// answer makes one request while the hook is blocked. One that waits a quarter
// of the client's budget is behind the work: the hook is freed and the row fails.
func (w *serviceWorkerHost) answer(t *testing.T, call func() (ServiceStatus, error)) (ServiceStatus, error) {
	got := make(chan serviceReply, 1)
	go func() { status, err := call(); got <- serviceReply{status: status, err: err} }()
	select {
	case reply := <-got:
		return reply.status, reply.err
	case <-time.After(w.cell.cfg.Timeout):
		w.free()
		<-got
		t.Fatal("a request made during work waited behind the blocked hook")
		return ServiceStatus{}, nil
	}
}

// A request made while a tick's health hook is blocked is answered before the
// release: observe and status read busy, every other action is deferred, and
// nothing it asked for runs then or after.
func TestServiceWorkerAnswers(t *testing.T) {
	for _, action := range []string{"observe", "status", "health", "start", "stop", "repair"} {
		t.Run(action, func(t *testing.T) {
			w := serviceWorkerStart(t, "health", false)
			w.ready(t)
			w.blockTick(t)
			before := w.recorded()
			status, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), action) })
			if action == "observe" || action == "status" {
				if err != nil {
					t.Fatal("a query made during work was answered with an error")
				}
				if status.State != "busy" || status.Reason != "service_operation_in_progress" || status.Healthy {
					t.Fatal("a query made during work did not read the held status as busy")
				}
			} else if !errors.Is(err, errServiceControlDeferred) {
				t.Fatal("a control action made during work was not deferred")
			}
			if !slices.Equal(w.recorded(), before) {
				t.Fatal("a request made during work ran a lifecycle hook")
			}
			w.free()
			w.ready(t)
			if !slices.Equal(w.recorded(), append(before, "health_returned")) {
				t.Fatal("a request made during work was applied after the work ended")
			}
			if d, err := w.cell.ServiceDescriptor(); err != nil || d.DesiredState != "running" {
				t.Fatal("a request made during work changed the desired state")
			}
		})
	}
}

// Three ticks taken while a tick is blocked are skipped, not queued: health
// runs once until the release, then once for each later tick.
func TestServiceWorkerTickSkipped(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	w.blockTick(t)
	healths := func() int { return len(slices.DeleteFunc(w.recorded(), func(e string) bool { return e != "health" })) }
	blocked := healths()
	for range 3 {
		w.within(t, nil, w.ticks, "the loop did not take a tick within the row bound")
	}
	if healths() != blocked {
		t.Fatal("a tick taken during work ran health before the work ended")
	}
	w.free()
	w.ready(t)
	if healths() != blocked {
		t.Fatal("a tick taken during work was queued and ran after the work")
	}
	for later := 1; later <= 2; later++ {
		w.within(t, nil, w.ticks, "the loop did not take a tick within the row bound")
		w.ready(t)
		if healths() != blocked+later {
			t.Fatal("a tick after the work did not run health exactly once")
		}
	}
}

// A client sending its next request on receipt is never deferred by its own command.
func TestServiceWorkerReplyThenNext(t *testing.T) {
	w := serviceWorkerStart(t, "", false)
	w.ready(t)
	for range 200 {
		if status, err := w.cell.Control(t.Context(), "status"); err != nil || status.State != "running" {
			t.Fatal("a sequential status request was deferred by the one before it")
		}
	}
}

// Shutdown joins the worker: Run does not return while a tick's hook is
// blocked, and the deferred shutdown's repair and stop run once each after it.
func TestServiceWorkerShutdownJoins(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	w.blockTick(t)
	cancelled := len(w.recorded())
	w.cancel()
	select {
	case <-w.done:
		t.Fatal("Run returned while its worker was still blocked")
	case <-time.After(w.cell.cfg.Timeout / 10):
	}
	w.free()
	select {
	case <-w.done:
	case <-time.After(serviceControlBudget(w.cell.cfg.Timeout, 2)):
		t.Fatal("Run did not return within its bound after the worker")
	}
	if !slices.Equal(w.recorded()[cancelled:], []string{"health_returned", "repair", "stop"}) {
		t.Fatal("the deferred shutdown did not run repair and stop once each after the worker")
	}
}

// A command in flight at shutdown still gets its reply, published by the loop
// after the join; the shutdown's repair waits for the receipt, so closing the
// endpoint cannot race the response.
func TestServiceWorkerCommandReplyOnShutdown(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	received := make(chan struct{})
	w.mu.Lock()
	w.gated, w.gate = "repair", received
	w.mu.Unlock()
	w.arm()
	replies := make(chan error, 1)
	go func() { _, err := w.cell.Control(t.Context(), "status"); replies <- err; close(received) }()
	w.within(t, w.entered, nil, "the armed hook was not reached within the row bound")
	w.cancel()
	w.free()
	// Freed before or after the host's cancellation reaches the command, it
	// completes or is deferred; a lost reply is a closed transport instead.
	if err := <-replies; err != nil && !errors.Is(err, errServiceControlDeferred) {
		t.Fatal("the command in flight at shutdown lost its reply")
	}
}

// Startup is work too: observe reads busy while its repair hook is blocked.
func TestServiceWorkerStartupBusy(t *testing.T) {
	w := serviceWorkerStart(t, "repair", true)
	w.within(t, w.entered, nil, "the armed hook was not reached within the row bound")
	if status, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), "observe") }); err != nil || status.State != "busy" {
		t.Fatal("observe during startup did not read busy at once")
	}
	w.free()
	w.ready(t)
}

// The observe row once more on one host, for the race detector (R9 cut it from
// fifty rounds to one): the loop and the worker hand the supervisor over.
func TestServiceWorkerRace(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	for range 1 {
		w.blockTick(t)
		if status, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), "observe") }); err != nil || status.State != "busy" {
			t.Fatal("a round of observe during work did not read busy")
		}
		w.free()
		w.ready(t)
	}
}
