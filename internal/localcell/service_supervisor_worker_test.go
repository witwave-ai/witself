package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
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
	commands               chan serviceCommand
	done                   <-chan error
	cancel                 context.CancelFunc
	mu                     sync.Mutex
	block, gated           string
	armed, freed           bool
	entered, release, gate chan struct{}
	events                 []string
}

func serviceWorkerStart(t *testing.T, block string, armed bool) *serviceWorkerHost {
	return serviceWorkerConfigured(t, block, armed, nil)
}

func serviceWorkerConfigured(t *testing.T, block string, armed bool, prepare func(*Cell, ServiceDescriptor)) *serviceWorkerHost {
	t.Helper()
	cell := serviceCancellationReceiptCell(t)
	d := prepareSupervisorTest(t, cell)
	if prepare != nil {
		prepare(cell, d)
	}
	return serviceWorkerOnCell(t, cell, block, armed)
}

func serviceWorkerOnCell(t *testing.T, cell *Cell, block string, armed bool) *serviceWorkerHost {
	t.Helper()
	w := &serviceWorkerHost{cell: cell, ticks: make(chan time.Time), commands: make(chan serviceCommand), block: block}
	if armed {
		w.arm()
	}
	hooks, _, _, _, _ := supervisorTestHooks()
	hooks.ticks = w.ticks
	hooks.commands = w.commands
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
	if block == "reconcile" {
		reconcile := hook("reconcile")
		hooks.reconcile = func() error { return reconcile(t.Context()) }
	}
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

func serviceColdBusy(status ServiceStatus) bool {
	return status.State == "busy" && status.Reason == "service_operation_in_progress" && !status.Healthy && status.LastStepOutcome == "not_run"
}

// Only the cold, unknown publication can precede the first completed observation here.
func serviceObserveAfterStartup(ctx context.Context, c *Cell) (ServiceStatus, error) {
	for ctx.Err() == nil {
		status, err := c.Control(ctx, "observe")
		if err != nil || !status.WorkInFlight {
			return status, err
		}
		if !serviceColdBusy(status) {
			return ServiceStatus{}, errors.New("unexpected provisional startup observation")
		}
	}
	return ServiceStatus{}, ctx.Err()
}

func serviceHealthyAfterStartup(ctx context.Context, c *Cell) error {
	status, err := serviceObserveAfterStartup(ctx, c)
	if err != nil || status.State != "running" || !status.Healthy {
		return errors.New("startup did not publish healthy running")
	}
	return nil
}

// Endpoint publication may race the first call. Inspect the first settled response.
func (w *serviceWorkerHost) ready(t *testing.T) ServiceStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), serviceControlBudget(w.cell.cfg.Timeout, 3))
	defer cancel()
	for {
		// A cold host has no discovery yet. Poll for publication before the
		// first request. Once published, every admission refusal is fatal.
		published := false
		if runtime.GOOS == "windows" {
			_, err := os.Lstat(w.cell.path("service", "control-endpoint.json"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the worker host did not settle within the row bound")
			}
			published = err == nil
		} else {
			published = w.cell.validateServiceControlEndpoint(t.Context()) == nil
		}
		if published {
			status := serviceWorkerSettled(ctx, t, w.cell.Control)
			if status.State != "running" || !status.Healthy || status.LastStepOutcome != "ok" {
				t.Fatal("the first settled worker observation was not healthy running")
			}
			return status
		}
		select {
		case <-ctx.Done():
			t.Fatal("the worker host did not settle within the row bound")
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
// release: observe and status read the last publication, every other action is deferred, and
// nothing it asked for runs then or after.
func TestServiceWorkerAnswers(t *testing.T) {
	for _, action := range []string{"observe", "status", "health", "start", "stop", "repair"} {
		t.Run(action, func(t *testing.T) {
			w := serviceWorkerStart(t, "health", false)
			want := w.ready(t)
			want.WorkInFlight = true
			w.blockTick(t)
			before := w.recorded()
			status, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), action) })
			if action == "observe" || action == "status" {
				if err != nil {
					t.Fatal("a query made during work was answered with an error")
				}
				if !reflect.DeepEqual(status, want) {
					t.Fatal("a query during work changed the last published snapshot")
				}
			} else if err != errServiceControlDeferred || !reflect.DeepEqual(status, ServiceStatus{}) {
				t.Fatal("deferred action changed service effects")
			}
			if !slices.Equal(w.recorded(), before) {
				t.Fatal("deferred action changed service effects")
			}
			w.free()
			w.ready(t)
			if !slices.Equal(w.recorded(), append(before, "health_returned")) {
				t.Fatal("deferred action changed service effects")
			}
			if d, err := w.cell.ServiceDescriptor(); err != nil || d.DesiredState != "running" {
				t.Fatal("deferred action changed service effects")
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
		if status, err := w.cell.Control(t.Context(), "status"); err != nil || status.State != "running" || status.WorkInFlight {
			t.Fatal("completed command remained marked as active")
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

// The handler seam distinguishes an actual reply from its HTTP deadline fallback.
func TestServiceWorkerCommandReplyOnShutdown(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	w.arm()
	reply := w.boundaryCommand(t, "status")
	w.within(t, w.entered, nil, "shutdown command did not reach its hook")
	parked := w.boundaryCommand(t, "exit-host")
	// The next receive proves the loop finished admitting the parked exit
	// before cancellation. A send to the unbuffered queue alone cannot.
	_ = w.boundaryCommand(t, "observe")
	w.cancel()
	// Two consecutive receives prove the first post-cancel request did not
	// return from Run while the hook was still held.
	for range 2 {
		select {
		case w.commands <- serviceCommand{ctx: t.Context(), action: "observe", reply: make(chan serviceReply, 1)}:
		case <-w.done:
			t.Fatal("request after cancellation ended the blocked host")
		case <-time.After(w.cell.cfg.Timeout):
			t.Fatal("cancelled host did not receive the join probe")
		}
	}
	w.free()
	_ = serviceWorkerReceive(t, w, w.done, "released shutdown command did not finish")
	select {
	case got := <-reply:
		if got.err != nil && !errors.Is(got.err, context.Canceled) {
			t.Fatal("shutdown command returned an unrelated failure")
		}
	default:
		t.Fatal("the command in flight at shutdown lost its reply")
	}
	select {
	case <-parked:
		t.Fatal("host cancellation promoted a waiting exit")
	default:
	}
}

func TestServiceWorkerStartupBusy(t *testing.T) {
	for _, plant := range []string{"unknown", "wrong_revision"} {
		t.Run(plant, func(t *testing.T) { serviceWorkerCold(t, plant, false) })
	}
}

func TestServiceWorkerStartupFault(t *testing.T) {
	for _, plant := range []string{"descriptor", "circuit"} {
		t.Run(plant, func(t *testing.T) { serviceWorkerCold(t, plant, true) })
	}
}

func serviceWorkerCold(t *testing.T, plant string, faulted bool) {
	t.Helper()
	var want ServiceStatus
	w := serviceWorkerConfigured(t, "reconcile", true, func(c *Cell, d ServiceDescriptor) {
		want = ServiceStatus{DesiredState: d.DesiredState, Generation: d.Payload.Generation, WorkInFlight: true, LastStepOutcome: "not_run", State: "busy", Reason: "service_operation_in_progress"}
		if faulted {
			want.State, want.Reason = "faulted", "service_circuit_open"
		}
		if plant == "descriptor" && c.setServiceFaulted(t.Context(), true) != nil {
			t.Fatal("could not seed a descriptor fault")
		}
		if plant == "wrong_revision" || plant == "circuit" {
			r := supervisorRecord{Version: 1, RepairRevision: d.RepairRevision, Failures: make([]time.Time, 5)}
			if plant == "wrong_revision" {
				d.RepairRevision++
				if c.writeServiceDescriptor(d) != nil {
					t.Fatal("could not supersede the failure record")
				}
			}
			if writeAtomic(c.path("service", "supervisor.json"), r, nil) != nil {
				t.Fatal("could not seed the failure record")
			}
		}
	})
	w.within(t, w.entered, nil, "first reconcile did not enter")
	var lease serviceLeaseRecord
	if readJSON(t.Context(), w.cell.path("service", "lease.json"), &lease) != nil || lease.PID <= 1 || lease.StartIdentity == "" {
		t.Fatal("cold fixture did not publish its lease identity")
	}
	want.Processes = []ServiceProcessIdentity{{Component: "service-host", PID: lease.PID, StartTime: lease.StartIdentity}}
	for _, action := range []string{"observe", "status"} {
		got, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), action) })
		if err != nil || !reflect.DeepEqual(want, got) {
			if faulted {
				t.Fatal("cold host hid a known circuit fault")
			}
			t.Fatal("cold host reported desired state as observed")
		}
	}
	w.free()
	if !faulted {
		w.ready(t)
	}
}

// The observe row once more on one host, for the race detector (R9 cut it from
// fifty rounds to one): the loop and the worker hand the supervisor over.
func TestServiceWorkerRace(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	want := w.ready(t)
	want.WorkInFlight = true
	for range 1 {
		w.blockTick(t)
		if status, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), "observe") }); err != nil || !reflect.DeepEqual(status, want) {
			t.Fatal("a query during work changed the last published snapshot")
		}
		w.free()
		w.ready(t)
	}
}

