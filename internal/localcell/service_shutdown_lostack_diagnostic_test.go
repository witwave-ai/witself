package localcell

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

type lostACKReporter interface {
	Helper()
	Error(...any)
}

// These labels describe only the final outcome, not an intermediate stage,
// observed exit, listener state, or cause. Component is deliberately ignored.
func reportLostACKOutcome(t lostACKReporter, outcome serviceStopOutcome) {
	t.Helper()
	switch outcome.Kind {
	case serviceStopGraceful, serviceStopAlreadyExited:
		return
	case serviceStopForced:
		if outcome.Detail == "service_shutdown_forced" {
			t.Error("lost ACK final outcome forced")
			return
		}
	case serviceStopUnknown:
		switch outcome.Detail {
		case "shutdown:deadline":
			t.Error("lost ACK final outcome unknown deadline")
			return
		case "shutdown:ack":
			t.Error("lost ACK final outcome unknown ack")
			return
		case "shutdown:incarnation":
			t.Error("lost ACK final outcome unknown incarnation")
			return
		case "shutdown:force":
			t.Error("lost ACK final outcome unknown force")
			return
		}
	case serviceStopRefused:
		switch outcome.Detail {
		case "shutdown:incarnation":
			t.Error("lost ACK final outcome refused incarnation")
			return
		case "shutdown:descriptor":
			t.Error("lost ACK final outcome refused descriptor")
			return
		case "shutdown:pipe":
			t.Error("lost ACK final outcome refused pipe")
			return
		case "shutdown:ack":
			t.Error("lost ACK final outcome refused ack")
			return
		case "shutdown:migration":
			t.Error("lost ACK final outcome refused migration")
			return
		case "shutdown:force_rights":
			t.Error("lost ACK final outcome refused force_rights")
			return
		case "shutdown:force":
			t.Error("lost ACK final outcome refused force")
			return
		}
	}
	t.Error("lost ACK final outcome unclassified")
}

type lostACKRecordingReporter struct {
	errors [][]any
}

func (*lostACKRecordingReporter) Helper() {}

func (r *lostACKRecordingReporter) Error(args ...any) {
	r.errors = append(r.errors, append([]any(nil), args...))
}

func TestLostACKOutcomeDiagnostic(t *testing.T) {
	const sentinel = "PRIVATE_SENTINEL_LOSTACK"
	// This explicit table is the expected public vocabulary, independent of the
	// helper's switches. Never derive expectations from the implementation.
	for _, tc := range []struct {
		name   string
		kind   serviceStopKind
		detail string
		want   string
	}{
		{"forced", serviceStopForced, "service_shutdown_forced", "lost ACK final outcome forced"},
		{"unknown_deadline", serviceStopUnknown, "shutdown:deadline", "lost ACK final outcome unknown deadline"},
		{"unknown_ack", serviceStopUnknown, "shutdown:ack", "lost ACK final outcome unknown ack"},
		{"unknown_incarnation", serviceStopUnknown, "shutdown:incarnation", "lost ACK final outcome unknown incarnation"},
		{"unknown_force", serviceStopUnknown, "shutdown:force", "lost ACK final outcome unknown force"},
		{"refused_incarnation", serviceStopRefused, "shutdown:incarnation", "lost ACK final outcome refused incarnation"},
		{"refused_descriptor", serviceStopRefused, "shutdown:descriptor", "lost ACK final outcome refused descriptor"},
		{"refused_pipe", serviceStopRefused, "shutdown:pipe", "lost ACK final outcome refused pipe"},
		{"refused_ack", serviceStopRefused, "shutdown:ack", "lost ACK final outcome refused ack"},
		{"refused_migration", serviceStopRefused, "shutdown:migration", "lost ACK final outcome refused migration"},
		{"refused_force_rights", serviceStopRefused, "shutdown:force_rights", "lost ACK final outcome refused force_rights"},
		{"refused_force", serviceStopRefused, "shutdown:force", "lost ACK final outcome refused force"},
		{"graceful", serviceStopGraceful, "", ""},
		{"already_exited", serviceStopAlreadyExited, "", ""},
		{"graceful_private_detail", serviceStopGraceful, sentinel, ""},
		{"already_exited_private_detail", serviceStopAlreadyExited, sentinel, ""},
		{"unknown_kind", "unexpected", "shutdown:ack", "lost ACK final outcome unclassified"},
		{"unknown_detail", serviceStopUnknown, "unexpected", "lost ACK final outcome unclassified"},
		{"inconsistent_pair", serviceStopRefused, "shutdown:deadline", "lost ACK final outcome unclassified"},
		{"inconsistent_forced", serviceStopForced, "shutdown:force", "lost ACK final outcome unclassified"},
		{"empty_pair", "", "", "lost ACK final outcome unclassified"},
		{"private_kind", serviceStopKind(sentinel), "shutdown:pipe", "lost ACK final outcome unclassified"},
		{"private_detail", serviceStopRefused, sentinel, "lost ACK final outcome unclassified"},
		{"private_kind_and_detail", serviceStopKind(sentinel), sentinel, "lost ACK final outcome unclassified"},
		{"extended_detail", serviceStopRefused, "shutdown:pipe " + sentinel, "lost ACK final outcome unclassified"},
		{"extended_kind", serviceStopKind("refused " + sentinel), "shutdown:pipe", "lost ACK final outcome unclassified"},
		{"padded_detail", serviceStopRefused, " shutdown:pipe ", "lost ACK final outcome unclassified"},
		{"padded_kind", " refused ", "shutdown:pipe", "lost ACK final outcome unclassified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise every pair with ordinary and adversarial components. Neither
			// component can affect classification or become output.
			for _, component := range []string{"server", sentinel} {
				r := &lostACKRecordingReporter{}
				reportLostACKOutcome(r, serviceStopOutcome{Kind: tc.kind, Detail: tc.detail, Component: component})
				if tc.want == "" {
					if len(r.errors) != 0 {
						t.Fatal("lost ACK success emitted a diagnostic")
					}
					continue
				}
				if len(r.errors) != 1 || len(r.errors[0]) != 1 {
					t.Fatal("lost ACK failure did not emit exactly one fixed argument")
				}
				got, ok := r.errors[0][0].(string)
				if !ok || got != tc.want || strings.Contains(got, sentinel) {
					t.Fatal("lost ACK diagnostic violated exact mapping or privacy")
				}
			}
		})
	}
}

