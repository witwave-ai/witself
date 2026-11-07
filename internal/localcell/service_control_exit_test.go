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
