package localcell_test

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

const (
	lostPortTimingSamples = 3
	lostPortTimingFloor   = 250 * time.Millisecond
)

type lostPortBudget struct {
	unit, cellTimeout, migrationWait, readiness, firstAttemptDelay, retryDelay, tolerance time.Duration
}

func lostPortBudgetFor(samples []time.Duration) lostPortBudget {
	unit := max(slices.Max(samples), lostPortTimingFloor)
	return lostPortBudget{unit: unit, cellTimeout: 7 * unit, migrationWait: 14 * unit, readiness: 21 * unit,
		firstAttemptDelay: 12 * unit, retryDelay: 18 * unit, tolerance: 3 * unit}
}

func TestLostPortReadinessBudgetArithmetic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		samples []time.Duration
		unit    time.Duration
	}{
		{"zero", []time.Duration{0}, 250 * time.Millisecond},
		{"below-floor", []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}, 250 * time.Millisecond},
		{"at-floor", []time.Duration{250 * time.Millisecond}, 250 * time.Millisecond},
		{"largest-in-middle", []time.Duration{100 * time.Millisecond, 400 * time.Millisecond, 200 * time.Millisecond}, 400 * time.Millisecond},
		{"largest-first", []time.Duration{2 * time.Second, time.Second, time.Second}, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := lostPortBudgetFor(tc.samples)
			if budget.unit != tc.unit || budget.unit < lostPortTimingFloor {
				t.Errorf("lost-port timing unit differs from the expected floored maximum: got %s want %s", budget.unit, tc.unit)
			}
			for _, sample := range tc.samples {
				if budget.unit < sample {
					t.Error("lost-port timing unit is smaller than a calibration sample")
				}
			}
			if budget.cellTimeout+budget.migrationWait != budget.readiness {
				t.Error("lost-port readiness budget differs from the production timeout sum")
			}
			if budget.cellTimeout < 7*budget.unit {
				t.Error("lost-port rollback budget has fewer than seven timing units")
			}
			if budget.readiness-budget.firstAttemptDelay < 2*(budget.readiness/10) {
				t.Error("lost-port first attempt leaves insufficient relaunch reserve")
			}
			if budget.firstAttemptDelay+budget.retryDelay-budget.readiness < 2*budget.tolerance {
				t.Error("lost-port retry can become ready inside the tolerated window")
			}
		})
	}
}

type lostPortReadinessEntry struct {
	Operation string    `json:"operation"`
	Step      string    `json:"step"`
	State     string    `json:"state"`
	Cause     string    `json:"cause"`
	At        time.Time `json:"at"`
}

// Select by decoded fields and append order, ignoring the cause-bearing relaunch
// start. Journal timestamps use wall time, not the process's monotonic clock.
// Leave the sign check to the caller so it has its own failure classification.
func lostPortReadinessWindow(journal string) (time.Duration, error) {
	decoder := json.NewDecoder(strings.NewReader(journal))
	starts, failures, startIndex, failureIndex := 0, 0, -1, -1
	var started, failed time.Time
	for index := 0; ; index++ {
		var entry lostPortReadinessEntry
		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, errors.New("cannot decode lost-port readiness journal")
		}
		if entry.Operation != "start" || entry.Step != "server_ready" {
			continue
		}
		if entry.State == "started" && entry.Cause == "" {
			starts++
			startIndex, started = index, entry.At
		}
		if entry.State == "failed" {
			failures++
			failureIndex, failed = index, entry.At
		}
	}
	if starts != 1 || failures != 1 || failureIndex <= startIndex {
		return 0, errors.New("lost-port readiness journal has no unique ordered window")
	}
	return failed.Sub(started), nil
}

