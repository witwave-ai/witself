package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// The Windows foreground host's readiness wait, sized from timings the same
// test measured on its own runner. The calibrated allowance, max(5s, 10 times
// the cold spawn), stays the floor, but a cold spawn exits before it opens the
// installation. Before its first authenticated status the host walks about ten
// times as many private-tree entries as the in-process service preparation and
// flushes about three times as many records; the preparation scale is twice
// the larger ratio. A restart repeats the first start's work on the same runner
// and replaces its lease and boot records. Every input is an in-test interval
// inside the 24s share, far from overflow.
const (
	foregroundPrepareScale    = 20
	foregroundFirstStartScale = 3
	// Each status probe's bound. A wait ends at least one probe before the
	// command deadline, so its last probe completes before the host is stopped.
	foregroundStatusProbe = 100 * time.Millisecond
)

// foregroundReadinessAllowance returns one readiness wait and whether the time
// left before the command deadline, not the fixture's own timings, set it.
func foregroundReadinessAllowance(calibrated, prepare, firstStart, remaining time.Duration) (time.Duration, bool) {
	derived := max(calibrated, foregroundPrepareScale*prepare, foregroundFirstStartScale*firstStart)
	if limit := remaining - foregroundStatusProbe; derived > limit {
		return max(limit, 0), true
	}
	return derived, false
}

type foregroundReadinessRow struct {
	name                                       string
	calibrated, prepare, firstStart, remaining time.Duration
	want                                       time.Duration
	capped                                     bool
}

// The ten-times rows multiply every measurement of the fast rows by ten. Their
// cold spawn (50ms, then 500ms) keeps the calibrated term at the 5s floor, which
// is all the old rule could give.
var foregroundReadinessRows = []foregroundReadinessRow{
	{"fast_runner_first_start", 5 * time.Second, 40 * time.Millisecond, 0, 22 * time.Second, 5 * time.Second, false},
	{"fast_runner_restart", 5 * time.Second, 40 * time.Millisecond, 700 * time.Millisecond, 17 * time.Second, 5 * time.Second, false},
	{"ten_times_slower_first_start", 5 * time.Second, 400 * time.Millisecond, 0, 22 * time.Second, 20 * (400 * time.Millisecond), false},
	{"ten_times_slower_restart", 5 * time.Second, 400 * time.Millisecond, 7 * time.Second, 22 * time.Second, 3 * (7 * time.Second), false},
	{"first_start_slower_than_cold_spawn_suggests", 5 * time.Second, 40 * time.Millisecond, 3 * time.Second, 17 * time.Second, 3 * (3 * time.Second), false},
	{"calibrated_term_above_measurements", 8 * time.Second, 40 * time.Millisecond, 2 * time.Second, 17 * time.Second, 8 * time.Second, false},
	{"zero_measurements", 5 * time.Second, 0, 0, 22 * time.Second, 5 * time.Second, false},
	{"negative_measurements", 5 * time.Second, -time.Second, -2 * time.Second, 22 * time.Second, 5 * time.Second, false},
	// The ten-times restart again, with only 9s left before the command deadline.
	{"exceeds_remaining_budget", 5 * time.Second, 400 * time.Millisecond, 7 * time.Second, 9 * time.Second, 9*time.Second - 100*time.Millisecond, true},
	{"derived_exactly_fits", 5 * time.Second, 0, 0, 5*time.Second + 100*time.Millisecond, 5 * time.Second, false},
	{"budget_already_spent", 5 * time.Second, 40 * time.Millisecond, 700 * time.Millisecond, 50 * time.Millisecond, 0, true},
}

func TestForegroundReadinessAllowance(t *testing.T) {
	for _, row := range foregroundReadinessRows {
		t.Run(row.name, func(t *testing.T) {
			got, capped := foregroundReadinessAllowance(row.calibrated, row.prepare, row.firstStart, row.remaining)
			if got != row.want || capped != row.capped {
				t.Fatal("foreground readiness allowance left its rule")
			}
			// Apart from want: never negative, never past the last probe before the deadline.
			if got < 0 || got > max(row.remaining-foregroundStatusProbe, 0) {
				t.Fatal("foreground readiness allowance left the remaining budget")
			}
		})
	}
}

