package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

// The Windows foreground host's readiness wait ends on authenticated status
// inside the command budget; no earlier sample sizes it. A cold spawn exits
// before it opens the installation, and a neighbour's load can stretch a later
// start past any multiple of one. A wait that reaches its deadline first names
// its last probe: no endpoint record, a refusal, a state other than stopped,
// or no answer at all.
var foregroundProbeOutcomes = []string{"probe_endpoint", "probe_refused", "probe_state", "probe_unanswered"}

func foregroundProbeOutcome(probeErr, endpointErr error) string {
	switch {
	case errors.Is(endpointErr, os.ErrNotExist):
		return "probe_endpoint"
	case probeErr == nil:
		return "probe_state"
	case errors.Is(probeErr, localcell.ErrServiceControlRefused):
		return "probe_refused"
	default:
		return "probe_unanswered"
	}
}

func TestForegroundProbeOutcome(t *testing.T) {
	absent := &os.PathError{Op: "lstat", Path: "endpoint", Err: os.ErrNotExist}
	unanswered := errors.New("localcell: service_control_unavailable: transport:deadline")
	for _, row := range []struct {
		name                  string
		probeErr, endpointErr error
		want                  string
	}{
		{"endpoint_absent_refused", localcell.ErrServiceControlRefused, absent, "probe_endpoint"},
		{"endpoint_absent_unanswered", unanswered, absent, "probe_endpoint"},
		{"refused", localcell.ErrServiceControlRefused, nil, "probe_refused"},
		{"refused_wrapped", fmt.Errorf("endpoint record: %w", localcell.ErrServiceControlRefused), nil, "probe_refused"},
		{"state", nil, nil, "probe_state"},
		{"unanswered", unanswered, nil, "probe_unanswered"},
		{"endpoint_unreadable", unanswered, errors.New("synthetic lstat refusal"), "probe_unanswered"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := foregroundProbeOutcome(row.probeErr, row.endpointErr)
			if got != row.want || !slices.Contains(foregroundProbeOutcomes, got) {
				t.Fatal("foreground probe outcome left its closed classes")
			}
		})
	}
}

// The Windows test cannot run here; pin how it waits instead.
func TestWindowsForegroundReadinessWiring(t *testing.T) {
	source, err := os.ReadFile("service_subprocess_windows_test.go")
	if err != nil {
		t.Fatal("cannot read Windows foreground readiness source")
	}
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, mutation := range []struct{ name, from, to, problem string }{
		{"lf", "", "", ""},
		{"crlf", "\n", "\r\n", ""},
		{"deadline_from_parent", "commandDeadline, _ := commandCtx.Deadline()", "commandDeadline, _ := ctx.Deadline()", "each wait must end one WaitDelay share before the command deadline"},
		{"wait_past_command_deadline", "commandDeadline.Add(-windowsSyntheticTestBudget / 90)", "commandDeadline.Add(windowsSyntheticTestBudget / 90)", "each wait must end one WaitDelay share before the command deadline"},
		{"wait_from_cold_spawn", "time.NewTimer(time.Until(waitDeadline))", "time.NewTimer(allowance)", "each wait must end one WaitDelay share before the command deadline"},
		{"wait_deadline_moved", "\t\t\tprobeCancel()\n", "\t\t\tprobeCancel()\n\t\t\twaitDeadline = waitDeadline.Add(allowance)\n", "the wait deadline must be written once"},
		{"probe_fixed_bound", "context.WithDeadline(ctx, waitDeadline)", "context.WithTimeout(ctx, 100*time.Millisecond)", "each status probe may use the whole wait"},
		{"kill_not_deferred", "defer func() {\n\t\t\tif !finished {", "go func() {\n\t\t\tif !finished {", "the host kill must be deferred so it runs before t.Context is canceled"},
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
		{"var oldStart, oldBoot string\ncommandDeadline, _ := commandCtx.Deadline()\nfor iteration := 0; iteration < 2; iteration++ {", "each wait must end one WaitDelay share before the command deadline"},
		{"finished := false\ndefer func() {\nif !finished {\n_ = command.Process.Kill()", "the host kill must be deferred so it runs before t.Context is canceled"},
		{"waitDeadline := commandDeadline.Add(-windowsSyntheticTestBudget / 90)\ndeadline := time.NewTimer(time.Until(waitDeadline))\nready := false", "each wait must end one WaitDelay share before the command deadline"},
		{"for !ready {\nprobeCtx, probeCancel := context.WithDeadline(ctx, waitDeadline)\nstatus, probeErr := cell.Control(probeCtx, \"status\")", "each status probe may use the whole wait"},
		{"case <-time.After(20 * time.Millisecond):\n}\n}\ndeadline.Stop()", "each pause must start after its probe and the wait must stop its deadline"},
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
	if writes["waitDeadline"] != 1 || writes["commandDeadline"] != 1 {
		return "the wait deadline must be written once"
	}
	return ""
}
