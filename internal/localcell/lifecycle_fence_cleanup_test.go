package localcell_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// clearMaintenanceFixture is test cleanup, never a production clearing procedure.
func clearMaintenanceFixture(ctx context.Context, root string, stop func(context.Context) error, logf func(string, ...any)) error {
	path := filepath.Join(root, "journal", "maintenance.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("cannot inspect planted maintenance fixture")
	}
	if err := stop(ctx); err != nil {
		logf("test cleanup could not stop cell before clearing planted maintenance fixture")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot remove planted maintenance fixture")
	}
	logf("planted maintenance fixture cleared by test cleanup")
	return nil
}

func TestMaintenanceFixtureCleanup(t *testing.T) {
	for _, name := range []string{
		"absent-root", "absent-journal", "absent-file", "present",
		"present-stop-error", "present-already-gone", "remove-error", "stop-receives-parent",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			journal := filepath.Join(root, "journal")
			path := filepath.Join(journal, "maintenance.json")
			absent := strings.HasPrefix(name, "absent-")
			if name == "absent-root" {
				if os.Remove(root) != nil {
					t.Fatal("cannot prepare absent root")
				}
			} else if name != "absent-journal" {
				if os.Mkdir(journal, 0o700) != nil {
					t.Fatal("cannot prepare journal directory")
				}
				if lifecycleWriteFile(filepath.Join(journal, "operations.jsonl"), []byte("retained journal bytes\n")) != nil {
					t.Fatal("cannot prepare sibling fixture")
				}
			}
			if name == "remove-error" {
				if os.Mkdir(path, 0o700) != nil || lifecycleWriteFile(filepath.Join(path, "child"), []byte("retained child\n")) != nil {
					t.Fatal("cannot prepare nonempty fixture directory")
				}
			} else if !absent && lifecycleWriteFile(path, []byte("planted fixture\n")) != nil {
				t.Fatal("cannot prepare maintenance fixture")
			}
			rootBefore := maintenanceFixtureNames(t, root)
			journalBefore := maintenanceFixtureNames(t, journal)
			type parentKey struct{}
			parentValue := new(int)
			ctx := context.WithValue(t.Context(), parentKey{}, parentValue)
			stopCalls := 0
			stop := func(parent context.Context) error {
				stopCalls++
				if parent != ctx || parent.Value(parentKey{}) != parentValue {
					t.Error("stop lost the supplied parent context")
				}
				if _, err := os.Lstat(path); err != nil {
					t.Error("stop must precede fixture removal")
				}
				if name == "present-already-gone" && os.Remove(path) != nil {
					t.Error("cannot remove fixture during stop")
				}
				if name == "present-stop-error" {
					return errors.New("private stop failure detail")
				}
				return nil
			}
			var logs []string
			err := clearMaintenanceFixture(ctx, root, stop, func(format string, args ...any) {
				logs = append(logs, fmt.Sprintf(format, args...))
			})
			wantStops := 1
			var wantLogs []string
			if absent {
				wantStops = 0
			} else if name != "remove-error" {
				if name == "present-stop-error" {
					wantLogs = append(wantLogs, "test cleanup could not stop cell before clearing planted maintenance fixture")
				}
				wantLogs = append(wantLogs, "planted maintenance fixture cleared by test cleanup")
			}
			if stopCalls != wantStops {
				t.Error("cleanup made an unexpected number of stop calls")
			}
			if !slices.Equal(logs, wantLogs) {
				t.Error("cleanup logs differ from the fixed value-free lines")
			}
			if name == "remove-error" {
				if err == nil || err.Error() != "cannot remove planted maintenance fixture" {
					t.Error("removal failure lost its fixed value-free error")
				}
				data, readErr := os.ReadFile(filepath.Join(path, "child"))
				if readErr != nil || string(data) != "retained child\n" {
					t.Error("failed removal changed the fixture directory child")
				}
			} else {
				if err != nil {
					t.Error("fixture cleanup returned an unexpected error")
				}
				if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
					t.Error("fixture remains after cleanup")
				}
				if !absent {
					journalBefore = slices.DeleteFunc(journalBefore, func(name string) bool { return name == "maintenance.json" })
				}
			}
			if !slices.Equal(rootBefore, maintenanceFixtureNames(t, root)) ||
				!slices.Equal(journalBefore, maintenanceFixtureNames(t, journal)) {
				t.Error("cleanup changed unrelated directory entries")
			}
			if name != "absent-root" && name != "absent-journal" {
				data, readErr := os.ReadFile(filepath.Join(journal, "operations.jsonl"))
				if readErr != nil || string(data) != "retained journal bytes\n" {
					t.Error("cleanup changed sibling journal bytes")
				}
			}
		})
	}
}