func TestServiceWorkerMaintenanceSnapshot(t *testing.T) {
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	serviceMaintenanceHostPlant(t, w.cell, "Valid")
	want, err := w.cell.Control(t.Context(), "status")
	if err != nil || want.State != "maintenance" || want.Healthy || want.WorkInFlight {
		t.Fatal("fixture did not publish a settled maintenance hold")
	}
	serviceMaintenanceHostClear(t, w.cell)
	w.blockTick(t)
	want.WorkInFlight = true
	for _, action := range []string{"observe", "status"} {
		got, err := w.answer(t, func() (ServiceStatus, error) { return w.cell.Control(t.Context(), action) })
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("the in-flight query lost the published hold")
		}
	}
	w.free()
	w.ready(t)
}

func TestServiceWorkerSnapshotCopy(t *testing.T) {
	const original = 42
	s := supervisor{status: ServiceStatus{Processes: []ServiceProcessIdentity{{PID: original}}}}
	held := s.projectedStatus()
	first := deferServiceCommand("observe", held)
	first.status.Processes[0].PID++
	second := deferServiceCommand("status", held)
	if held.Processes[0].PID != original || second.status.Processes[0].PID != original {
		t.Fatal("held response modified an earlier observation")
	}
	held.Processes[0].PID++
	if s.status.Processes[0].PID != original || second.status.Processes[0].PID != original {
		t.Fatal("held response modified an earlier observation")
	}
}

