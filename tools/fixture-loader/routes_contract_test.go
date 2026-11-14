package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type recordingRouteReporter struct{ logs, fatals []string }

func (*recordingRouteReporter) Helper() {}
func (r *recordingRouteReporter) Logf(f string, a ...any) {
	r.logs = append(r.logs, fmt.Sprintf(f, a...))
}
func (r *recordingRouteReporter) Fatal(a ...any) { r.fatals = append(r.fatals, fmt.Sprint(a...)) }

func TestRouteDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, auth, classes string
		allowed                           bool
	}{
		{"unknown", "GET", "/private-route", "", "GET unknown none false", false},
		{"redirect", "GET", "/redirect-target", "Bearer " + fakeAgentToken, "GET redirect_target agent false", false},
		{"other_account", "GET", "/v1/directory/acc_bbbbbbbbbbbbbbbb", "", "GET directory_other_account none false", false},
		{"foreign_allowed", "GET", "/v1/account", "private-header", "GET read_account foreign false", true},
		{"other_method", "DELETE", "/v1/account", "Bearer " + fakeOtherToken, "other read_account other_fixture false", false},
		{"empty_header", "GET", "/private-route", "", "GET unknown foreign false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCell(t)
			req, err := http.NewRequest(tc.method, f.server.URL+tc.path, nil)
			if err != nil {
				t.Fatal("cannot build injection")
			}
			if tc.auth != "" || tc.name == "empty_header" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, err := newHTTPClient().Do(req)
			if err != nil {
				t.Fatal("injection failed")
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			f.mu.Lock()
			requests := append([]fakeRequest(nil), f.requests...)
			f.mu.Unlock()
			if len(requests) != 1 {
				t.Fatal("injection was not recorded exactly once")
			}
			r := requests[0]
			if fmt.Sprintf("%s %s %s %s", r.MethodClass, r.RouteClass, r.CredentialClass, r.InRun) != tc.classes {
				t.Fatal("arrival classification differs")
			}
			reporter := &recordingRouteReporter{}
			assertRoutes(reporter, f)
			if tc.allowed {
				if len(reporter.logs)+len(reporter.fatals) != 0 {
					t.Fatal("on-list route rejected")
				}
				return
			}
			c := strings.Fields(tc.classes)
			want := fmt.Sprintf("off_list_request index=0 of=1 method=%s route=%s credential=%s in_run=%s", c[0], c[1], c[2], c[3])
			if len(reporter.logs) != 1 || reporter.logs[0] != want || len(reporter.fatals) != 1 || reporter.fatals[0] != "HTTP request fell outside the closed route list" {
				t.Fatal("route refusal violated exact diagnostic or fatal contract")
			}
		})
	}
}

func TestRouteRunWindow(t *testing.T) {
	h := newHarness(t, 10)
	code, _, stderr := h.run(h.args("mark", "--yes")...)
	assertResult(t, code, 0, stderr, "")
	h.cell.mu.Lock()
	defer h.cell.mu.Unlock()
	if h.cell.inRun || len(h.cell.requests) == 0 {
		t.Fatal("run window was not cleared or exercised")
	}
	for _, r := range h.cell.requests {
		if r.InRun != classTrue {
			t.Fatal("command request was recorded outside its run")
		}
	}
}

func TestRouteDiagnosticSource(t *testing.T) {
	vocabulary := `"GET", "POST", "other", "directory_lookup", "close_account", "read_self", "read_account", "list_transcripts", "create_transcript", "append_entries", "read_transcript_tail", "export", "redirect_target", "directory_other_account", "unknown", "agent", "operator", "other_fixture", "none", "foreign", "true", "false"`
	wantLog := `t.Logf("off_list_request index=%d of=%d method=%s route=%s credential=%s in_run=%s", i, len(requests), r.MethodClass.String(), r.RouteClass.String(), r.CredentialClass.String(), r.InRun.String())`
	found := 0
	for _, name := range []string{"fakecell_test.go", "load_test.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("diagnostic source unavailable")
		}
		fs := token.NewFileSet()
		file, err := parser.ParseFile(fs, name, strings.ReplaceAll(string(data), "\r\n", "\n"), 0)
		if err != nil {
			t.Fatal("diagnostic source invalid")
		}
		render := func(n ast.Node) string {
			var b bytes.Buffer
			if format.Node(&b, fs, n) != nil {
				t.Fatal("cannot format source")
			}
			return b.String()
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "String" && fn.Recv != nil && render(fn.Recv.List[0].Type) == "requestClass" {
				found++
				if render(fn.Body) != "{\n\treturn [...]string{"+vocabulary+"}[c]\n}" {
					t.Fatal("class formatter no longer contains only closed labels")
				}
			}
			if fn.Name.Name == "assertRoutes" {
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if ok {
						if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Logf" {
							found++
							if render(call) != wantLog {
								t.Fatal("diagnostic includes more than closed classes and decimal counts")
							}
						}
					}
					return true
				})
			}
		}
	}
	if found != 2 {
		t.Fatal("diagnostic formatter or single log missing")
	}
}
