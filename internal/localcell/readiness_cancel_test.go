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
			installStarted := time.Now() // FLAKE-3 fix 2 (F6)
			if err := installed.Install(t.Context(), localcell.InstallOptions{}); err != nil {
				t.Fatalf("cancellation fixture install failed: %v", err)
			}
			installBaseline := time.Since(installStarted) // FLAKE-3 fix 2 (F6)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reached := false
			cfg := cancellationCellConfig(root, binaries, installBaseline) // FLAKE-3 fix 3 (F7)
			cfg.Failpoint = func(operation, step string) error {           // FLAKE-3 fix 3 (F7)
				if operation == "start" && step == phase { // FLAKE-3 fix 3 (F7)
					reached = true // FLAKE-3 fix 3 (F7)
					cancel()       // FLAKE-3 fix 3 (F7)
				} // FLAKE-3 fix 3 (F7)
				return nil // FLAKE-3 fix 3 (F7)
			} // FLAKE-3 fix 3 (F7)
			cell, err := localcell.New(cfg) // FLAKE-3 fix 3 (F7)
			if err != nil {
				t.Fatal("cannot configure the cancellation seam")
			}
			if cell.EffectiveTimeoutForTest() != cancellationFixtureTimeout(installBaseline) { // FLAKE-3 fix 3 (F7)
				t.Fatal("cancel-use timeout differs from measured install baseline") // FLAKE-3 fix 3 (F7)
			} // FLAKE-3 fix 3 (F7)
			err = cell.Start(ctx)
			if !reached {
				t.Fatalf("cancellation did not reach the post-launch boundary: %v", err)
			}
			component, cause, state, observationFailed, ok := localcell.ReadinessFailureForTest(err)
			if !ok {
				t.Fatalf("cancellation observation did not retain a readiness error: %v", err)
			}
			if component != phase {
				t.Fatal("cancellation observation retained the wrong component")
			}
			if state.PID <= 1 {
				t.Fatal("cancellation observation omitted the launched process")
			}
			if !state.Alive {
				t.Fatal("cancellation observation did not observe a live process")
			}
			if cause != "local cell "+phase+" readiness deadline exceeded" {
				t.Fatal("cancellation observation retained the wrong fixed cause")
			}
			if !state.Matches {
				t.Fatal("cancellation observation did not match the launched identity")
			}
			if observationFailed {
				t.Fatal("cancellation observation encountered an inspection failure")
			}
		})
	}
}

func cancellationCellConfig(root string, binaries localcell.Binaries, baseline time.Duration) localcell.Config { // FLAKE-3 fix 3 (F7)
	return localcell.Config{Root: root, Binaries: binaries, Timeout: cancellationFixtureTimeout(baseline)} // FLAKE-3 fix 3 (F7)
} // FLAKE-3 fix 3 (F7)
func TestReadinessCancellationCellUsesInstallBaseline(t *testing.T) { // FLAKE-3 fix 3 (F7)
	t.Setenv("WITSELF_HOME", t.TempDir()) // FLAKE-3 fix 3 (F7)
	t.Setenv("DSH_HOME", t.TempDir())     // FLAKE-3 fix 3 (F7)
	for _, test := range []struct {       // FLAKE-3 fix 3 (F7)
		name     string        // FLAKE-3 fix 3 (F7)
		baseline time.Duration // FLAKE-3 fix 3 (F7)
		want     time.Duration // FLAKE-3 fix 3 (F7)
	}{ // FLAKE-3 fix 3 (F7)
		{name: "Floor", baseline: 100 * time.Millisecond, want: 5 * time.Second},                // FLAKE-3 fix 3 (F7)
		{name: "Scaled", baseline: time.Second, want: 8 * time.Second},                          // FLAKE-3 fix 3 (F7)
		{name: "Saturated", baseline: time.Duration(1<<63 - 1), want: time.Duration(1<<63 - 1)}, // FLAKE-3 fix 3 (F7)
	} { // FLAKE-3 fix 3 (F7)
		t.Run(test.name, func(t *testing.T) { // FLAKE-3 fix 3 (F7)
			cell, err := localcell.New(cancellationCellConfig(t.TempDir(), localcell.Binaries{}, test.baseline)) // FLAKE-3 fix 3 (F7)
			if err != nil {                                                                                      // FLAKE-3 fix 3 (F7)
				t.Fatal("cancel-build configuration rejected") // FLAKE-3 fix 3 (F7)
			} // FLAKE-3 fix 3 (F7)
			if got := cell.EffectiveTimeoutForTest(); got != cancellationFixtureTimeout(test.baseline) || got != test.want { // FLAKE-3 fix 3 (F7)
				t.Fatal("cancel-bound constructed timeout differs from install baseline") // FLAKE-3 fix 3 (F7)
			} // FLAKE-3 fix 3 (F7)
		}) // FLAKE-3 fix 3 (F7)
	} // FLAKE-3 fix 3 (F7)
} // FLAKE-3 fix 3 (F7)
// FLAKE-3 fix 3 (F7)
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
				t.Fatalf("canceled-launch fixture install failed: %v", err)
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
				t.Fatalf("canceled-launch started a forbidden component: %s following %s", scenario.forbidden, scenario.cancelAfter)
			}
			assertStopped(t, cell)
		})
	}
}
