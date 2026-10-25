//go:build darwin || linux || windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestWaitReadyRetainsFailureObservation(t *testing.T) {
	for _, component := range []string{"server", "worker"} {
		for _, condition := range []string{"exited", "deadline", "identity-mismatch", "missing-record", "invalid-record"} {
			t.Run(component+"/"+condition, func(t *testing.T) {
				c, _ := unitCell(t)
				var want ProcessState
				var record ProcessRecord
				switch condition {
				case "exited":
					record = signalSafetyChild(t)
					if err := cleanupEnvironmentFixture(record, 5*time.Second, processIdentity, terminateEnvironmentFixture); err != nil {
						t.Fatal("cannot stop disposable readiness process")
					}
					want.PID = record.PID
				case "deadline", "identity-mismatch":
					identity, alive, err := processIdentity(os.Getpid())
					if err != nil || !alive {
						t.Fatal("cannot observe test process identity")
					}
					record = ProcessRecord{PID: os.Getpid(), StartTime: identity}
					want = ProcessState{PID: record.PID, Alive: true, Matches: condition == "deadline"}
					if condition == "identity-mismatch" {
						record.StartTime = "synthetic-other-identity"
					}
				}
				if condition != "missing-record" {
					record.Root, record.Component = c.cfg.Root, component
					if err := writeAtomic(c.path("run", component+".json"), record, nil); err != nil {
						t.Fatal("cannot stage readiness process record")
					}
				}
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				defer cancel()
				err := c.step("start", component+"_ready", func() error { return c.waitReady(ctx, component, 1) })
				if condition != "missing-record" {
					if err := removePrivateFile(c.path("run", component+".json")); err != nil {
						t.Fatal("cannot remove readiness process record")
					}
				}
				gotComponent, cause, state, observationFailed, ok := ReadinessFailureForTest(err)
				wantCause := "local cell " + component + " exited before readiness"
				if condition == "deadline" {
					wantCause = "local cell " + component + " readiness deadline exceeded"
				}
				if !ok || gotComponent != component || cause != wantCause || state != want || observationFailed != (condition == "invalid-record") {
					t.Fatal("readiness failure lost its fixed cause or pre-rollback observation")
				}
				if err.Error() != "local cell start: "+component+"_ready failed: "+wantCause {
					t.Fatal("step did not preserve the fixed readiness cause")
				}
			})
		}
	}
}

func TestStepPreservesOnlyTrustedReadinessErrors(t *testing.T) {
	const privateValue = "password=synthetic-secret postgres://synthetic:secret@127.0.0.1"
	for _, component := range []string{"server", "worker"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline-%t", component, deadline), func(t *testing.T) {
				c, _ := unitCell(t)
				trusted := &readinessError{component: component, deadlineExceeded: deadline, state: ProcessState{PID: 42}}
				wrapped := fmt.Errorf("%s: %w", privateValue, trusted)
				err := c.step("start", component+"_ready", func() error { return wrapped })
				if err == nil || err.Error() != "local cell start: "+component+"_ready failed: "+trusted.Error() || !errors.Is(err, trusted) {
					t.Fatal("step lost trusted readiness detail or retained a private wrapper")
				}
			})
		}
	}
	for _, injected := range []error{
		errors.New(privateValue),
		errors.New("local cell server exited before readiness"),
		fmt.Errorf("local cell server readiness deadline exceeded: %s", privateValue),
	} {
		c, _ := unitCell(t)
		err := c.step("start", "server_ready", func() error { return injected })
		if err == nil || err.Error() != "local cell start: server_ready failed" || errors.Is(err, injected) {
			t.Fatal("step exposed an untrusted error")
		}
		if _, _, _, _, ok := ReadinessFailureForTest(err); ok {
			t.Fatal("untrusted error acquired a readiness observation")
		}
	}
	if (&readinessError{component: privateValue}).Error() != "local cell readiness failed" {
		t.Fatal("readiness error exposed an unexpected component value")
	}
}
