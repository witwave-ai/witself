package localcell

import (
	"bytes"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPublishErrorKeepsTextAndCause(t *testing.T) {
	cause := errors.New("synthetic private cause")
	var err error = &publishError{cause: cause}
	if err.Error() != "cannot publish local cell record" || !errors.Is(err, cause) {
		t.Fatal("publication error lost its public text or private cause")
	}
	source := leasePublishReadSource(t, "files.go")
	f, err := parser.ParseFile(token.NewFileSet(), "files.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var literals []string
	for _, declaration := range f.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "writeAtomicBytes" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if literal, ok := n.(*ast.BasicLit); ok && literal.Kind == token.STRING {
				value, unquoteErr := strconv.Unquote(literal.Value)
				if unquoteErr != nil {
					t.Fatal(unquoteErr)
				}
				literals = append(literals, value)
			}
			return true
		})
	}
	// Every remaining string literal in writeAtomicBytes is unchanged. Only
	// the final publication error now carries the underlying platform cause.
	// "credentials" is the per-role credential directory whose files share the
	// password file's pre-write hook (selected Local Cell principals).
	want := []string{"local cell record must be a regular file", "cannot create local cell record", "postgres.password", "credentials", "cannot persist local cell record"}
	if !slices.Equal(literals, want) {
		t.Fatal("writeAtomicBytes changed an unrelated literal")
	}
	if !strings.Contains(source, "if err := publishPrivateFile(f.Name(), path); err != nil {\n\t\treturn &publishError{cause: err}\n\t}") {
		t.Fatal("writeAtomicBytes discarded the publication cause")
	}
}

func TestRetryPublishEngine(t *testing.T) {
	transient := errors.New("synthetic transient")
	permanent := errors.New("synthetic refusal")
	for _, test := range []struct {
		name       string
		failures   int
		failure    error
		wantCount  int
		wantSleeps []time.Duration
		wantErr    error
	}{
		{"eventual_success", 3, transient, 4, []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}, nil},
		{"budget_exhausted", 99, transient, 14, []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}, transient},
		{"permanent", 1, permanent, 1, nil, permanent},
		{"first_success", 0, nil, 1, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(0, 0)
			var sleeps []time.Duration
			var lastFailure error
			calls := 0
			wallStart := time.Now()
			attempts, err := retryPublish(func() error {
				calls++
				// Keep a broken budget or sleeper mutant finite without
				// letting that fuse masquerade as the expected last error.
				if calls > 16 {
					return permanent
				}
				if calls <= test.failures {
					lastFailure = &publishError{cause: test.failure}
					return lastFailure
				}
				return nil
			}, func(err error) bool { return errors.Is(err, transient) }, leasePublishHooks{
				now: func() time.Time { return now },
				sleep: func(delay time.Duration) {
					sleeps = append(sleeps, delay)
					now = now.Add(delay)
				},
			})
			if time.Since(wallStart) > serviceLeasePublishBudget/2 {
				t.Fatal("retry engine bypassed the injected sleeper's fixture-proportional bound")
			}
			if calls != test.wantCount || attempts != calls || !errors.Is(err, test.wantErr) || !slices.Equal(sleeps, test.wantSleeps) {
				t.Fatalf("retry oracle mismatch: calls=%d attempts=%d sleeps=%v", calls, attempts, sleeps)
			}
			if test.wantErr != nil && err != lastFailure {
				t.Fatal("retry engine did not return the final attempt's error")
			}
		})
	}
}