func TestServiceWorkerStatusWire(t *testing.T) {
	for _, name := range []string{"settled", "marked"} {
		t.Run(name, func(t *testing.T) {
			w := serviceWorkerStart(t, "health", false)
			want := w.ready(t)
			if name == "marked" {
				want.WorkInFlight = true
				w.blockTick(t)
			}
			got, err := w.cell.Control(t.Context(), "observe")
			body := httptest.NewRecorder()
			writeServiceControlResponse(body, "observe", got, io.Discard)
			raw := body.Body.Bytes()
			count := 0
			if want.WorkInFlight {
				count = 1
			}
			if err != nil || !reflect.DeepEqual(got, want) || body.Code != 200 || bytes.Count(raw, []byte(`"work_in_flight"`)) != count || bytes.Count(raw, []byte(`"work_in_flight":true`)) != count {
				t.Fatal("work marker changed settled response shape")
			}
			decode := func(input []byte) (ServiceStatus, error) {
				var status ServiceStatus
				d := json.NewDecoder(bytes.NewReader(input))
				d.DisallowUnknownFields()
				err := d.Decode(&status)
				return status, err
			}
			roundTrip, decodeErr := decode(raw)
			_, unknownErr := decode(bytes.Replace(raw, []byte("{"), []byte(`{"other":true,`), 1))
			if decodeErr != nil || !reflect.DeepEqual(roundTrip, want) || unknownErr == nil {
				t.Fatal("work marker changed settled response shape")
			}
		})
	}
}

// One of two concurrent exit clients must receive a drain refusal while a
// status worker is held. The other receives its ACK before stop can finish.
func TestServiceWorkerPairedExit(t *testing.T) {
	const fail = "held exit changed its admission outcome"
	w := serviceWorkerStart(t, "health", false)
	w.ready(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	w.arm()
	status := StartJoinedTestRunForTest(t, cancel, w.cell.cfg.Timeout, func() error {
		_, err := w.cell.Control(ctx, "status")
		return err
	})
	defer w.free()
	w.within(t, w.entered, nil, fail)
	drain := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(drain) }) }
	defer unblock()
	w.mu.Lock()
	w.gated, w.gate = "stop", drain
	w.mu.Unlock()
	type reply struct {
		session *serviceHostExitSession
		err     error
	}
	acks := make(chan reply, 2)
	for range 2 {
		envelope, _, err := w.cell.newHostExitEnvelope()
		if err != nil {
			t.Fatal(fail)
		}
		_ = StartJoinedTestRunForTest(t, cancel, w.cell.cfg.Timeout, func() error {
			session, err := w.cell.startHostExitRequest(ctx, envelope)
			acks <- reply{session, err}
			return nil
		})
	}
	first := <-acks
	var refused *serviceControlRefusal
	if first.session != nil || !errors.As(first.err, &refused) || refused.step != "host_exit:drain" || errors.Is(first.err, context.DeadlineExceeded) {
		t.Fatal("live parked exit failed to refuse another exit")
	}
	w.free()
	if err := <-status; err != nil {
		t.Fatal(fail)
	}
	second := <-acks
	if second.err != nil || second.session == nil {
		t.Fatal(fail)
	}
	defer second.session.cancel()
	select {
	case <-w.done:
		t.Fatal(fail)
	default:
	}
	unblock()
	second.session.finish()
	if second.session.err != nil || second.session.disposition != "drained" {
		t.Fatal(fail)
	}
	if err := <-w.done; err != nil {
		t.Fatal(fail)
	}
}

