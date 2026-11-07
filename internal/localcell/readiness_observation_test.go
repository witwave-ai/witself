package localcell

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestReadinessExitObservation(t *testing.T) {
	for _, completed := range []bool{false, true} {
		for _, recovered := range []bool{false, true} {
			name := "context_expired"
			if completed {
				name = "wait_completed"
			}
			if recovered {
				name += "_recovered"
			}
			t.Run(name, func(t *testing.T) {
				c, _ := unitCell(t)
				identity, alive, err := processIdentity(os.Getpid())
				if err != nil || !alive {
					t.Fatal("cannot identify test process for readiness observation")
				}
				record := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: "server"}
				if !recovered {
					record.StartTime += "-mismatch"
				}
				c.beginChildObservation("server", 0)
				observation := c.children["server"]
				observation.pid = record.PID
				if completed {
					close(observation.done)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				err = c.childReadinessExit(ctx, "server", record)
				var exit *readinessExitError
				if !errors.As(err, &exit) || exit.waitCompleted != completed || exit.recoveredAlive != recovered {
					t.Fatal("readiness exit lost wait completion or recovered identity observation")
				}
				if exit.exitStatus != "unavailable" {
					t.Fatal("missing process state invented a completed exit status")
				}
				want := "wait_observation=context_expired recovered_alive=false"
				if completed {
					want = strings.Replace(want, "context_expired", "wait_completed", 1)
				}
				if recovered {
					want = strings.Replace(want, "recovered_alive=false", "recovered_alive=true", 1)
				}
				if exit.observationDiagnostic() != want || !strings.HasSuffix(err.Error(), "; "+want) {
					t.Fatal("readiness exit did not render the closed observation vocabulary")
				}
				component, _, state, failed, ok := ReadinessFailureForTest(err)
				if !ok || component != "server" || state.PID != record.PID || state.Alive || failed {
					t.Fatal("exit observation replaced the original dead classification")
				}
			})
		}
	}
}

func TestReadinessRecoveryRequiresOriginalIdentity(t *testing.T) {
	c, _ := unitCell(t)
	record := ProcessRecord{PID: os.Getpid(), StartTime: "recorded-start", Root: c.cfg.Root, Component: "server"}
	for _, tc := range []struct {
		name      string
		mutate    func(*ProcessRecord)
		identity  string
		alive     bool
		err       error
		want      bool
		wantCalls int
	}{
		{name: "same identity", identity: record.StartTime, alive: true, want: true, wantCalls: 1},
		{name: "dead", identity: record.StartTime, wantCalls: 1},
		{name: "unknown identity", alive: true, wantCalls: 1},
		{name: "reused identity", identity: "replacement-start", alive: true, wantCalls: 1},
		{name: "inspection failure", identity: record.StartTime, alive: true, err: errors.New("inspection failed"), wantCalls: 1},
		{name: "missing identity", mutate: func(r *ProcessRecord) { r.StartTime = "" }},
		{name: "missing process", mutate: func(r *ProcessRecord) { r.PID = 0 }},
		{name: "different root", mutate: func(r *ProcessRecord) { r.Root += "-other" }},
		{name: "different component", mutate: func(r *ProcessRecord) { r.Component = "worker" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := record
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			calls := 0
			got := c.readinessIdentityRecovered("server", candidate, func(pid int) (string, bool, error) {
				calls++
				if pid != record.PID {
					t.Fatal("recovery observed a different process")
				}
				return tc.identity, tc.alive, tc.err
			})
			if got != tc.want || calls != tc.wantCalls {
				t.Fatal("readiness recovery weakened the original identity fence or retried an observation")
			}
		})
	}
}

func TestReadinessExitWithoutOwnedObserverHasNoAttribution(t *testing.T) {
	for _, mismatched := range []bool{false, true} {
		c, _ := unitCell(t)
		record := ProcessRecord{PID: os.Getpid()}
		if mismatched {
			c.beginChildObservation("server", 0)
			c.children["server"].pid = record.PID + 1
		}
		err := c.childReadinessExit(t.Context(), "server", record)
		var exit *readinessExitError
		if errors.As(err, &exit) || strings.Contains(err.Error(), "wait_observation=") {
			t.Fatal("unowned observer invented readiness exit attribution")
		}
	}
}

func TestReadinessObservationRetainsClassifiedRecord(t *testing.T) {
	c, _ := unitCell(t)
	identity, alive, err := processIdentity(os.Getpid())
	if err != nil || !alive {
		t.Fatal("cannot identify test process for frozen readiness record")
	}
	record := ProcessRecord{PID: os.Getpid(), StartTime: identity, Root: c.cfg.Root, Component: "server"}
	if err := writeAtomic(c.path("run", "server.json"), &record, nil); err != nil {
		t.Fatal("cannot persist original readiness record")
	}
	_, classified, err := c.processStateWithRecord("server")
	if err != nil {
		t.Fatal("cannot capture original readiness record")
	}
	record.StartTime += "-replacement"
	if err := writeAtomic(c.path("run", "server.json"), &record, nil); err != nil {
		t.Fatal("cannot replace durable readiness record")
	}
	c.beginChildObservation("server", 0)
	c.children["server"].pid = classified.PID
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = c.childReadinessExit(ctx, "server", classified)
	var exit *readinessExitError
	if !errors.As(err, &exit) || !exit.recoveredAlive {
		t.Fatal("readiness recovery reread a replacement durable record")
	}
}
