package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestClassifyServiceInstallFailure(t *testing.T) {
	exitErr := &exec.ExitError{}
	otherErr := errors.New("synthetic process failure")
	for _, test := range []struct {
		name                        string
		parentErr, spawnErr, runErr error
		hasCause                    bool
		want                        string
	}{
		{"parent_budget", context.DeadlineExceeded, nil, exitErr, false, "parent_budget"},
		{"deadline", nil, context.DeadlineExceeded, nil, false, "deadline"},
		{"exit_zero", nil, nil, nil, false, "exit_zero"},
		{"exit_without_cause", nil, nil, exitErr, false, "exit_without_cause"},
		{"start_failed", nil, nil, otherErr, false, "start_failed"},
		{"parent_before_spawn", context.DeadlineExceeded, context.DeadlineExceeded, exitErr, false, "parent_budget"},
		{"spawn_before_exit", nil, context.DeadlineExceeded, exitErr, false, "deadline"},
		{"parent_before_clean_exit", context.DeadlineExceeded, nil, nil, true, "parent_budget"},
		{"spawn_before_cause", nil, context.DeadlineExceeded, exitErr, true, "deadline"},
		{"parent_before_cause", context.DeadlineExceeded, nil, exitErr, true, "parent_budget"},
		{"parent_cancelled", context.Canceled, context.DeadlineExceeded, otherErr, false, "parent_budget"},
		{"spawn_cancelled", nil, context.Canceled, otherErr, false, "deadline"},
		{"zero_with_cause", nil, nil, nil, true, "exit_zero"},
		{"wrapped_exit", nil, nil, &os.PathError{Op: "wait", Path: "fixture", Err: exitErr}, false, "exit_without_cause"},
		{"wait_delay", nil, nil, exec.ErrWaitDelay, false, "start_failed"},
		{"expected_refusal", nil, nil, exitErr, true, ""},
		// The caller's existing predicate accepts any non-nil error with the
		// expected cause, including a copier error after the host wrote it.
		{"cause_with_other_error", nil, nil, otherErr, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := classifyServiceInstallFailure(test.parentErr, test.spawnErr, test.runErr, test.hasCause)
			switch got {
			case "", "deadline", "parent_budget", "exit_zero", "exit_without_cause", "start_failed":
			default:
				t.Fatal("service install classifier returned a word outside its closed vocabulary")
			}
			if got != test.want {
				t.Fatal("service install classifier violated failure class or budget precedence")
			}
		})
	}
}

func TestWindowsServiceInstallFailureContract(t *testing.T) {
	source, err := os.ReadFile("service_subprocess_windows_test.go")
	if err != nil {
		t.Fatal("cannot read Windows service install source")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, test := range []struct{ name, source string }{
		{"lf", lf},
		{"crlf", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertWindowsServiceInstallFailureContract(t, test.source)
		})
	}
}

