package localcell_test

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

// The Windows-only bridge checks this portable oracle's budget against its real
// fixture. Its budget/3 floor is two seconds, above the former one-second floor.
const serviceControlChildSyntheticBudgetMirror = 6 * time.Second

func serviceControlChildReadinessAllowance(rowBudget, prep time.Duration) time.Duration {
	prep = max(prep, 0)
	scaledPrep := time.Duration(math.MaxInt64)
	if prep <= scaledPrep/10 {
		scaledPrep = 10 * prep
	}
	// One duration tick is the representational saturation for zero/tiny budgets,
	// not an absolute wall-clock allowance. Saturate multiplication before clamping.
	return max(time.Duration(1), min(max(scaledPrep, rowBudget/3), rowBudget/2))
}

func TestServiceControlChildReadinessAllowance(t *testing.T) {
	const budget = serviceControlChildSyntheticBudgetMirror
	for _, test := range []struct {
		name                  string
		rowBudget, prep, want time.Duration
	}{
		{"near_zero_prep", budget, 1, 2 * time.Second},
		{"floor_scales_with_budget", 2 * budget, 0, 2 * budget / 3},
		{"measured_prep", budget, budget / 24, 5 * budget / 12},
		{"clamp", budget, budget, budget / 2},
		{"zero_budget", 0, 0, 1},
		{"zero_budget_large_prep", 0, time.Duration(math.MaxInt64), 1},
		{"negative_budget", -budget, budget, 1},
		{"negative_prep", budget, -budget, budget / 3},
		{"minimum_prep", budget, time.Duration(math.MinInt64), budget / 3},
		{"tiny_budget", 1, 0, 1},
		{"floor_rounds_to_zero", 2, 0, 1},
		{"fractional_clamp", 17, 1, 8},
		{"maximum_prep", budget, time.Duration(math.MaxInt64), budget / 2},
		{"maximum_budget_floor", time.Duration(math.MaxInt64), 0, time.Duration(math.MaxInt64) / 3},
		{"maximum_budget_and_prep", time.Duration(math.MaxInt64), time.Duration(math.MaxInt64), time.Duration(math.MaxInt64) / 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "near_zero_prep" && serviceControlChildReadinessAllowance(test.rowBudget, test.prep) == time.Second {
				t.Fatal("real-budget readiness floor must not remain one second")
			}
			if got := serviceControlChildReadinessAllowance(test.rowBudget, test.prep); got != test.want {
				t.Fatalf("readiness allowance=%d want=%d", got, test.want)
			}
		})
	}
}

func TestServiceControlChildReadinessAllowanceBudgetScaling(t *testing.T) {
	// The real-budget floor must differ from the old bound. Neighboring budgets
	// prove it remains relative, while the cap reserves room for row commands.
	const budget = serviceControlChildSyntheticBudgetMirror
	for _, test := range []struct {
		name                       string
		rowBudget, prep, old, want time.Duration
	}{
		{"smaller_budget_floor", budget / 2, 0, time.Second, budget / 6},
		{"real_budget_floor_increased", budget, 0, time.Second, 2 * time.Second},
		{"larger_budget_floor", 2 * budget, 0, time.Second, 2 * budget / 3},
		{"real_budget_measured_prep", budget, budget / 24, 5 * budget / 12, 5 * budget / 12},
		{"real_budget_clamp", budget, budget / 10, budget, budget / 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if old := max(time.Second, 10*test.prep); old != test.old {
				t.Fatal("old readiness allowance comparison changed")
			}
			if got := serviceControlChildReadinessAllowance(test.rowBudget, test.prep); got != test.want {
				t.Fatalf("relative readiness allowance=%d want=%d", got, test.want)
			}
		})
	}
}

