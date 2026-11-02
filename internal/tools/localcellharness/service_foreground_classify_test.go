package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os/exec"
	"testing"
)

type foregroundExitKind uint8

const (
	foregroundCommandBudgetExpired foregroundExitKind = iota
	foregroundParentBudgetExpired
	foregroundExitedWithError
	foregroundExitedCleanly
)

// Wait can complete after either context expires, including a nil Wait result.
// Classify the observed state in this order: command budget, parent budget,
// error exit, clean exit. exec.Cmd.Wait returns both process and copier/WaitDelay
// errors through exitErr. A clean exit is fatal before readiness, accepted after
// shutdown; the Windows call sites, not this pure decision, implement that policy.
func classifyForegroundExit(commandErr, parentErr, exitErr error) foregroundExitKind {
	switch {
	case commandErr != nil:
		return foregroundCommandBudgetExpired
	case parentErr != nil:
		return foregroundParentBudgetExpired
	case exitErr != nil:
		return foregroundExitedWithError
	default:
		return foregroundExitedCleanly
	}
}

func TestClassifyForegroundExit(t *testing.T) {
	// Explicit expectations cover every nil/non-nil combination, including each
	// adjacent precedence inversion and the R6 command-expired OR clean mutant.
	for _, test := range []struct {
		name                           string
		commandErr, parentErr, exitErr error
		want                           foregroundExitKind
	}{
		{"clean", nil, nil, nil, foregroundExitedCleanly},
		{"error", nil, nil, errors.New("synthetic exit"), foregroundExitedWithError},
		{"parent_clean", nil, context.DeadlineExceeded, nil, foregroundParentBudgetExpired},
		{"parent_error", nil, context.DeadlineExceeded, errors.New("synthetic exit"), foregroundParentBudgetExpired},
		{"command_clean", context.DeadlineExceeded, nil, nil, foregroundCommandBudgetExpired},
		{"command_error", context.DeadlineExceeded, nil, errors.New("synthetic exit"), foregroundCommandBudgetExpired},
		{"both_clean", context.DeadlineExceeded, context.DeadlineExceeded, nil, foregroundCommandBudgetExpired},
		{"both_error", context.DeadlineExceeded, context.DeadlineExceeded, errors.New("synthetic exit"), foregroundCommandBudgetExpired},
		{"command_cancelled", context.Canceled, context.DeadlineExceeded, exec.ErrWaitDelay, foregroundCommandBudgetExpired},
		{"parent_cancelled", nil, context.Canceled, exec.ErrWaitDelay, foregroundParentBudgetExpired},
		{"wait_delay", nil, nil, exec.ErrWaitDelay, foregroundExitedWithError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if classifyForegroundExit(test.commandErr, test.parentErr, test.exitErr) != test.want {
				t.Fatal("foreground exit classification violated budget precedence")
			}
		})
	}
}

// Single source of truth for the kind x generation contract and the report's
// literal-to-cause table. Empty literals mean accepted clean shutdown. The four
// wait branches retain their existing direct literals and are checked too.
var foregroundDiagnosticLiterals = []struct {
	phase, branch string
	literals      [2]string
}{
	{"readiness", "foregroundCommandBudgetExpired", [2]string{
		"foreground command budget expired before authenticated status on first start",
		"foreground command budget expired before authenticated status after restart"}},
	{"readiness", "foregroundParentBudgetExpired", [2]string{
		"foreground synthetic budget expired before authenticated status on first start",
		"foreground synthetic budget expired before authenticated status after restart"}},
	{"readiness", "foregroundExitedWithError", [2]string{
		"foreground host exited with error before authenticated status on first start",
		"foreground host exited with error before authenticated status after restart"}},
	{"readiness", "foregroundExitedCleanly", [2]string{
		"foreground host exited cleanly before authenticated status on first start",
		"foreground host exited cleanly before authenticated status after restart"}},
	{"shutdown", "foregroundCommandBudgetExpired", [2]string{
		"foreground command budget expired during shutdown on first start",
		"foreground command budget expired during shutdown after restart"}},
	{"shutdown", "foregroundParentBudgetExpired", [2]string{
		"foreground synthetic budget expired during shutdown on first start",
		"foreground synthetic budget expired during shutdown after restart"}},
	{"shutdown", "foregroundExitedWithError", [2]string{
		"foreground host exited with error during shutdown on first start",
		"foreground host exited with error during shutdown after restart"}},
	{"shutdown", "foregroundExitedCleanly", [2]string{}},
	{"readiness", "parent_wait", [2]string{
		"foreground readiness exceeded synthetic budget on first start",
		"foreground readiness exceeded synthetic budget after restart"}},
	{"readiness", "allowance_wait", [2]string{
		"foreground host did not expose authenticated status within allowance on first start",
		"foreground host did not expose authenticated status within allowance after restart"}},
	{"shutdown", "parent_wait", [2]string{
		"foreground shutdown exceeded synthetic budget on first start",
		"foreground shutdown exceeded synthetic budget after restart"}},
	{"shutdown", "allowance_wait", [2]string{
		"foreground console shutdown exceeded allowance on first start",
		"foreground console shutdown exceeded allowance after restart"}},
}