func TestLeasePublishDiagnosticLine(t *testing.T) {
	for _, class := range []string{"sharing_violation", "access_denied", "validation", "other", "none", "synthetic private cause"} {
		var output bytes.Buffer
		writeLeasePublishDiagnostic(&output, class, 4, 175*time.Millisecond)
		wantClass := class
		if class == "none" || class == "synthetic private cause" {
			wantClass = "other"
		}
		want := "native service host lease_publish class=" + wantClass + " attempts=4 elapsed_ms=175\n"
		if output.String() != want {
			t.Fatal("lease publication diagnostic escaped its closed vocabulary")
		}
	}
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			cause := errors.New("synthetic private cause")
			oldHooks := serviceLeasePublishTestHooks
			t.Cleanup(func() { serviceLeasePublishTestHooks = oldHooks })
			calls, sleeps := 0, 0
			serviceLeasePublishTestHooks = leasePublishHooks{
				write: func(string, any, func() error) error {
					calls++
					if fail {
						return &publishError{cause: cause}
					}
					return nil
				},
				sleep: func(time.Duration) { sleeps++ },
				now:   func() time.Time { return time.Unix(0, 0) },
			}
			output := leasePublishCaptureStderr(t, func() {
				err := new(Cell).publishServiceLease("synthetic-lease", serviceLeaseRecord{})
				if fail && !errors.Is(err, cause) || !fail && err != nil {
					t.Error("publisher changed the engine result")
				}
				WriteServiceHostFailure(os.Stderr, serviceFailureAt(err, "lease_write"), "unknown")
			})
			want := ""
			if fail {
				want = "native service host lease_publish class=other attempts=1 elapsed_ms=0\nnative service host site=lease_write\n"
			}
			if output != want || strings.Contains(output, cause.Error()) || calls != 1 || sleeps != 0 {
				t.Fatal("publisher diagnostic must be value-free, failure-only, and before the site receipt")
			}
		})
	}
}

func leasePublishCaptureStderr(t *testing.T, run func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal("cannot create diagnostic capture")
	}
	t.Cleanup(func() { _ = f.Close() })
	previous := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = previous }()
	run()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal("cannot read diagnostic capture")
	}
	return string(data)
}

func leasePublishReadSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal("cannot read lease publisher source")
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func TestPublishServiceLeaseSourceContract(t *testing.T) {
	sources := make(map[string]string)
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.HasSuffix(path, "_test.go") {
			sources[path] = leasePublishReadSource(t, path)
		}
	}
	sources["windows_private_class_other.go"] = leasePublishReadSource(t, "../runtimeconfig/windows_private_class_other.go")
	if err := leasePublishSourceContract(sources); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ name, file, before, after string }{
		{"retry_every_error", "service_lease_publish.go", "!retryable(err) || ", ""},
		{"budget_zero", "service_lease_publish.go", "2 * time.Second", "0 * time.Second"},
		{"unbounded_budget", "service_lease_publish.go", "h.now().Sub(start) >= serviceLeasePublishBudget", "false"},
		{"predicate_true", "service_lease_publish.go", "func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }", "func(error) bool { return true }"},
		{"production_hook", "service_lease_publish.go", "var serviceLeasePublishTestHooks leasePublishHooks", "var serviceLeasePublishTestHooks = leasePublishHooks{sleep: time.Sleep}"},
		{"stub_true", "windows_private_class_other.go", "return false", "return true"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			copySources := make(map[string]string, len(sources))
			for name, source := range sources {
				copySources[name] = source
			}
			original := copySources[mutation.file]
			if strings.Count(original, mutation.before) != 1 {
				t.Fatal("source contract mutation anchor is not unique")
			}
			copySources[mutation.file] = strings.Replace(original, mutation.before, mutation.after, 1)
			if leasePublishSourceContract(copySources) == nil {
				t.Fatal("source contract accepted mutation")
			}
		})
	}
	if runtime.GOOS != "windows" {
		// Real non-Windows publication stays single-shot; the Windows predicate
		// is covered by the source contract and native classifier test.
		t.Run("production_hooks_nil", func(t *testing.T) {
			if !reflect.ValueOf(serviceLeasePublishTestHooks).IsZero() {
				t.Fatal("production hooks are not nil")
			}
		})
	}
}

