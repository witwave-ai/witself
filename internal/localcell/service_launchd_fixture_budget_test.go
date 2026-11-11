package localcell

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// launchdFixtureRemovalTimeout is deliberately far above any scheduling or
// filesystem delay: a test that is not about expiry must never be decided by it.
// The adapter polls every timeout/128, so this also sets the fixture poll interval.
const launchdFixtureRemovalTimeout = 16 * time.Second

func launchdRemovalRefusalClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrServiceOwnershipConflict):
		return "ownership_conflict"
	case errors.Is(err, ErrServiceRemovalFailed):
		return "removal_failed"
	default:
		return "other"
	}
}

func TestLaunchdFixtureRemovalBudget(t *testing.T) {
	if launchdFixtureRemovalTimeout < 4*time.Second || launchdFixtureRemovalTimeout > launchdExitTimeout+30*time.Second {
		t.Fatal("launchd fixture removal budget is outside its bounds")
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: "none"},
		{name: "ownership_conflict", err: ErrServiceOwnershipConflict, want: "ownership_conflict"},
		{name: "removal_failed", err: ErrServiceRemovalFailed, want: "removal_failed"},
		{name: "wrapped_ownership_conflict", err: fmt.Errorf("x: %w", ErrServiceOwnershipConflict), want: "ownership_conflict"},
		{name: "wrapped_removal_failed", err: fmt.Errorf("x: %w", ErrServiceRemovalFailed), want: "removal_failed"},
		{name: "joined", err: errors.Join(ErrServiceRemovalFailed, ErrServiceOwnershipConflict), want: "ownership_conflict"},
		{name: "unrelated", err: errors.New("unrelated"), want: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if launchdRemovalRefusalClass(tc.err) != tc.want {
				t.Fatal("launchd removal refusal class differs")
			}
		})
	}
}

func TestLaunchdFixtureRemovalBudgetSourceContract(t *testing.T) {
	data, err := os.ReadFile("service_launchd_adapter_test.go")
	if err != nil {
		t.Fatal("launchd fixture source could not be read")
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "service_launchd_adapter_test.go", source, 0)
	if err != nil {
		t.Fatal("launchd fixture source could not be parsed")
	}
	namedBudgets := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "launchdFixture" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			kind, ok := literal.Type.(*ast.Ident)
			if !ok || kind.Name != "launchdAgentAdapter" {
				return true
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				text := source[positions.Position(field.Pos()).Offset:positions.Position(field.End()).Offset]
				if text == "removalTimeout: launchdFixtureRemovalTimeout" {
					namedBudgets++
				}
			}
			return true
		})
	}
	if namedBudgets != 1 {
		t.Fatal("launchd fixture no longer uses the named removal budget")
	}
	if strings.Contains(source, "128 * time.Millisecond") {
		t.Fatal("launchd fixture retains the old removal budget")
	}
	overrides := make(map[string]int)
	ast.Inspect(file, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, left := range assignment.Lhs {
			selector, ok := left.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "removalTimeout" {
				continue
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != "a" {
				continue
			}
			if assignment.Tok != token.ASSIGN || len(assignment.Lhs) != len(assignment.Rhs) {
				t.Fatal("launchd fixture removal override has an unexpected assignment form")
			}
			right := assignment.Rhs[index]
			overrides[source[positions.Position(right.Pos()).Offset:positions.Position(right.End()).Offset]]++
		}
		return true
	})
	if len(overrides) != 2 || overrides["100 * observedCommandDelay"] != 1 || overrides["20 * time.Millisecond"] != 1 {
		t.Fatal("launchd fixture removal overrides differ from the allowed pair")
	}
}