func TestLostACKOutcomeDiagnosticWiring(t *testing.T) {
	data, err := os.ReadFile("service_shutdown_windows_test.go")
	if err != nil {
		t.Fatal("lost ACK wiring source unavailable")
	}
	// Source contracts must also work with a Windows checkout.
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "service_shutdown_windows_test.go", source, 0)
	if err != nil {
		t.Fatal("lost ACK wiring source cannot be parsed")
	}
	var escalation *ast.FuncDecl
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "TestWindowsShutdownEscalation" {
			escalation = fn
		}
	}
	if escalation == nil || escalation.Body == nil {
		t.Fatal("lost ACK escalation test missing")
	}
	render := func(node ast.Node) string {
		t.Helper()
		var output bytes.Buffer
		if err := format.Node(&output, positions, node); err != nil {
			t.Fatal("lost ACK wiring node cannot be formatted")
		}
		return output.String()
	}
	helperCalls := 0
	ast.Inspect(escalation.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "reportLostACKOutcome" {
				helperCalls++
			}
		}
		return true
	})
	if helperCalls != 1 {
		t.Fatal("lost ACK diagnostic must have exactly one guarded call")
	}
	lostCases := 0
	ast.Inspect(escalation.Body, func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for index, statement := range block.List {
			selection, ok := statement.(*ast.SwitchStmt)
			if !ok || selection.Tag == nil || render(selection.Tag) != "mode" {
				continue
			}
			for _, branch := range selection.Body.List {
				clause, ok := branch.(*ast.CaseClause)
				if !ok || len(clause.List) != 1 || render(clause.List[0]) != `"lost"` {
					continue
				}
				lostCases++
				if selection.Init != nil || index == 0 || render(block.List[index-1]) != "outcome := f.stop(t)" {
					t.Fatal("lost ACK outcome must come directly from fixture stop")
				}
				if len(clause.Body) != 1 {
					t.Fatal("lost ACK case must contain only the original failing guard")
				}
				guard, ok := clause.Body[0].(*ast.IfStmt)
				if !ok || guard.Init != nil || guard.Else != nil || render(guard.Cond) != "!outcome.permitsDrain() || outcome.Kind == serviceStopForced" {
					t.Fatal("lost ACK failing guard semantics changed")
				}
				if len(guard.Body.List) != 3 {
					t.Fatal("lost ACK failing guard must retain diagnostic log and fatal")
				}
				if render(guard.Body.List[0]) != "reportLostACKOutcome(t, outcome)" {
					t.Fatal("lost ACK diagnostic must precede the original log and fatal")
				}
				if render(guard.Body.List[1]) != `t.Logf("shutdown kind=%s detail=%s budget=%s startup=%s", outcome.Kind, outcome.Detail, shutdownFixtureBudget(f.startup), f.startup)` ||
					render(guard.Body.List[2]) != `t.Fatal("lost ACK with observed exit did not complete safely")` {
					t.Fatal("lost ACK original log and generic fatal changed")
				}
			}
		}
		return true
	})
	if lostCases != 1 {
		t.Fatal("lost ACK case must be uniquely located")
	}
}
