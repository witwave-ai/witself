package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Pin the class to its assertion, not just to a word appearing somewhere in
// the file. The four waits classify once before either generation's fatal.
var foregroundSubprocessDiagnosticSites = []struct {
	class, assertion, restart string
}{
	{"fixture_layout", "cannot create synthetic installation layout", ""},
	{"fixture_record_open", "cannot create synthetic private record", ""},
	{"fixture_record_write", "cannot publish synthetic private record", ""},
	{"fixture_cell", "cannot open synthetic installation", ""},
	{"fixture_prepare", "cannot prepare synthetic foreground service", ""},
	{"host_spawn", "cannot start synthetic foreground host", ""},
	{"cleanup_budget", "foreground cleanup exceeded synthetic budget", ""},
	{"readiness_budget", "foreground readiness exceeded synthetic budget on first start", "foreground readiness exceeded synthetic budget after restart"},
	{"readiness_allowance", "foreground host did not expose authenticated status within allowance on first start", "foreground host did not expose authenticated status within allowance after restart"},
	{"endpoint_binding", "foreground endpoint lost fresh process binding", ""},
	{"boot_binding_check", "foreground restart lost independent boot binding", ""},
	{"command_state", "foreground command did not preserve durable stopped state", ""},
	{"console_dispatch", "foreground CTRL_BREAK console dispatch unavailable", ""},
	{"shutdown_budget", "foreground shutdown exceeded synthetic budget on first start", "foreground shutdown exceeded synthetic budget after restart"},
	{"shutdown_allowance", "foreground console shutdown exceeded allowance on first start", "foreground console shutdown exceeded allowance after restart"},
	{"endpoint_retained", "foreground shutdown retained endpoint record", ""},
	{"pipe_name", "invalid synthetic pipe name", ""},
	{"pipe_retained", "foreground shutdown retained a pipe instance", ""},
	{"pipe_absence", "foreground closed pipe could not be classified absent", ""},
	{"lease_open", "cannot open stable foreground lease", ""},
	{"lease_retained", "foreground shutdown retained its lease", ""},
	{"build_source", "cannot locate harness source", ""},
	{"build_tool", "cannot locate Go tool", ""},
	{"build_cache", "cannot locate build caches", ""},
	{"build_harness", "cannot build synthetic foreground harness", ""},
}

