package localcell_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// The output contract alone cannot detect a raising site bypassing the renderer.
// Read the actual sources, including the native lifecycle helper on every host.
func TestPostStartReadinessDiagnosticSourceContract(t *testing.T) {
	fatalSites, rendererDefinitions := 0, 0
	for _, name := range []string{"lifecycle_hermetic_test.go", "lifecycle_integration_test.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("cannot read readiness source %s", name)
		}
		source := strings.ReplaceAll(string(data), "\r\n", "\n")
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, name, source, 0)
		if err != nil {
			t.Fatalf("cannot parse readiness source %s", name)
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv == nil && function.Name.Name == "formatPostStartReadinessDiagnostic" {
				rendererDefinitions++
			}
		}
		rendererLines := make(map[int]bool)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			function, ok := call.Fun.(*ast.Ident)
			if !ok || function.Name != "formatPostStartReadinessDiagnostic" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			site, err := strconv.Unquote(literal.Value)
			if err == nil && (site == "concurrent_start" || site == "assert_ready") {
				rendererLines[positions.Position(call.Pos()).Line] = true
			}
			return true
		})
		lines := strings.Split(source, "\n")
		for index, line := range lines {
			if !strings.Contains(line, "both local readiness endpoints must be ready when start returns") {
				continue
			}
			fatalSites++
			found := false
			for previous, nonblank := index-1, 0; previous >= 0 && nonblank < 3; previous-- {
				if strings.TrimSpace(lines[previous]) == "" {
					continue
				}
				nonblank++
				found = found || rendererLines[previous+1]
			}
			if !found {
				t.Errorf("%s:%d: readiness fatal must call formatPostStartReadinessDiagnostic with site literal concurrent_start or assert_ready within the preceding 3 non-blank lines", name, index+1)
			}
		}
	}
	if fatalSites != 2 {
		t.Errorf("readiness fatal site count = %d, want exactly 2", fatalSites)
	}
	if rendererDefinitions != 1 {
		t.Errorf("formatPostStartReadinessDiagnostic definition count = %d, want exactly 1", rendererDefinitions)
	}
}

func TestPostStartReadinessDiagnosticContract(t *testing.T) {
	allowed := regexp.MustCompile(`^[A-Za-z0-9_=. :-]+$`)
	longDigits := regexp.MustCompile(`[0-9]{7,}`)
	check := func(line, want string) error {
		if strings.Contains(line, "/") || longDigits.MatchString(line) || !allowed.MatchString(line) {
			return errors.New("readiness diagnostic violates its value-free alphabet or digit bound")
		}
		fields := strings.Fields(line)
		for _, wantField := range strings.Fields(want) {
			key, _, _ := strings.Cut(wantField, "=")
			found := false
			for _, field := range fields {
				if strings.HasPrefix(field, key+"=") {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("readiness diagnostic missing key %q", key)
			}
		}
		switch fields[0] {
		case "site=concurrent_start", "site=assert_ready":
		default:
			return errors.New("readiness diagnostic site must be first and one of the two closed literals")
		}
		if line != want {
			return errors.New("readiness diagnostic contains unexpected fields, values, or ordering")
		}
		return nil
	}
	for _, site := range []struct {
		name      string
		rootIndex int
	}{
		{"concurrent_start", 1},
		{"assert_ready", -1},
	} {
		t.Run(site.name, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				elapsed     time.Duration
				status      localcell.Status
				err         error
				contextDone bool
				want        string
			}{
				{
					name:    "mixed flags",
					elapsed: 1234 * time.Nanosecond,
					status: localcell.Status{
						Inspection: localcell.Inspection{
							Manifest: &localcell.Manifest{Database: "/private/manifest", AccountID: strings.Repeat("a", 64)},
							Processes: map[string]localcell.ProcessState{
								"server": {PID: 1234567, Alive: true, Matches: false},
								"worker": {PID: 7654321, Alive: false, Matches: true},
							},
						},
						ServerReady: false, WorkerReady: true,
					},
					contextDone: true,
					want:        "root_index=1 probe_elapsed=0.000001s status_error=false manifest_present=true server_ready=false worker_ready=true server_alive=true server_matches=false worker_alive=false worker_matches=true context_done=true",
				},
				{
					name:    "path-like error and inverted flags",
					elapsed: 780 * time.Millisecond,
					status: localcell.Status{
						Inspection: localcell.Inspection{
							Processes: map[string]localcell.ProcessState{
								"server": {Alive: false, Matches: true},
								"worker": {Alive: true, Matches: false},
							},
						},
						ServerReady: true, WorkerReady: false,
					},
					err:  errors.New("/private/status/123456789/" + strings.Repeat("b", 64)),
					want: "root_index=1 probe_elapsed=0.780000s status_error=true manifest_present=false server_ready=true worker_ready=false server_alive=false server_matches=true worker_alive=true worker_matches=false context_done=false",
				},
				{
					name:    "missing process entries",
					elapsed: time.Nanosecond,
					want:    "root_index=1 probe_elapsed=0.000000s status_error=false manifest_present=false server_ready=false worker_ready=false server_alive=false server_matches=false worker_alive=false worker_matches=false context_done=false",
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					want := "site=" + site.name + " " + strings.Replace(tc.want, "root_index=1", fmt.Sprintf("root_index=%d", site.rootIndex), 1)
					line := formatPostStartReadinessDiagnostic(site.name, site.rootIndex, tc.elapsed, tc.status, tc.err, tc.contextDone)
					if err := check(line, want); err != nil {
						t.Fatal(err)
					}
					for index, field := range strings.Fields(want) {
						key, _, _ := strings.Cut(field, "=")
						t.Run("rejects missing "+key, func(t *testing.T) {
							fields := strings.Fields(want)
							missing := strings.Join(append(fields[:index], fields[index+1:]...), " ")
							err := check(missing, want)
							if err == nil || err.Error() != fmt.Sprintf("readiness diagnostic missing key %q", key) {
								t.Fatalf("contract did not reject missing key %q", key)
							}
						})
					}
					t.Run("rejects third site literal", func(t *testing.T) {
						third := formatPostStartReadinessDiagnostic("third_site", site.rootIndex, tc.elapsed, tc.status, tc.err, tc.contextDone)
						err := check(third, want)
						if err == nil || err.Error() != "readiness diagnostic site must be first and one of the two closed literals" {
							t.Fatal("contract did not reject a third site literal")
						}
					})
				})
			}
		})
	}
}
