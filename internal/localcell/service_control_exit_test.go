package localcell

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServiceHostExitChallenge(t *testing.T) {
	for _, value := range []string{"", "A234567ABCDEFGHIJKLMNOPQRSTUVWXYZ", "aaaaaaaaaaaaaaaaaaaaaaaaab", "aaaaaaaaaaaaaaaaaaaaaaaaaa!", "../private"} {
		if validHostExitChallenge(value) {
			t.Fatal("malformed challenge accepted")
		}
	}
	if !validHostExitChallenge("aaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatal("canonical challenge refused")
	}
}

func TestServiceHostExitDrainDoesNotCancelBeforeStop(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, _, _, _ := supervisorTestHooks()
	entered, release := make(chan struct{}), make(chan struct{})
	h.stop = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
		t.Fatal("cannot load exit fixture")
	}
	delivered := make(chan struct{})
	finished := make(chan bool, 1)
	go func() { finished <- s.finishHostExit(t.Context(), delivered) }()
	select {
	case <-entered:
		t.Fatal("drain preceded response delivery")
	default:
	}
	close(delivered)
	<-entered
	select {
	case <-finished:
		t.Fatal("host exited before drain completed")
	default:
	}
	close(release)
	if !<-finished || !s.exitDrained {
		t.Fatal("completed drain did not authorize exit")
	}
}

func TestServiceHostExitDrainRefusalFaults(t *testing.T) {
	c, _ := unitCell(t)
	d := prepareSupervisorTest(t, c)
	h, _, starts, _, _ := supervisorTestHooks()
	h.stop = func(context.Context) error { return errors.New("fixture unknown stop") }
	s := newSupervisor(c, SupervisorPolicy{hooks: &h})
	if s.load(d) != nil {
		t.Fatal("cannot load exit refusal fixture")
	}
	delivered := make(chan struct{})
	close(delivered)
	if s.finishHostExit(t.Context(), delivered) || s.exitDrained {
		t.Fatal("failed drain authorized exit")
	}
	after, err := c.ServiceDescriptor()
	if err != nil || !after.Faulted || s.status.HostExit != "refused" || *starts != 0 {
		t.Fatal("unknown drain did not fault without replacement")
	}
}