func leasePublishSourceContract(sources map[string]string) error {
	fail := errors.New("lease publisher source contract changed")
	const expected = `package localcell
const (
 serviceLeasePublishBudget = 2 * time.Second
 serviceLeasePublishInitialBackoff = 25 * time.Millisecond
 serviceLeasePublishMaxBackoff = 200 * time.Millisecond
)
func (h leasePublishHooks) defaults() leasePublishHooks {
 if h.write == nil { h.write = writeAtomic }
 if h.sleep == nil { h.sleep = time.Sleep }
 if h.now == nil { h.now = time.Now }
 return h
}
func retryPublish(attempt func() error, retryable func(error) bool, h leasePublishHooks) (attempts int, err error) {
 h = h.defaults()
 start := h.now()
 backoff := serviceLeasePublishInitialBackoff
 for {
  attempts++
  err = attempt()
  if err == nil || !retryable(err) || h.now().Sub(start) >= serviceLeasePublishBudget { return attempts, err }
  h.sleep(backoff)
  backoff = min(backoff*2, serviceLeasePublishMaxBackoff)
 }
}
func (c *Cell) publishServiceLease(path string, record serviceLeaseRecord) error {
 h := serviceLeasePublishTestHooks.defaults()
 start := h.now()
 attempts, err := retryPublish(func() error { return h.write(path, record, nil) }, func(err error) bool { return runtimeconfig.WindowsPrivateTransient(err) }, h)
 if err != nil { writeLeasePublishDiagnostic(os.Stderr, runtimeconfig.WindowsPrivateFailureClass(err), attempts, h.now().Sub(start)) }
 return err
}`
	wanted := leasePublishDeclarations(expected)
	actual := leasePublishDeclarations(sources["service_lease_publish.go"])
	for name, declaration := range wanted {
		if actual[name] != declaration {
			return fail
		}
	}
	if len(wanted) != 6 || !strings.Contains(sources["service_foreground.go"], `c.publishServiceLease(c.path("service", "lease.json"), record)`) {
		return fail
	}
	stub := leasePublishDeclarations(sources["windows_private_class_other.go"])
	wantStub := leasePublishDeclarations(`package runtimeconfig
func WindowsPrivateTransient(err error) bool { return false }
func WindowsPrivateFailureClass(err error) string { return "none" }
`)
	// Parameter names are irrelevant; pin the stub bodies separately.
	for _, name := range []string{"WindowsPrivateTransient", "WindowsPrivateFailureClass"} {
		body := func(s string) string { _, tail, _ := strings.Cut(s, "{"); return tail }
		if body(stub[name]) == "" || body(stub[name]) != body(wantStub[name]) {
			return fail
		}
	}
	declarations := 0
	for _, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), "production.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			return fail
		}
		invalid := false
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.ValueSpec:
				for _, name := range node.Names {
					if name.Name == "serviceLeasePublishTestHooks" {
						declarations++
						kind, ok := node.Type.(*ast.Ident)
						invalid = invalid || len(node.Values) != 0 || !ok || kind.Name != "leasePublishHooks"
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					ast.Inspect(lhs, func(part ast.Node) bool {
						if name, ok := part.(*ast.Ident); ok && name.Name == "serviceLeasePublishTestHooks" {
							invalid = true
						}
						return true
					})
				}
			}
			return true
		})
		if invalid {
			return fail
		}
	}
	if declarations != 1 {
		return fail
	}
	return nil
}

func leasePublishDeclarations(source string) map[string]string {
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, "publisher.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return nil
	}
	result := make(map[string]string)
	for _, declaration := range file.Decls {
		var nodes []struct {
			name string
			node ast.Node
		}
		switch declaration := declaration.(type) {
		case *ast.FuncDecl:
			nodes = append(nodes, struct {
				name string
				node ast.Node
			}{declaration.Name.Name, declaration})
		case *ast.GenDecl:
			if declaration.Tok == token.CONST {
				for _, spec := range declaration.Specs {
					value := spec.(*ast.ValueSpec)
					nodes = append(nodes, struct {
						name string
						node ast.Node
					}{value.Names[0].Name, value})
				}
			}
		}
		for _, item := range nodes {
			var formatted bytes.Buffer
			if format.Node(&formatted, token.NewFileSet(), item.node) != nil {
				return nil
			}
			result[item.name] = strings.Join(strings.Fields(formatted.String()), " ")
		}
	}
	return result
}