// Read names only; no cell lock file is opened or inspected.
func maintenanceFixtureNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{"absent-directory"}
	}
	if err != nil {
		t.Fatal("cannot list fixture directory")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestMaintenanceFixtureCleanupSourceContract(t *testing.T) {
	read := func(path string) []byte {
		t.Helper()
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read maintenance fixture contract source")
		}
		return bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	}
	source := read("lifecycle_integration_test.go")
	admission := read("update_admission_lifecycle_test.go")
	for _, lineEnding := range []string{"LF", "CRLF"} {
		t.Run(lineEnding, func(t *testing.T) {
			left, right := source, admission
			if lineEnding == "CRLF" {
				left = bytes.ReplaceAll(left, []byte("\n"), []byte("\r\n"))
				right = bytes.ReplaceAll(right, []byte("\n"), []byte("\r\n"))
			}
			if err := maintenanceFixtureCleanupContract(left, right); err != nil {
				t.Fatal(err)
			}
		})
	}
	const guard = "\t\tif err := clearMaintenanceFixture(ctx, root, cleanupCell.Stop, t.Logf); err != nil {\n" +
		"\t\t\tt.Error(\"cannot clear planted maintenance fixture\")\n\t\t\treturn\n\t\t}\n"
	const repair = "\t\tif repairErr := cleanupCell.Repair(ctx); repairErr != nil {\n" +
		"\t\t\tt.Errorf(\"cleanup repair: %v\", repairErr)\n\t\t\treturn\n\t\t}\n"
	for _, mutation := range []struct{ name, before, after, refusal string }{
		{"deleted", guard, "", "fixture cleanup call must occur exactly once"},
		{"after-repair", repair, repair + guard, "fixture cleanup call is outside the required order"},
		{"background-parent", "clearMaintenanceFixture(ctx, root,", "clearMaintenanceFixture(context.Background(), root,", "fixture cleanup arguments changed"},
		{"nil-stop", "clearMaintenanceFixture(ctx, root, cleanupCell.Stop,", "clearMaintenanceFixture(ctx, root, nil,", "fixture cleanup arguments changed"},
		{"duplicate", guard, guard + guard, "fixture cleanup call must occur exactly once"},
		{"no-return", guard, strings.Replace(guard, "\t\t\treturn\n", "", 1), "fixture cleanup error branch must return"},
		{"unreachable", guard, "\t\tif false {\n" + guard + "\t\t}\n", "fixture cleanup guard must be a direct cleanup statement"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			text := string(source)
			if strings.Count(text, mutation.before) != 1 {
				t.Fatal("maintenance fixture contract mutation site is not unique")
			}
			if mutation.name == "after-repair" {
				if strings.Count(text, guard) != 1 {
					t.Fatal("maintenance fixture contract move site is not unique")
				}
				text = strings.Replace(text, guard, "", 1)
			}
			mutant := strings.Replace(text, mutation.before, mutation.after, 1)
			if mutant == string(source) {
				t.Fatal("maintenance fixture contract mutation did not apply")
			}
			err := maintenanceFixtureCleanupContract([]byte(mutant), admission)
			if err == nil || err.Error() != mutation.refusal {
				t.Fatal("maintenance fixture contract did not produce the expected refusal")
			}
		})
	}
}

func maintenanceFixtureCleanupContract(source, admission []byte) error {
	function := func(source []byte, name string) *ast.FuncDecl {
		file, err := parser.ParseFile(token.NewFileSet(), "", source, 0)
		if err != nil {
			return nil
		}
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv == nil {
				return fn
			}
		}
		return nil
	}
	disposable := function(source, "disposableCellWithCleanup")
	if disposable == nil || disposable.Body == nil {
		return errors.New("cannot parse disposable cleanup function")
	}
	var closure *ast.FuncLit
	closures := 0
	for _, statement := range disposable.Body.List {
		expression, ok := statement.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expression.X.(*ast.CallExpr)
		if !ok || lifecycleProofExpression(call.Fun) != "t.Cleanup" || len(call.Args) != 1 {
			continue
		}
		closure, _ = call.Args[0].(*ast.FuncLit)
		closures++
	}
	if closures != 1 || closure == nil {
		return errors.New("fixture cleanup closure must occur exactly once")
	}
	var fixture, before, repair *ast.CallExpr
	fixtureCount, beforeCount, repairCount := 0, 0, 0
	ast.Inspect(closure.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch lifecycleProofExpression(call.Fun) {
		case "clearMaintenanceFixture":
			fixture, fixtureCount = call, fixtureCount+1
		case "beforeCleanup":
			before, beforeCount = call, beforeCount+1
		case "cleanupCell.Repair":
			repair, repairCount = call, repairCount+1
		}
		return true
	})
	if fixtureCount != 1 {
		return errors.New("fixture cleanup call must occur exactly once")
	}
	if lifecycleProofExpression(fixture) != "clearMaintenanceFixture(ctx, root, cleanupCell.Stop, t.Logf)" {
		return errors.New("fixture cleanup arguments changed")
	}
	if beforeCount != 1 || repairCount != 1 || before.Pos() >= fixture.Pos() || fixture.Pos() >= repair.Pos() {
		return errors.New("fixture cleanup call is outside the required order")
	}
	var guard *ast.IfStmt
	for _, statement := range closure.Body.List {
		branch, ok := statement.(*ast.IfStmt)
		if !ok {
			continue
		}
		assignment, ok := branch.Init.(*ast.AssignStmt)
		if ok && len(assignment.Rhs) == 1 && assignment.Rhs[0] == fixture &&
			lifecycleProofExpression(assignment) == "err := clearMaintenanceFixture(ctx, root, cleanupCell.Stop, t.Logf)" {
			guard = branch
		}
	}
	if guard == nil || lifecycleProofExpression(guard.Cond) != "err != nil" || guard.Else != nil {
		return errors.New("fixture cleanup guard must be a direct cleanup statement")
	}
	if len(guard.Body.List) == 0 {
		return errors.New("fixture cleanup error branch must return")
	}
	last, ok := guard.Body.List[len(guard.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(last.Results) != 0 {
		return errors.New("fixture cleanup error branch must return")
	}
	observation := function(admission, "TestMaintenanceFenceStopAndInspectionRemainUsable")
	if observation == nil || observation.Body == nil {
		return errors.New("cannot parse maintenance observation test")
	}
	removes := false
	ast.Inspect(observation.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && lifecycleProofExpression(call.Fun) == "os.Remove" {
			removes = true
		}
		return true
	})
	if removes {
		return errors.New("maintenance observation test restored private fixture removal")
	}
	return nil
}