func TestServiceControlChildReadinessSourceContract(t *testing.T) {
	source, err := os.ReadFile("service_control_child_windows_test.go")
	if err != nil {
		t.Fatal("cannot read Windows child readiness source")
	}
	// Both the contract and its multiline mutation sites must survive Windows checkouts.
	original := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, ending := range []struct{ name, newline string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
		t.Run(ending.name, func(t *testing.T) {
			if problem := serviceControlChildReadinessSourceContract(strings.ReplaceAll(original, "\n", ending.newline)); problem != "" {
				t.Fatal(problem)
			}
		})
	}
	for _, mutation := range []struct{ name, from, to, problem string }{
		{"shared_row_parent", "ctx, cancel := context.WithTimeout(t.Context(), localcell.NativeServiceChildSyntheticBudget)", "ctx, cancel := context.WithTimeout(testContext, localcell.NativeServiceChildSyntheticBudget)", "each row must create its own budget and work deadline inside the row closure"},
		{"second_row_reuses_outer", "prepared := time.Now()", "prepared := time.Now()\nif row.name == \"full\" { ctx = testContext }", "row contexts must not be reassigned or shadowed"},
		{"shared_command_parent", "context.WithTimeout(workContext, commandAllowance)", "context.WithTimeout(testContext, commandAllowance)", "commands must use the row work context"},
		{"outer_bound", "time.Duration(len(rows))*localcell.NativeServiceChildSyntheticBudget", "localcell.NativeServiceChildSyntheticBudget", "whole-test budget must cover every sequential row"},
		{"old_readiness_site", "serviceControlChildReadinessAllowance(localcell.NativeServiceChildSyntheticBudget, time.Since(prepared))", "max(time.Second, 10*time.Since(prepared))", "readiness must use the portable row-budget allowance"},
		{"bare_fatal", `t.Fatalf("synthetic host did not reach authenticated readiness: row=%s elapsed_ms=%d allowance_ms=%d work_remaining_ms=%d last_control=%s", row.name, time.Since(readinessStarted).Milliseconds(), allowance.Milliseconds(), max(time.Until(workDeadline).Milliseconds(), 0), lastControl)`, `t.Fatal("synthetic host did not reach authenticated readiness")`, "readiness failure must log the host and bounded timing/class fields"},
		{"raw_error", "switch serviceControlChildDiagnostic(controlErr)", "switch controlErr.Error()", "readiness must classify the last control result"},
		{"startup_seed", "allowance = max(allowance, 10*time.Since(readinessStarted))", "allowance = allowance", "commands must be seeded from this row's measured readiness"},
		{"mirror_check", "serviceControlChildSyntheticBudgetMirror != localcell.NativeServiceChildSyntheticBudget", "false", "Windows must check the portable budget mirror"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutant := strings.Replace(original, mutation.from, mutation.to, 1)
			if mutant == original {
				t.Fatal("readiness contract mutation did not apply")
			}
			for _, ending := range []struct{ name, newline string }{{"LF", "\n"}, {"CRLF", "\r\n"}} {
				t.Run(ending.name, func(t *testing.T) {
					if problem := serviceControlChildReadinessSourceContract(strings.ReplaceAll(mutant, "\n", ending.newline)); problem != mutation.problem {
						t.Fatalf("readiness contract mutation raised %q want %q", problem, mutation.problem)
					}
				})
			}
		})
	}
}