func TestServiceWorkerIdleObservers(t *testing.T) {
	for stage, count := range map[string]int{"ticks": 8, "health": 4} {
		t.Run(stage, func(t *testing.T) {
			w := serviceWorkerStart(t, "", false)
			w.ready(t)
			ctx, cancel := context.WithCancel(t.Context())
			var readers sync.WaitGroup
			started := make(chan struct{}, count)
			defer func() { cancel(); readers.Wait() }()
			for range count {
				readers.Go(func() {
					_, err := w.cell.Control(ctx, "observe")
					started <- struct{}{}
					for err == nil && ctx.Err() == nil {
						_, err = w.cell.Control(ctx, "observe")
					}
					if err != nil && ctx.Err() == nil {
						t.Error("concurrent observer lost its control reply")
					}
				})
			}
			for range count {
				w.within(t, started, nil, "observer did not complete its first request")
			}
			if stage == "health" {
				completed := 0
				for range 200 {
					if _, err := w.cell.Control(t.Context(), "health"); err == nil {
						completed++
					}
				}
				if completed != 200 {
					t.Fatal("idle observers prevented a health decision")
				}
				return
			}
			before := len(w.recorded())
			const ticks = 12
			for range ticks {
				w.within(t, nil, w.ticks, "reader traffic blocked delivery of a tick")
				w.ready(t) // Later ticks cannot overlap earlier tick work.
			}
			if !slices.Equal(w.recorded()[before:], slices.Repeat([]string{"health"}, ticks)) {
				t.Fatal("idle observers displaced a supervision tick")
			}
		})
	}
}

func serviceWorkerReceive[T any](t *testing.T, w *serviceWorkerHost, ch <-chan T, text string) (value T) {
	t.Helper()
	select {
	case value = <-ch:
	case <-time.After(w.cell.cfg.Timeout):
		t.Fatal(text)
	}
	return value
}

func TestServiceWorkerExitWhileBusy(t *testing.T) {
	for _, stage := range []string{"abandoned_retry", "abandoned_promotion"} {
		t.Run(stage, func(t *testing.T) {
			w := serviceWorkerStart(t, "health", false)
			w.ready(t)
			w.blockTick(t)
			ctx, abandon := context.WithCancel(t.Context())
			defer abandon()
			stale := serviceCommand{ctx: ctx, action: "exit-host", reply: make(chan serviceReply, 1)}
			w.commands <- stale
			_ = w.boundaryCommand(t, "observe") // The first exit has been parked.
			abandon()
			if stage == "abandoned_retry" {
				envelope, _, err := w.cell.newHostExitEnvelope()
				if err != nil {
					t.Fatal("could not prepare the retry envelope")
				}
				done := make(chan string, 1)
				ack := make(chan serviceReply, 1)
				delivered := make(chan struct{})
				w.commands <- serviceCommand{ctx: t.Context(), action: "exit-host", exit: &envelope, reply: ack, delivered: delivered, completed: done, finalized: delivered}
				w.free()
				if response := serviceWorkerReceive(t, w, ack, "parked retry did not receive a decision"); response.err != nil || response.status.HostExit != "accepted" {
					t.Fatal("abandoned parked exit denied the retry")
				}
				close(delivered)
				if serviceWorkerReceive(t, w, done, "accepted retry missed its terminal reply") != "drained" {
					t.Fatal("replacement exit did not complete its drain")
				}
			} else {
				w.free()
				w.ready(t)
			}
			if len(stale.reply) != 0 {
				t.Fatal("abandoned exit was promoted after its handler returned")
			}
		})
	}
}