// One mutation per rule, each standing for an edit of foregroundReadinessAllowance
// through its inputs, and each killed by the named row.
func TestForegroundReadinessAllowanceMutations(t *testing.T) {
	for _, mutation := range []struct {
		name, killer string
		allowance    func(calibrated, prepare, firstStart, remaining time.Duration) (time.Duration, bool)
	}{
		// The calibrated term, and with it the 5s floor, dropped from the maximum.
		{"floor_removed", "fast_runner_first_start", func(_, p, f, r time.Duration) (time.Duration, bool) {
			return foregroundReadinessAllowance(0, p, f, r)
		}},
		{"preparation_multiplier_dropped", "ten_times_slower_first_start", func(c, p, f, r time.Duration) (time.Duration, bool) {
			return foregroundReadinessAllowance(c, p/foregroundPrepareScale, f, r)
		}},
		{"first_start_multiplier_dropped", "first_start_slower_than_cold_spawn_suggests", func(c, p, f, r time.Duration) (time.Duration, bool) {
			return foregroundReadinessAllowance(c, p, f/foregroundFirstStartScale, r)
		}},
		{"budget_cap_removed", "exceeds_remaining_budget", func(c, p, f, _ time.Duration) (time.Duration, bool) {
			return foregroundReadinessAllowance(c, p, f, math.MaxInt64)
		}},
		// The cap at the command deadline itself, leaving no room for a last probe.
		{"cap_not_strict", "exceeds_remaining_budget", func(c, p, f, r time.Duration) (time.Duration, bool) {
			return foregroundReadinessAllowance(c, p, f, r+foregroundStatusProbe)
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			killed := false
			for _, row := range foregroundReadinessRows {
				if row.name == mutation.killer {
					got, capped := mutation.allowance(row.calibrated, row.prepare, row.firstStart, row.remaining)
					killed = got != row.want || capped != row.capped
				}
			}
			if !killed {
				t.Fatal("foreground readiness mutation survived its named row")
			}
		})
	}
}

// The Windows test cannot run here; pin how it feeds the rule instead.
func TestWindowsForegroundReadinessWiring(t *testing.T) {
	source, err := os.ReadFile("service_subprocess_windows_test.go")
	if err != nil {
		t.Fatal("cannot read Windows foreground readiness source")
	}
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, mutation := range []struct{ name, from, to, problem string }{
		{"lf", "", "", ""},
		{"crlf", "\n", "\r\n", ""},
		{"prepare_unmeasured", "prepare := time.Since(prepared)", "prepare := time.Duration(0)", "preparation must be measured around PrepareService"},
		{"restart_ignores_first_start", "allowance, prepare, firstStart,", "allowance, prepare, 0,", "each wait must derive from the calibrated allowance, preparation, first start and command deadline"},
		{"budget_from_parent", "time.Until(commandDeadline)", "time.Until(deadline)", "each wait must derive from the calibrated allowance, preparation, first start and command deadline"},
		{"timer_uses_calibrated", "time.NewTimer(readiness)", "time.NewTimer(allowance)", "each wait must derive from the calibrated allowance, preparation, first start and command deadline"},
		{"first_start_reset", "readiness, capped :=", "firstStart = 0\nreadiness, capped :=", "each readiness input must be written once and the rule called once"},
		{"probe_unreserved", "context.WithTimeout(ctx, foregroundStatusProbe)", "context.WithTimeout(ctx, 200*time.Millisecond)", "each status probe must use the bound the cap reserves"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutant := original
			if mutation.from != "" {
				mutant = strings.ReplaceAll(original, mutation.from, mutation.to)
				if mutant == original {
					t.Fatal("readiness wiring mutation no longer applies")
				}
			}
			if foregroundReadinessWiring(mutant) != mutation.problem {
				t.Fatal("readiness wiring contract raised the wrong problem")
			}
		})
	}
}

func foregroundReadinessWiring(source string) string {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "windows-harness", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	var formatted bytes.Buffer
	if err != nil || format.Node(&formatted, positions, file) != nil {
		return "cannot parse Windows foreground readiness source"
	}
	var lines []string
	for _, line := range strings.Split(formatted.String(), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	normalized := strings.Join(lines, "\n")
	for _, requirement := range []struct{ fragment, problem string }{
		{"prepared := time.Now()\nif cell.PrepareService(ctx, descriptor) != nil {\nt.Logf(\"foreground_exit class=%s\", \"fixture_prepare\")\nt.Fatal(\"cannot prepare synthetic foreground service\")\n}\nprepare := time.Since(prepared)", "preparation must be measured around PrepareService"},
		{"var firstStart time.Duration\ncommandDeadline, _ := commandCtx.Deadline()\nfor iteration := 0; iteration < 2; iteration++ {", "each wait must derive from the calibrated allowance, preparation, first start and command deadline"},
		{"readiness, capped := foregroundReadinessAllowance(allowance, prepare, firstStart, time.Until(commandDeadline))\nwaitStart := time.Now()\ndeadline := time.NewTimer(readiness)", "each wait must derive from the calibrated allowance, preparation, first start and command deadline"},
		{"for !ready {\nprobeCtx, probeCancel := context.WithTimeout(ctx, foregroundStatusProbe)", "each status probe must use the bound the cap reserves"},
		{"case <-ticker.C:\n}\n}\nif iteration == 0 {\nfirstStart = time.Since(waitStart)\n}\ndeadline.Stop()", "the first start must be recorded once, after its own wait"},
	} {
		if strings.Count(normalized, requirement.fragment) != 1 {
			return requirement.problem
		}
	}
	writes := map[string]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		if assignment, ok := node.(*ast.AssignStmt); ok {
			for _, target := range assignment.Lhs {
				if name, ok := target.(*ast.Ident); ok {
					writes[name.Name]++
				}
			}
		}
		return true
	})
	if writes["firstStart"] != 1 || writes["prepare"] != 1 || writes["commandDeadline"] != 1 || strings.Count(normalized, "foregroundReadinessAllowance(") != 1 {
		return "each readiness input must be written once and the rule called once"
	}
	return ""
}