func TestWindowsForegroundSubprocessDiagnosticSites(t *testing.T) {
	source, err := os.ReadFile("service_subprocess_windows_test.go")
	if err != nil {
		t.Fatal("cannot read Windows foreground diagnostic sites")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, row := range []struct{ name, source string }{
		{"lf", lf},
		{"crlf", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(row.name, func(t *testing.T) {
			assertForegroundSubprocessDiagnosticSites(t, row.source)
		})
	}
}

func assertForegroundSubprocessDiagnosticSites(t *testing.T, source string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "windows-harness", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		t.Fatal("cannot parse Windows foreground diagnostic sites")
	}
	for _, site := range foregroundSubprocessDiagnosticSites {
		t.Run(site.class, func(t *testing.T) {
			log := fmt.Sprintf(`t.Logf("foreground_exit class=%%s", %q)`, site.class)
			method := "Fatal"
			if site.class == "cleanup_budget" {
				method = "Error"
			}
			following := fmt.Sprintf("t.%s(%q)", method, site.assertion)
			if site.restart != "" {
				following = fmt.Sprintf("if iteration == 0 { t.Fatal(%q) }; t.Fatal(%q)", site.assertion, site.restart)
			}
			fixture, err := parser.ParseFile(token.NewFileSet(), "contract", "package main; func contract() {"+log+";"+following+"}", 0)
			if err != nil {
				t.Fatal("invalid Windows foreground diagnostic contract")
			}
			expected := fixture.Decls[0].(*ast.FuncDecl).Body
			matches := 0
			ast.Inspect(file, func(node ast.Node) bool {
				var statements []ast.Stmt
				switch node := node.(type) {
				case *ast.BlockStmt:
					statements = node.List
				case *ast.CommClause:
					statements = node.Body
				case *ast.CaseClause:
					statements = node.Body
				}
				for index, statement := range statements {
					if foregroundSource(t, statement) != log {
						continue
					}
					matches++
					end := index + len(expected.List)
					if end > len(statements) || foregroundSource(t, &ast.BlockStmt{List: statements[index:end]}) != foregroundSource(t, expected) {
						t.Error("foreground class must directly precede its unchanged failure assertion")
					}
				}
				return true
			})
			if matches != 1 {
				t.Error("foreground failure site must have exactly one unconditional fixed class line")
			}
		})
	}
}

func TestWindowsForegroundSubprocessClassEnrollment(t *testing.T) {
	var rows []foregroundClassEnrollment
	for _, site := range foregroundSubprocessDiagnosticSites {
		rows = append(rows, foregroundClassEnrollment{site.class, site.class, site.class, site.assertion, site.assertion})
	}
	// The cold install classifier already has a portable truth table. Exercise
	// its vocabulary through the foreground prefix as well as service_install.
	for _, class := range []string{"deadline", "parent_budget", "exit_zero", "exit_without_cause", "start_failed"} {
		const refusal = "Windows service install did not preserve adapter refusal"
		rows = append(rows, foregroundClassEnrollment{"install_" + class, class, class, refusal, refusal})
	}
	// Raw strings are deliberately outside the checker's vocabulary scan.
	rows = append(rows, foregroundClassEnrollment{"reject_unknown", `outsidevocabularyprivate`, "(rejected)", "", "(unclassified)"})
	assertForegroundClassEnrollments(t, rows)
}

func TestWindowsForegroundShortReceiptClassEnrollment(t *testing.T) {
	var rows []foregroundClassEnrollment
	for _, row := range []struct{ name, receipt, class string }{
		{"missing", "", "no_output"},
		{"malformed", "private /synthetic/home\n", "unclassified"},
		{"partial", "service_shutdown", "unclassified"},
	} {
		receipt := []byte(row.receipt)
		failure := foregroundShutdownFailure(receipt)
		if len(foregroundOutputLines(receipt)) > 1 || failure == "" {
			t.Fatal("short receipt fixture did not exercise an unclassified failure")
		}
		class := foregroundFailureClass(receipt)
		if class != row.class {
			t.Fatal("short receipt lost its closed foreground failure class")
		}
		// Inject the real dynamic fatal. The scanner does not enroll returned
		// error strings, but the independent class must remain visible.
		rows = append(rows, foregroundClassEnrollment{row.name, class, row.class, failure, "(unclassified)"})
	}
	assertForegroundClassEnrollments(t, rows)
}

type foregroundClassEnrollment struct {
	name, class, wantClass, assertion, wantWhy string
}

func assertForegroundClassEnrollments(t *testing.T, rows []foregroundClassEnrollment) {
	t.Helper()
	node := testenv.RequireNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const packageName = "github.com/witwave-ai/witself/internal/tools/localcellharness"
	const testName = "TestWindowsServiceForegroundSubprocessCommands"
	type event struct {
		Action  string
		Package string
		Test    string `json:",omitempty"`
		Output  string `json:",omitempty"`
	}
	events := []event{
		{Action: "start", Package: packageName},
		{Action: "run", Package: packageName, Test: testName},
	}
	for _, row := range rows {
		child := testName + "/" + row.name
		events = append(events,
			event{Action: "run", Package: packageName, Test: child},
			event{Action: "output", Package: packageName, Test: child, Output: "    service_subprocess_windows_test.go:1: foreground_exit class=" + row.class + "\r\n"})
		if row.assertion != "" {
			events = append(events, event{Action: "output", Package: packageName, Test: child, Output: "    service_subprocess_windows_test.go:2: " + row.assertion + "\r\n"})
		}
		events = append(events, event{Action: "fail", Package: packageName, Test: child})
	}
	events = append(events, event{Action: "fail", Package: packageName, Test: testName}, event{Action: "fail", Package: packageName})
	var input strings.Builder
	encoder := json.NewEncoder(&input)
	for _, item := range events {
		if encoder.Encode(item) != nil {
			t.Fatal("cannot encode foreground diagnostic fixture")
		}
	}
	command := exec.CommandContext(ctx, node, filepath.Join("..", "..", "..", "scripts", "ci-go-test-json-check.mjs"), "--package", packageName, "--require", testName)
	command.Stdin = strings.NewReader(input.String())
	command.WaitDelay = time.Second
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatal("foreground diagnostic checker did not report the synthetic failure")
	}
	// All rows share one checker launch; per-row subprocess startup must not
	// consume an aggregate fixed budget on a loaded Windows runner.
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			classes, classFound, whyFound := 0, false, false
			child := testName + "/" + row.name
			prefix := "class " + packageName + " " + child + " :: "
			for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
				if strings.HasPrefix(line, prefix) {
					classes++
				}
				classFound = classFound || line == prefix+"foreground_exit class="+row.wantClass
				whyFound = whyFound || line == "why "+packageName+" "+child+" :: "+row.wantWhy
			}
			if classes != 1 || !classFound || !whyFound || strings.Contains(string(output), "/synthetic/home") || (row.wantClass == "(rejected)" && strings.Contains(string(output), row.class)) {
				t.Fatal("foreground diagnostic enrollment lost its prefix or closed vocabulary")
			}
		})
	}
}