func serviceControlChildReadinessSourceContract(source string) string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	positions := token.NewFileSet()
	// Parse without comments so commented-out code cannot satisfy the contract.
	file, err := parser.ParseFile(positions, "child.go", source, 0)
	if err != nil {
		return "cannot parse Windows child readiness source"
	}
	var formatted bytes.Buffer
	if err := format.Node(&formatted, positions, file); err != nil {
		return "cannot format Windows child readiness source"
	}
	var lines []string
	for _, line := range strings.Split(formatted.String(), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	normalized := strings.Join(lines, "\n")
	for _, requirement := range []struct{ fragment, problem string }{
		{`testContext, testCancel := context.WithTimeout(t.Context(), time.Duration(len(rows))*localcell.NativeServiceChildSyntheticBudget)
defer testCancel()
for _, row := range rows {`, "whole-test budget must cover every sequential row"},
		{`for _, row := range rows {
t.Run(row.name, func(t *testing.T) {
ctx, cancel := context.WithTimeout(t.Context(), localcell.NativeServiceChildSyntheticBudget)
defer cancel()
stopOuter := context.AfterFunc(testContext, cancel)
defer stopOuter()
deadline, _ := ctx.Deadline()
workContext, workCancel := context.WithDeadline(ctx, deadline.Add(-localcell.NativeServiceChildSyntheticBudget/20))
defer workCancel()`, "each row must create its own budget and work deadline inside the row closure"},
		{"hostContext, stopHost := context.WithCancel(workContext)", "host must use the row work context"},
		{`defer func() {
stopHost()
if !finished {
select {
case <-done:
case <-ctx.Done():`, "host cleanup must wait within the live row context before deferred cancellation"},
		{"commandContext, commandCancel := context.WithTimeout(workContext, commandAllowance)", "commands must use the row work context"},
		{"observeContext, observeCancel := context.WithTimeout(workContext, allowance)", "post-stop observation must use the row work context"},
		{"case <-ctx.Done():\nt.Fatal(\"synthetic foreground host shutdown exceeded shared budget\")", "shutdown must use the row context"},
		{"allowance := serviceControlChildReadinessAllowance(localcell.NativeServiceChildSyntheticBudget, time.Since(prepared))", "readiness must use the portable row-budget allowance"},
		{"readinessStarted := time.Now()\ngo func() { done <- cell.Run(hostContext, policy) }()\nreadyContext, readyCancel := context.WithTimeout(workContext, allowance)", "readiness timing must start before the host and bound its control context"},
		{"for {\nstatus, controlErr := cell.Control(readyContext, \"health\")\nvar lastControl string\nif controlErr != nil {\nswitch serviceControlChildDiagnostic(controlErr) {", "readiness must classify the last control result"},
		{`case "localcell: service_control_unavailable: transport:dial":
lastControl = "dial"
case "localcell: service_control_unavailable: transport:deadline":
lastControl = "deadline"
case "localcell: service_control_unavailable: transport:eof":
lastControl = "eof"
case "localcell: service_control_unavailable: transport:response":
lastControl = "response"
default:
lastControl = "unclassified"
}
} else {
if status.Healthy {
break
}
lastControl = "unhealthy"
}
select {`, "readiness control classes must stay in the closed vocabulary"},
		{`case <-readyContext.Done():
logHost()
workDeadline, _ := workContext.Deadline()
t.Fatalf("synthetic host did not reach authenticated readiness: row=%s elapsed_ms=%d allowance_ms=%d work_remaining_ms=%d last_control=%s", row.name, time.Since(readinessStarted).Milliseconds(), allowance.Milliseconds(), max(time.Until(workDeadline).Milliseconds(), 0), lastControl)`, "readiness failure must log the host and bounded timing/class fields"},
		{"allowance = max(allowance, 10*time.Since(readinessStarted))\nfor _, action := range []string{\"health\", \"status\", \"observe\", \"stop\"} {", "commands must be seeded from this row's measured readiness"},
		{"func TestServiceControlChildSyntheticBudget(t *testing.T) {\nif serviceControlChildSyntheticBudgetMirror != localcell.NativeServiceChildSyntheticBudget {", "Windows must check the portable budget mirror"},
	} {
		if !strings.Contains(normalized, requirement.fragment) {
			return requirement.problem
		}
	}
	// A correct declaration alone must not hide a later full-row substitution of
	// the outer context, or a shadow that diverts any wait from the row deadline.
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "TestServiceControlChildProcessClient" {
			continue
		}
		writes := map[string]int{}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, ok := node.(*ast.AssignStmt); ok {
				for _, target := range assignment.Lhs {
					if name, ok := target.(*ast.Ident); ok {
						writes[name.Name]++
					}
				}
			}
			return true
		})
		if writes["ctx"] != 1 || writes["workContext"] != 1 || writes["testContext"] != 1 {
			return "row contexts must not be reassigned or shadowed"
		}
	}
	return ""
}