func TestWindowsForegroundExitClassifierContract(t *testing.T) {
	// Parse Windows source on every platform. Pin both the call arguments and
	// each enum case's generation-specific direct t.Fatal; merely finding all
	// literals cannot detect a misplaced clean-exit branch (R6 M5).
	file, err := parser.ParseFile(token.NewFileSet(), "service_subprocess_windows_test.go", nil, 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground classifier source")
	}
	var selections []*ast.SelectStmt
	ast.Inspect(file, func(node ast.Node) bool {
		selection, ok := node.(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, statement := range selection.Body.List {
			clause := statement.(*ast.CommClause)
			if foregroundSource(t, clause.Comm) == "exitErr := <-done" {
				selections = append(selections, selection)
			}
		}
		return true
	})
	if len(selections) != 2 {
		t.Fatal("Windows foreground classifier lost readiness or shutdown exit selection")
	}
	for index, phase := range []string{"readiness", "shutdown"} {
		t.Run(phase, func(t *testing.T) {
			branches := make(map[string][]ast.Stmt)
			for _, statement := range selections[index].Body.List {
				clause := statement.(*ast.CommClause)
				switch foregroundSource(t, clause.Comm) {
				case "exitErr := <-done":
					// No extra conditional may gate the decision or bypass it.
					prefix := 1
					if phase == "readiness" {
						prefix++
					}
					if len(clause.Body) != prefix+1 || foregroundSource(t, clause.Body[0]) != "finished = true" {
						t.Fatal("Windows foreground exit classifier is conditionally bypassed")
					}
					if phase == "readiness" && foregroundSource(t, clause.Body[1]) != "windowsLogForegroundExit(t, command, captured.Bytes())" {
						t.Fatal("Windows foreground readiness exit lost guarded diagnostics")
					}
					decision, ok := clause.Body[prefix].(*ast.SwitchStmt)
					if !ok || decision.Init != nil || foregroundSource(t, decision.Tag) != "classifyForegroundExit(commandCtx.Err(), ctx.Err(), exitErr)" {
						t.Fatal("Windows foreground exit must use the pure classifier with observed errors")
					}
					if len(decision.Body.List) != 4 {
						t.Fatal("Windows foreground exit classifier lost an explicit kind")
					}
					for _, statement := range decision.Body.List {
						branch := statement.(*ast.CaseClause)
						if len(branch.List) != 1 {
							t.Fatal("Windows foreground exit classifier requires one explicit kind per case")
						}
						name := foregroundSource(t, branch.List[0])
						if _, duplicate := branches[name]; duplicate {
							t.Fatal("Windows foreground exit classifier repeats a kind")
						}
						branches[name] = branch.Body
					}
				case "<-ctx.Done()":
					branches["parent_wait"] = clause.Body
				case "<-deadline.C", "<-time.After(allowance)":
					branches["allowance_wait"] = clause.Body
				}
			}
			if len(branches) != 6 {
				t.Fatal("Windows foreground classifier branch set changed")
			}
			for _, contract := range foregroundDiagnosticLiterals {
				if contract.phase != phase {
					continue
				}
				t.Run(contract.branch, func(t *testing.T) {
					body, exists := branches[contract.branch]
					if !exists {
						t.Fatal("Windows foreground classifier lacks a contracted branch")
					}
					want := ""
					if contract.literals[0] != "" {
						if phase == "shutdown" && contract.branch != "parent_wait" && contract.branch != "allowance_wait" {
							want = "windowsLogForegroundExit(t, command, captured.Bytes());"
						}
						want += fmt.Sprintf("if iteration == 0 { t.Fatal(%q) }; t.Fatal(%q)", contract.literals[0], contract.literals[1])
					}
					fixture, err := parser.ParseFile(token.NewFileSet(), "contract", "package main; func contract() {"+want+"}", 0)
					if err != nil {
						t.Fatal("invalid foreground literal contract")
					}
					expected := fixture.Decls[0].(*ast.FuncDecl).Body
					// Normalize positions as well as whitespace; source line breaks
					// must not affect the semantic statement/literal comparison.
					actual := &ast.BlockStmt{List: body}
					if foregroundSource(t, actual) != foregroundSource(t, expected) {
						t.Fatal("Windows foreground kind and generation selected the wrong literal or clean policy")
					}
				})
			}
		})
	}
}

func foregroundSource(t *testing.T, node ast.Node) string {
	t.Helper()
	if node == nil {
		return ""
	}
	var output bytes.Buffer
	if format.Node(&output, token.NewFileSet(), node) != nil {
		t.Fatal("cannot format foreground source contract")
	}
	return output.String()
}
