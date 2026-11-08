package store_test

import (
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

type failingTB interface {
	Helper()
	Log(args ...any)
	FailNow()
}

func fatalLines(t failingTB, err error) {
	t.Helper()
	for _, line := range strings.Split(err.Error(), "\n") {
		if line != "" {
			t.Log(line)
		}
	}
	t.FailNow()
}

type fatalLinesRecorder struct {
	logs      []string
	events    []string
	failCalls int
}

func (r *fatalLinesRecorder) Helper() {
	r.events = append(r.events, "helper")
}

func (r *fatalLinesRecorder) Log(args ...any) {
	r.logs = append(r.logs, fmt.Sprint(args...))
	r.events = append(r.events, "log")
}

func (r *fatalLinesRecorder) FailNow() {
	r.failCalls++
	r.events = append(r.events, "fail")
}

func TestFatalLinesLogsEachLineThenFails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "joined_three_lines",
			err:  errors.Join(errors.New("first"), errors.New("second"), errors.New("third")),
			want: []string{"first", "second", "third"},
		},
		{
			name: "single_line",
			err:  errors.New("only line"),
			want: []string{"only line"},
		},
		{
			name: "trailing_newline",
			err:  errors.New("line before newline\n"), //nolint:revive // Intentional trailing newline tests empty-line suppression.
			want: []string{"line before newline"},
		},
		{
			name: "readiness_join",
			err: errors.Join(
				errors.New("capturemix stage failed: processes_ready"),
				errors.New("capturemix process readiness failed: kind=worker version=new cause=unclassified"),
			),
			want: []string{
				"capturemix stage failed: processes_ready",
				"capturemix process readiness failed: kind=worker version=new cause=unclassified",
			},
		},
		{
			name: "empty_lines_between",
			err:  errors.New("\nfirst\n\nlast"),
			want: []string{"first", "last"},
		},
		{
			name: "only_empty_lines",
			err:  errors.New("\n\n"), //nolint:revive // Intentional empty lines must still fail without logging.
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var recorder fatalLinesRecorder
			fatalLines(&recorder, tt.err)
			if len(recorder.logs) != len(tt.want) {
				t.Fatalf("Log call count = %d, want %d", len(recorder.logs), len(tt.want))
			}
			for i, want := range tt.want {
				if recorder.logs[i] != want {
					t.Fatalf("Log call %d text differs from its source line", i+1)
				}
			}
			if recorder.failCalls != 1 {
				t.Fatalf("FailNow call count = %d, want 1", recorder.failCalls)
			}
			wantEvents := []string{"helper"}
			for range tt.want {
				wantEvents = append(wantEvents, "log")
			}
			wantEvents = append(wantEvents, "fail")
			if !slices.Equal(recorder.events, wantEvents) {
				t.Fatal("calls must be Helper first, all Log calls in order, then FailNow last")
			}
		})
	}
}

func TestCaptureMixRehearsalFatalsUseFatalLines(t *testing.T) {
	source, err := os.ReadFile("capture_mix_rehearsal_test.go")
	if err != nil {
		t.Fatal("read capturemix rehearsal source")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, tc := range []struct {
		name   string
		source string
	}{
		{name: "LF", source: lf},
		{name: "CRLF", source: strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture_mix_rehearsal_test.go")
			if err := os.WriteFile(path, []byte(tc.source), 0600); err != nil {
				t.Fatal("write capturemix rehearsal source fixture")
			}
			assertCaptureMixRehearsalFatalsUseFatalLines(t, path)
		})
	}
}

func assertCaptureMixRehearsalFatalsUseFatalLines(t *testing.T, path string) {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read capturemix rehearsal source fixture")
	}
	normalized := strings.ReplaceAll(string(source), "\r\n", "\n")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, normalized, 0)
	if err != nil {
		t.Fatal("parse capturemix rehearsal source")
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "TestCaptureMixMatrix" || fn.Body == nil {
			continue
		}
		body := normalized[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset]
		if got := strings.Count(body, "fatalLines(t, err)"); got != 2 {
			t.Errorf("fatalLines call-site count = %d, want 2", got)
		}
		if got := strings.Count(body, "t.Fatal(err.Error())"); got != 0 {
			t.Errorf("legacy fatal call-site count = %d, want 0", got)
		}
		return
	}
	t.Fatal("TestCaptureMixMatrix function body not found")
}