func assertWindowsServiceInstallFailureContract(t *testing.T, source string) {
	t.Helper()
	// Normalize before matching multiline source so a Windows checkout has
	// exactly the same contract as the host and the explicit CRLF replay.
	source = strings.ReplaceAll(source, "\r\n", "\n")
	const fatal = `t.Fatal("Windows service install did not preserve adapter refusal")`
	if strings.Count(source, fatal) != 1 || strings.Count(source, "Windows service install did not preserve adapter refusal") != 1 {
		t.Fatal("Windows service install lost its unique unchanged fatal text")
	}
	const allowance = `max(5*time.Second, 10*time.Since(baseline))`
	if strings.Count(source, allowance) != 1 || strings.Index(source, allowance) < strings.Index(source, "output, err := command.CombinedOutput()") {
		t.Fatal("Windows service install lost the single later calibrated allowance")
	}
	for _, budget := range []string{
		"const windowsSyntheticTestBudget = 90 * time.Second",
		"ctx, cancel := context.WithTimeout(t.Context(), windowsSyntheticTestBudget*4/15)",
		"commandCtx, commandCancel := context.WithDeadline(ctx, deadline.Add(-windowsSyntheticTestBudget/90))",
	} {
		if strings.Count(source, budget) != 1 {
			t.Fatal("Windows service install lost the named command budget and cleanup reserve")
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "service_subprocess_windows_test.go", source, 0)
	if err != nil {
		t.Fatal("cannot parse Windows service install source")
	}
	var body *ast.BlockStmt
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "TestWindowsServiceForegroundSubprocessCommands" {
			body = function.Body
		}
	}
	if body == nil {
		t.Fatal("Windows foreground subprocess test is missing")
	}
	baseline := -1
	for index, statement := range body.List {
		if foregroundSource(t, statement) == "baseline := time.Now()" {
			if baseline != -1 {
				t.Fatal("Windows service install repeated the cold spawn baseline")
			}
			baseline = index
		}
	}
	if baseline < 0 || len(body.List) <= baseline+6 {
		t.Fatal("Windows service install lost its top-level cold spawn")
	}
	// These consecutive statements require one direct cold invocation under
	// commandCtx. A timeout wrapper, retry, or moved calibration cannot pass.
	for index, want := range []string{
		`command := exec.CommandContext(commandCtx, host, "service", "install", "--root", root)`,
		"command.Env = environment",
		"command.WaitDelay = windowsSyntheticTestBudget / 90",
		"output, err := command.CombinedOutput()",
		"allowance := max(5*time.Second, 10*time.Since(baseline))",
	} {
		if foregroundSource(t, body.List[baseline+1+index]) != want {
			t.Fatal("Windows service install cold spawn must use the command budget before calibration")
		}
	}
	// A second assignment before the spawn could silently shorten the same
	// context even while leaving the direct CommandContext call unchanged.
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || assignment.Pos() >= body.List[baseline].Pos() {
			return true
		}
		for _, left := range assignment.Lhs {
			name, ok := left.(*ast.Ident)
			if !ok || (name.Name != "ctx" && name.Name != "commandCtx") {
				continue
			}
			actual := foregroundSource(t, assignment)
			if actual != "ctx, cancel := context.WithTimeout(t.Context(), windowsSyntheticTestBudget*4/15)" &&
				actual != "commandCtx, commandCancel := context.WithDeadline(ctx, deadline.Add(-windowsSyntheticTestBudget/90))" {
				t.Fatal("Windows service install cold context acquired an additional bound")
			}
		}
		return true
	})
	failure, ok := body.List[baseline+6].(*ast.IfStmt)
	if !ok || failure.Init != nil || failure.Else != nil ||
		foregroundSource(t, failure.Cond) != `err == nil || !strings.Contains(string(output), "service_manager_unavailable")` {
		t.Fatal("Windows service install changed its original refusal predicate")
	}
	statements := failure.Body.List
	if len(statements) != 4 || foregroundSource(t, statements[len(statements)-1]) != fatal ||
		foregroundSource(t, statements[len(statements)-2]) != `t.Logf("service_install class=%s", class)` {
		t.Fatal("Windows service install class line must immediately precede its fatal in the same block")
	}
	if foregroundSource(t, statements[0]) != `class := classifyServiceInstallFailure(commandCtx.Err(), nil, err, strings.Contains(string(output), "service_manager_unavailable"))` {
		t.Fatal("Windows service install lost the pure classifier with observed command context")
	}
	cause, ok := statements[1].(*ast.IfStmt)
	if !ok || cause.Init != nil || foregroundSource(t, cause.Cond) != `class == "exit_without_cause"` ||
		len(cause.Body.List) != 1 || foregroundSource(t, cause.Body.List[0]) != `t.Logf("foreground_exit class=%s", foregroundFailureClass(output))` {
		t.Fatal("Windows service install missing cause path lost its fixed foreground failure class")
	}
	other, ok := cause.Else.(*ast.BlockStmt)
	if !ok || len(other.List) != 1 || foregroundSource(t, other.List[0]) != `t.Logf("foreground_exit class=%s", class)` {
		t.Fatal("Windows service install budget or spawn failure lost its foreground class")
	}
	spawns := 0
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && foregroundSource(t, call) == `exec.CommandContext(commandCtx, host, "service", "install", "--root", root)` {
			spawns++
		}
		return true
	})
	if spawns != 1 {
		t.Fatal("Windows service install must make exactly one cold attempt")
	}
}

func TestServiceInstallClassDiagnosticEnrollment(t *testing.T) {
	node := testenv.RequireNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const packageName = "github.com/witwave-ai/witself/internal/tools/localcellharness"
	const testName = "TestWindowsServiceForegroundSubprocessCommands"
	const fatal = "Windows service install did not preserve adapter refusal"
	for _, row := range []struct{ value, want string }{
		{"deadline", "deadline"},
		{"parent_budget", "parent_budget"},
		{"exit_zero", "exit_zero"},
		{"exit_without_cause", "exit_without_cause"},
		{"start_failed", "start_failed"},
		// A quoted lowercase Go literal would enroll itself as vocabulary.
		{`outsidevocabularyprivate`, "(rejected)"},
	} {
		t.Run(strings.Trim(row.want, "()"), func(t *testing.T) {
			type event struct {
				Action  string
				Package string
				Test    string `json:",omitempty"`
				Output  string `json:",omitempty"`
			}
			var input strings.Builder
			encoder := json.NewEncoder(&input)
			for _, item := range []event{
				{Action: "start", Package: packageName},
				{Action: "run", Package: packageName, Test: testName},
				{Action: "output", Package: packageName, Test: testName, Output: "    service_subprocess_windows_test.go:1: service_install class=" + row.value + "\r\n"},
				{Action: "output", Package: packageName, Test: testName, Output: "    service_subprocess_windows_test.go:2: " + fatal + "\r\n"},
				{Action: "fail", Package: packageName, Test: testName},
				{Action: "fail", Package: packageName},
			} {
				if encoder.Encode(item) != nil {
					t.Fatal("cannot encode service install diagnostic fixture")
				}
			}
			command := exec.CommandContext(ctx, node, filepath.Join("..", "..", "..", "scripts", "ci-go-test-json-check.mjs"), "--package", packageName, "--require", testName)
			command.Stdin = strings.NewReader(input.String())
			command.WaitDelay = time.Second
			output, runErr := command.CombinedOutput()
			var exitErr *exec.ExitError
			if ctx.Err() != nil || !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatal("service install diagnostic checker did not report the synthetic failure")
			}
			want := "class " + packageName + " " + testName + " :: service_install class=" + row.want
			lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
			classes, found, why := 0, false, false
			for _, line := range lines {
				if strings.HasPrefix(line, "class ") {
					classes++
				}
				found = found || line == want
				why = why || line == "why "+packageName+" "+testName+" :: "+fatal
			}
			if classes != 1 || !found || !why || (row.want == "(rejected)" && strings.Contains(string(output), row.value)) {
				t.Fatal("service install diagnostic enrollment lost its prefix or closed vocabulary")
			}
		})
	}
}
