package localcell_test

import (
	"context"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// A caller cancelled between a component launch and its readiness step must
// still receive the fixed readiness cause with the pre-rollback process
// observation; a bare context error would discard the diagnostics slice
// LC-S2-PW3 established (review LC-S2-R1R4, finding R4-1).
func TestStartCanceledBeforeReadinessRetainsObservation(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, phase := range []string{"server", "worker"} {
		t.Run(phase, func(t *testing.T) {
			installed, root := disposableCell(t, binaries, nil)
			if err := installed.Install(t.Context(), localcell.InstallOptions{}); err != nil {
				t.Fatalf("install: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reached := false
			cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 5 * time.Second,
				Failpoint: func(operation, step string) error {
					if operation == "start" && step == phase {
						reached = true
						cancel()
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal("cannot configure the cancellation seam")
			}
			err = cell.Start(ctx)
			if !reached {
				t.Fatal("cancellation did not reach the post-launch boundary")
			}
			component, cause, state, observationFailed, ok := localcell.ReadinessFailureForTest(err)
			if !ok || component != phase || cause != "local cell "+phase+" readiness deadline exceeded" ||
				state.PID <= 1 || !state.Alive || !state.Matches || observationFailed {
				t.Fatal("cancellation after component launch lost the pre-rollback readiness observation")
			}
		})
	}
}

// A caller cancelled before a component launch must not have that component
// launched at all: the early context guard still applies to every step other
// than the two readiness waits (review LC-S2-R1R5, finding R5-1).
func TestStartCanceledBeforeLaunchDoesNotStartComponent(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	for _, scenario := range []struct{ cancelAfter, forbidden string }{{"postgres", "server"}, {"account", "worker"}} {
		t.Run(scenario.forbidden, func(t *testing.T) {
			installed, root := disposableCell(t, binaries, nil)
			if err := installed.Install(t.Context(), localcell.InstallOptions{}); err != nil {
				t.Fatalf("install: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reached, launched := false, false
			cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 5 * time.Second,
				Failpoint: func(operation, step string) error {
					if operation == "start" && step == scenario.cancelAfter {
						reached = true
						cancel()
					}
					if operation == "start" && step == scenario.forbidden {
						launched = true
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal("cannot configure the canceled-launch seam")
			}
			err = cell.Start(ctx)
			if !reached || err == nil {
				t.Fatal("canceled-launch seam did not stop startup")
			}
			if launched {
				t.Fatalf("%s was launched after caller cancellation following %s", scenario.forbidden, scenario.cancelAfter)
			}
			assertStopped(t, cell)
		})
	}
}