func TestServiceHostExitRemoveOrderingContract(t *testing.T) {
	source, err := os.ReadFile("lifecycle.go")
	if err != nil {
		t.Fatal("cannot read removal contract")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	start := strings.Index(normalized, "func (c *Cell) Remove(")
	end := strings.Index(normalized[start:], "func (c *Cell) finishRemoval(")
	if start < 0 || end < 0 {
		t.Fatal("cannot locate removal contract")
	}
	body := normalized[start : start+end]
	check := func(body string) bool {
		observed := strings.Index(body, "c.exitServiceHostForRemoval(ctx)")
		locked := strings.Index(body, "c.ownExisting()")
		removed := strings.Index(body, "os.RemoveAll(c.path(name))")
		return observed >= 0 && locked > observed && removed > locked
	}
	if !check(body) {
		t.Fatal("removal can mutate records before host-exit observation")
	}
	mutant := strings.Replace(body, "c.exitServiceHostForRemoval(ctx)", "nil", 1)
	if check(mutant) {
		t.Fatal("ordering contract missed host-exit bypass")
	}
	// CRLF checkout replay exercises the exact source normalization, without
	// rewriting the worker's shared source during other package compilation.
	replay := strings.ReplaceAll(strings.ReplaceAll(body, "\n", "\r\n"), "\r\n", "\n")
	if !check(replay) {
		t.Fatal("removal contract does not survive CRLF checkout")
	}
}

func TestServiceShutdownControlBudget(t *testing.T) {
	for _, platform := range []string{"windows", "linux", "darwin"} {
		for _, action := range []string{"stop", "repair", "health"} {
			want := 4 * time.Second
			if platform == "windows" && action != "health" {
				want = ServiceShutdownTimeout
			}
			if serviceControlActionBudgetForPlatform(time.Second, 4, action, platform) != want {
				t.Fatal("shutdown control and host budgets diverged")
			}
			if platform == runtime.GOOS && serviceControlActionBudget(time.Second, 4, action) != want {
				t.Fatal("native action selection diverged from its platform budget")
			}
		}
	}
}

func TestServiceHostExitTerminalReceipt(t *testing.T) {
	challenge := "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, test := range []struct {
		name, disposition, echo string
		want                    error
	}{
		{"graceful", "drained", challenge, nil},
		{"forced", "forced", challenge, ErrServiceShutdownForced},
		{"refused", "refused", challenge, ErrServiceControlRefused},
		{"unknown", "unknown", challenge, ErrServiceControlRefused},
		{"ack_is_not_completion", "accepted", challenge, ErrServiceControlRefused},
		{"challenge_mismatch", "drained", "mismatched", ErrServiceControlRefused},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"disposition":"` + test.disposition + `","challenge":"` + test.echo + `"}` + "\n"
			session := &serviceHostExitSession{body: io.NopCloser(strings.NewReader(body)), cancel: func() {}, transport: &http.Transport{}, challenge: challenge}
			session.finish()
			if !errors.Is(session.err, test.want) {
				t.Fatal("terminal receipt lost typed disposition or challenge fence")
			}
		})
	}
	for _, body := range []string{"", `{"disposition":"drained"}`, strings.Repeat(" ", 4097),
		`{"disposition":"drained","challenge":"` + challenge + `"}` + "\n{}\n",
		`{"disposition":"drained","challenge":"` + challenge + `"}` + "\n" + strings.Repeat(" ", 4097),
	} {
		session := &serviceHostExitSession{body: io.NopCloser(strings.NewReader(body)), cancel: func() {}, transport: &http.Transport{}, challenge: challenge}
		session.finish()
		if !errors.Is(session.err, ErrServiceControlRefused) {
			t.Fatal("malformed or oversized terminal receipt accepted")
		}
	}
}

func TestServiceHostExitPermittedForcedWrapper(t *testing.T) {
	if !serviceStopPermitsHostExit(serviceFailureAt(ErrServiceShutdownForced, "supervisor_stop")) {
		t.Fatal("diagnostic wrapper hid confirmed forced drain")
	}
	mixed := serviceFailureAt(errors.Join(ErrServiceShutdownForced, errors.New("unknown child")), "supervisor_stop")
	if serviceStopPermitsHostExit(mixed) {
		t.Fatal("forced prefix authorized exit despite unknown child")
	}
}

// Admission covers a queued health operation plus response delivery. It cannot
// consume the separately reserved tree-drain allocation.
func TestServiceHostExitBudgetPolicy(t *testing.T) {
	const maximum = time.Duration(1<<63 - 1)
	for _, tc := range []struct {
		name                           string
		health, admission, transaction time.Duration
	}{
		{"small_health", time.Millisecond, 2 * time.Millisecond, ServiceShutdownTimeout + 2*time.Millisecond},
		{"native_health", time.Minute, 2 * time.Minute, ServiceShutdownTimeout + 2*time.Minute},
		{"admission_saturates", maximum, maximum, maximum},
		{"transaction_saturates", maximum/2 - 1, maximum - 3, maximum},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceHostExitBudgets(tc.health)
			if got.admission != tc.admission || got.transaction != tc.transaction {
				t.Fatal("host-exit policy lost proportional admission or independent drain allocation")
			}
		})
	}
}

func TestServiceHostExitPhaseBudgetClamp(t *testing.T) {
	const phase = time.Hour
	if got := serviceHostExitPhaseBudget(context.Background(), phase); got != phase {
		t.Fatal("unbounded caller changed the selected phase allowance")
	}
	callerDeadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), callerDeadline)
	defer cancel()
	before := time.Until(callerDeadline)
	got := serviceHostExitPhaseBudget(ctx, phase)
	after := time.Until(callerDeadline)
	if got > before || got < after || got <= 0 {
		t.Fatal("phase did not clamp to the original caller deadline")
	}
	if got := serviceHostExitPhaseBudget(ctx, time.Millisecond); got != time.Millisecond {
		t.Fatal("caller extended a shorter selected phase allowance")
	}
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	if serviceHostExitPhaseBudget(expired, phase) != 0 {
		t.Fatal("expired caller received a fresh phase allocation")
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if serviceHostExitPhaseBudget(canceled, phase) != 0 {
		t.Fatal("canceled caller received a fresh phase allocation")
	}
}

// This contract checks the real timer/transport/drain use sites as well as the
// policy tests. A correct but unused policy must not hide a fixed ACK timer.
func serviceHostExitBudgetContract(source []byte) bool {
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	body := func(signature string) string {
		start := strings.Index(normalized, signature)
		if start < 0 {
			return ""
		}
		tail := normalized[start:]
		if end := strings.Index(tail[1:], "\nfunc "); end >= 0 {
			return tail[:end+1]
		}
		return tail
	}
	server := body("func (c *Cell) serveHostExit(")
	for _, required := range []string{
		"budgets := serviceHostExitBudgets(c.cfg.Timeout)\n\tctx, cancel := context.WithTimeout(r.Context(), serviceHostExitPhaseBudget(r.Context(), budgets.transaction))",
		"deadline, _ := ctx.Deadline()",
		"SetWriteDeadline(deadline)",
		"context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, budgets.admission))",
		"serviceCommand{ctx: ctx, action: \"exit-host\"",
		"case <-admissionCtx.Done():",
		"context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, ServiceShutdownTimeout))",
		"SetWriteDeadline(completionDeadline)",
		"case <-completionCtx.Done():",
	} {
		if !strings.Contains(server, required) {
			return false
		}
	}
	client := body("func (c *Cell) startHostExitRequest(")
	// All four ACK-error observations must use the same deadline selector.
	if strings.Count(client, "serviceHostExitAdmissionCause(ackCtx, ctx, time.Now())") != 4 || strings.Contains(client, "ackCtx.Err()") {
		return false
	}
	if strings.Count(server, "http.Error(w, serviceHostExitAdmissionTimeout, http.StatusRequestTimeout)") != 2 {
		return false
	}
	selector := body("func serviceHostExitAdmissionCause(")
	for _, required := range []string{"admission.Err()", "caller.Err()", "admission.Deadline()", "!now.Before(deadline)", "return context.DeadlineExceeded"} {
		if !strings.Contains(selector, required) {
			return false
		}
	}
	for _, required := range []string{
		"budgets := serviceHostExitBudgets(c.cfg.Timeout)\n\trequestCtx, cancel := context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, budgets.transaction))",
		"admission := serviceHostExitPhaseBudget(requestCtx, budgets.admission)",
		"context.WithTimeout(requestCtx, admission)",
		"stopACK := context.AfterFunc(ackCtx, cancel)",
		"!stopACK()",
		"ResponseHeaderTimeout: admission",
		"http.NewRequestWithContext(requestCtx,",
		"context.WithTimeout(requestCtx, serviceHostExitPhaseBudget(requestCtx, ServiceShutdownTimeout))",
		"session.ctx = completionCtx",
		"response.StatusCode == http.StatusRequestTimeout",
		"readErr == nil && limited.N > 0 && string(data) == serviceHostExitAdmissionTimeout+\"\\n\"",
		"resultErr = context.DeadlineExceeded",
	} {
		if !strings.Contains(client, required) {
			return false
		}
	}
	command := body("func (s *supervisor) finishHostExitCommand(")
	if !strings.Contains(command, "context.WithCancel(command.ctx)") || strings.Contains(command, "WithoutCancel") {
		return false
	}
	drain := body("func (s *supervisor) finishHostExit(")
	if !strings.Contains(drain, "s.hooks.shutdownContext(ctx, serviceHostExitPhaseBudget(ctx, ServiceShutdownTimeout))") {
		return false
	}
	for _, signature := range []string{"func (c *Cell) ExitServiceHost(", "func (c *Cell) exitServiceHostForRemoval("} {
		caller := body(signature)
		if !strings.Contains(caller, "c.waitServiceHostExit(session.ctx, lease, observer)") {
			return false
		}
	}
	observation := body("func (c *Cell) waitServiceHostExit(")
	return !strings.Contains(observation, "context.WithTimeout(ctx, ServiceShutdownTimeout)") &&
		!strings.Contains(client, "30*time.Second") && !strings.Contains(client, "30 * time.Second")
}

func TestServiceHostExitProductionBudgetContract(t *testing.T) {
	source, err := os.ReadFile("service_control_exit.go")
	if err != nil {
		t.Fatal("cannot read host-exit production budget contract")
	}
	if !serviceHostExitBudgetContract(source) {
		t.Fatal("production host-exit sites do not compose admission, drain and shared observation budgets")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	if !serviceHostExitBudgetContract([]byte(strings.ReplaceAll(normalized, "\n", "\r\n"))) {
		t.Fatal("host-exit budget use-site contract does not survive CRLF")
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"unused_policy", "budgets := serviceHostExitBudgets(c.cfg.Timeout)", "budgets := serviceHostExitPolicy{}"},
		{"old_request_timeout", "context.WithTimeout(ctx, serviceHostExitPhaseBudget(ctx, budgets.transaction))", "context.WithTimeout(ctx, ServiceShutdownTimeout)"},
		{"fixed_ack_timer", "admission := serviceHostExitPhaseBudget(requestCtx, budgets.admission)", "admission := 30 * time.Second"},
		{"unbounded_ack_body", "stopACK := context.AfterFunc(ackCtx, cancel)", "stopACK := func() bool { return true }"},
		{"fixed_header_timeout", "ResponseHeaderTimeout: admission", "ResponseHeaderTimeout: 30 * time.Second"},
		{"detached_drain", "context.WithCancel(command.ctx)", "context.WithCancel(context.WithoutCancel(command.ctx))"},
		{"unshared_observation", "c.waitServiceHostExit(session.ctx, lease, observer)", "c.waitServiceHostExit(ctx, lease, observer)"},
		{"unpublished_admission_error", "serviceHostExitAdmissionCause(ackCtx, ctx, time.Now())", "ackCtx.Err()"},
		{"silent_server_timeout", "http.Error(w, serviceHostExitAdmissionTimeout, http.StatusRequestTimeout)", "return"},
		{"missing_elapsed_check", "!now.Before(deadline)", "now.Before(deadline)"},
		{"unrecognized_timeout_body", "string(data) == serviceHostExitAdmissionTimeout+\"\\n\"", "len(data) > 0"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutant := strings.Replace(normalized, mutation.before, mutation.after, 1)
			if mutant == normalized {
				t.Fatal("host-exit use-site mutation did not apply")
			}
			if serviceHostExitBudgetContract([]byte(mutant)) {
				t.Fatal("host-exit production contract accepted the mutated use site")
			}
		})
	}
}

func TestServiceHostExitFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		cause string
	}{
		{"deadline", context.DeadlineExceeded, "deadline"},
		{"canceled", context.Canceled, "canceled"},
		{"forced", ErrServiceShutdownForced, "forced"},
		{"refused", ErrServiceControlRefused, "refused"},
		{"missing", os.ErrNotExist, "missing"},
		{"privacy", os.ErrPermission, "privacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := hostExitFailure("terminal", true, tc.err)
			var diagnostic *serviceHostExitError
			if !errors.As(err, &diagnostic) || !errors.Is(err, tc.err) {
				t.Fatal("host-exit diagnostics lost original typed cause")
			}
			phase, cause, ack := diagnostic.ServiceHostExitDiagnostic()
			if phase != "terminal" || cause != tc.cause || !ack {
				t.Fatal("host-exit diagnostics changed recorded stage, cause or acceptance")
			}
			wrapped := hostExitFailure("endpoint", false, err)
			if wrapped != err {
				t.Fatal("outer host-exit stage overwrote the original recorded cause")
			}
		})
	}
	// Deliberately opaque input is never echoed as a phase or error message.
	opaque := strings.Repeat("x", 65)
	original := errors.New(opaque)
	err := hostExitFailure(opaque, false, original)
	var diagnostic *serviceHostExitError
	if !errors.As(err, &diagnostic) || !errors.Is(err, original) {
		t.Fatal("opaque error lost private wrapped identity")
	}
	phase, cause, ack := diagnostic.ServiceHostExitDiagnostic()
	if phase != "other" || cause != "other" || ack || strings.Contains(err.Error(), opaque) {
		t.Fatal("host-exit diagnostics exposed an unrecognized stage or raw error")
	}
	if hostExitFailure("terminal", true, nil) != nil {
		t.Fatal("successful exit acquired a diagnostic failure")
	}
}

// Keep Err unpublished while exposing the selected deadline. The injected now
// retains the deadline's monotonic component; no scheduling boundary is slept to.
type serviceHostExitUnpublishedDeadline struct {
	context.Context
	deadline time.Time
}

func (c serviceHostExitUnpublishedDeadline) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func TestServiceHostExitAdmissionCause(t *testing.T) {
	now := time.Now()
	live := context.Background()
	canceled, cancel := context.WithCancel(live)
	cancel()
	expired, expire := context.WithDeadline(live, now.Add(-time.Second))
	defer expire()
	for _, tc := range []struct {
		name              string
		admission, caller context.Context
		want              error
	}{
		{"elapsed_unpublished", serviceHostExitUnpublishedDeadline{live, now.Add(-time.Nanosecond)}, live, context.DeadlineExceeded},
		{"exact_deadline_unpublished", serviceHostExitUnpublishedDeadline{live, now}, live, context.DeadlineExceeded},
		{"before_deadline_refusal", serviceHostExitUnpublishedDeadline{live, now.Add(time.Nanosecond)}, live, nil},
		{"no_deadline_refusal", live, live, nil},
		{"published_admission_deadline", expired, live, context.DeadlineExceeded},
		{"published_admission_cancellation", canceled, live, context.Canceled},
		{"pre_canceled_caller", serviceHostExitUnpublishedDeadline{live, now.Add(time.Nanosecond)}, canceled, context.Canceled},
		{"caller_cancellation_before_unpublished_deadline", serviceHostExitUnpublishedDeadline{live, now.Add(-time.Nanosecond)}, canceled, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := serviceHostExitAdmissionCause(tc.admission, tc.caller, now)
			if !errors.Is(cause, tc.want) {
				t.Fatal("admission selector lost a published context error or the selected deadline")
			}
			err := hostExitFailure("admission", false, errors.Join(hostExitRefused("envelope"), cause))
			var diagnostic *serviceHostExitError
			if !errors.As(err, &diagnostic) || diagnostic.phase != "admission" || diagnostic.accepted {
				t.Fatal("admission selector lost the failure boundary")
			}
			if tc.want == nil && (diagnostic.cause != "refused" || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
				t.Fatal("before-deadline refusal was misclassified as context exhaustion")
			}
		})
	}
}
