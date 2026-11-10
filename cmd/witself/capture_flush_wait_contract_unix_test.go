//go:build !windows

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureFlushHelperFileWaitBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed time.Duration
		want     time.Duration
	}{
		{"no_process_measurement", 0, 2 * time.Minute},
		{"healthy_helper", time.Second, 2 * time.Minute},
		{"loaded_helper", 30 * time.Second, 4 * time.Minute},
		{"very_loaded_helper", time.Minute, 8 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureFlushHelperFileWait(tc.observed); got != tc.want {
				t.Fatalf("helper file wait = %s, want %s for measured fixture time %s", got, tc.want, tc.observed)
			}
		})
	}
}

// Keep the measured budget wired into the actual reader: testing only the
// budget function would miss a regression to the previous fixed 60 s wait.
func TestCaptureFlushHelperWaitContract(t *testing.T) {
	functions := captureFlushFixtureFunctions(t)
	reader := functions["readCaptureFlushTestFile"]
	if reader == nil {
		t.Fatal("capture-flush observation reader is missing")
	}
	adaptiveDeadline := false
	ast.Inspect(reader, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Add" {
			return true
		}
		budget, ok := call.Args[0].(*ast.CallExpr)
		if !ok || len(budget.Args) != 1 {
			return true
		}
		name, ok := budget.Fun.(*ast.Ident)
		if !ok || name.Name != "captureFlushHelperFileWait" {
			return true
		}
		measured, ok := budget.Args[0].(*ast.Ident)
		adaptiveDeadline = ok && measured.Name == "observedElapsed"
		return true
	})
	if !adaptiveDeadline {
		t.Fatal("capture-flush helper file wait must use the measured fixture budget, not a fixed bound")
	}
}

func TestCaptureFlushObservationPublicationContract(t *testing.T) {
	functions := captureFlushFixtureFunctions(t)
	writer := functions["writeCaptureFlushTestFile"]
	if writer == nil {
		t.Fatal("capture-flush observation publisher is missing")
	}
	var writePosition, renamePosition token.Pos
	ast.Inspect(writer, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		switch selector.Sel.Name {
		case "WriteFile":
			writePosition = call.Pos()
		case "Rename":
			renamePosition = call.Pos()
		}
		return true
	})
	if !writePosition.IsValid() || !renamePosition.IsValid() || writePosition >= renamePosition {
		t.Fatal("capture-flush observations must be completely written before atomic publication")
	}
	for _, name := range []string{"TestCaptureFlushHelperProcess", "TestBackgroundCaptureFlushSurvivesParentExitAndHangup"} {
		function := functions[name]
		if function == nil {
			t.Fatalf("capture-flush fixture %s is missing", name)
		}
		ast.Inspect(function, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "WriteFile" {
				return true
			}
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "os" {
				t.Errorf("%s must publish observations atomically instead of calling os.WriteFile", name)
			}
			return true
		})
	}
}

func captureFlushFixtureFunctions(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	const path = "capture_flush_unix_test.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The source contract also runs on checkouts using CRLF line endings.
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	functions := make(map[string]*ast.FuncDecl)
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			functions[function.Name.Name] = function
		}
	}
	return functions
}

// A prefix must not reach the ordering assertion. Complete the synthetic append
// only after the reader has observed the prefix, without a scheduler-based sleep.
func TestCaptureFlushHelperWaitCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observation")
	writeCaptureFlushTestFile(t, path, []byte("detac"))
	inspections := 0
	got := readCaptureFlushTestFile(t, path, 0, func(raw []byte) bool {
		inspections++
		if raw[len(raw)-1] == '\n' {
			return true
		}
		writeCaptureFlushTestFile(t, path, []byte("detached\n"))
		return false
	})
	if string(got) != "detached\n" || inspections != 2 {
		t.Fatalf("helper observation = %q after %d inspections, want complete line after rejecting prefix", got, inspections)
	}
}