func TestLostPortReadinessBudgetJournal(t *testing.T) {
	started := lostPortReadinessEntry{Operation: "start", Step: "server_ready", State: "started", At: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)}
	relaunched := started
	relaunched.Cause = "port_conflict_relaunch=server"
	relaunched.At = started.At.Add(time.Second)
	failed := started
	failed.State, failed.At = "failed", started.At.Add(2*time.Second)
	failedWithCause := failed
	failedWithCause.Cause = "readiness_timeout"
	other := started
	other.Step = "worker_ready"
	otherOperation := started
	otherOperation.Operation = "stop"
	equal := failed
	equal.At = started.At
	earlier := failed
	earlier.At = started.At.Add(-time.Second)
	for _, tc := range []struct {
		name    string
		entries []lostPortReadinessEntry
		ordered bool
		window  time.Duration
	}{
		{"one-window", []lostPortReadinessEntry{started, failed}, true, 2 * time.Second},
		{"relaunch-and-unrelated", []lostPortReadinessEntry{other, otherOperation, started, relaunched, failed}, true, 2 * time.Second},
		{"failed-with-cause", []lostPortReadinessEntry{started, relaunched, failedWithCause}, true, 2 * time.Second},
		{"empty", nil, false, 0},
		{"missing-start", []lostPortReadinessEntry{relaunched, failed}, false, 0},
		{"missing-failure", []lostPortReadinessEntry{started, relaunched}, false, 0},
		{"duplicate-start", []lostPortReadinessEntry{started, started, failed}, false, 0},
		{"duplicate-failure", []lostPortReadinessEntry{started, failed, failedWithCause}, false, 0},
		{"failure-before-start", []lostPortReadinessEntry{failed, started}, false, 0},
		{"equal-times", []lostPortReadinessEntry{started, equal}, true, 0},
		{"backwards-clock", []lostPortReadinessEntry{started, earlier}, true, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var journal strings.Builder
			encoder := json.NewEncoder(&journal)
			for _, entry := range tc.entries {
				if err := encoder.Encode(entry); err != nil {
					t.Fatal("cannot encode lost-port budget journal fixture")
				}
			}
			window, err := lostPortReadinessWindow(journal.String())
			if (err == nil) != tc.ordered || window != tc.window {
				t.Fatalf("lost-port journal window differs from expected append order and duration: window %s error %v", window, err)
			}
			if valid := err == nil && window > 0; valid != (tc.ordered && tc.window > 0) {
				t.Fatal("lost-port journal accepted a window without a positive duration")
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		if _, err := lostPortReadinessWindow("{"); err == nil {
			t.Fatal("lost-port journal accepted malformed input")
		}
	})
}

func assertLostPortBudgetStopped(t *testing.T, cell *localcell.Cell) {
	t.Helper()
	inspection, err := cell.Inspect(lifecycleProofContext(t, nil))
	if err != nil {
		t.Logf("lost-port budget inspection error: %v", err)
		t.Fatal("cannot inspect the lost-port budget cell after rollback")
	}
	for component, process := range inspection.Processes {
		if process.Alive {
			t.Logf("lost-port budget component still alive: %s", component)
			t.Fatal("lost-port budget rollback left a component alive")
		}
	}
}

func TestLostPortReadinessBudgetSourceContract(t *testing.T) {
	functionBody := func(path, name string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("lost-port budget source unavailable")
		}
		// Windows checkouts can use CRLF; compare normalized function bodies.
		source := strings.ReplaceAll(string(data), "\r\n", "\n")
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, path, source, 0)
		if err != nil {
			t.Fatal("lost-port budget source cannot be parsed")
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name && function.Body != nil {
				return source[positions.Position(function.Body.Pos()).Offset:positions.Position(function.Body.End()).Offset]
			}
		}
		t.Fatal("lost-port budget function unavailable")
		return ""
	}
	body := functionBody("lifecycle_lost_port_test.go", "testLostPortReadinessBudget")
	for _, required := range []string{"lostPortBudgetFor(", "Timeout: budget.cellTimeout", "MigrationWait: budget.migrationWait"} {
		if !strings.Contains(body, required) {
			t.Errorf("lost-port readiness test lost its shared budget configuration: %s", required)
		}
	}
	for _, forbidden := range []string{"singleStart", "time.Since(readinessStarted) >", "assertStopped("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("lost-port readiness test retained an obsolete budget assertion: %s", forbidden)
		}
	}
	arithmetic := functionBody("lifecycle_lost_port_budget_test.go", "lostPortBudgetFor")
	if !strings.Contains(arithmetic, "slices.Max(samples)") || !strings.Contains(arithmetic, "lostPortTimingFloor") {
		t.Fatal("lost-port budget lost its largest sample or timing floor")
	}
}
