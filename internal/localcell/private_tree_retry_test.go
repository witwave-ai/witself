package localcell

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// FLAKE-63: validateRoot's tree walk is one attempt of the shared bounded retry; a fake
// walk refuses through the seam and a fake clock bounds each row by the fixture's budget.
func TestValidateRootTreeRetry(t *testing.T) {
	transient, refusal := errors.New("synthetic transient tree refusal"), errors.New("synthetic tree validation refusal")
	synthetic := func(err error) bool { return errors.Is(err, transient) }
	// failures: walks refused before one passes (-1: all); walks: 0 means until the budget.
	for _, test := range []struct {
		name            string
		failures, walks int
		failure         error
		retryable       func(error) bool
	}{
		{"released", 3, 4, transient, synthetic},
		{"held", -1, 0, transient, synthetic},
		{"refusal", -1, 1, refusal, synthetic},
		{"foreign", -1, 1, transient, runtimeconfig.WindowsPrivateTransient},
	} {
		t.Run(test.name, func(t *testing.T) {
			now, walks, slept := time.Unix(0, 0), 0, time.Duration(0)
			var sleeps []time.Duration
			var last error
			wallStart := time.Now()
			err := validateRootTree("synthetic-root", func(root string) error {
				walks++
				// A retry that ignores its budget ends here, with an error no row expects.
				if walks > 16 || root != "synthetic-root" {
					return errors.New("synthetic tree retry fuse")
				}
				if test.failures >= 0 && walks > test.failures {
					return nil
				}
				last = fmt.Errorf("%w", test.failure)
				return last
			}, test.retryable, leasePublishHooks{now: func() time.Time { return now }, sleep: func(delay time.Duration) {
				sleeps, now, slept = append(sleeps, delay), now.Add(delay), slept+delay
			}})
			if time.Since(wallStart) > serviceLeasePublishBudget/2 {
				t.Fatal("tree walk retry slept outside the injected sleeper")
			}
			if len(sleeps) != walks-1 {
				t.Fatal("tree walk retry did not sleep once between walks")
			}
			if test.walks != 0 && walks != test.walks {
				t.Fatalf("tree walk retry walked another number of times: %d", walks)
			}
			if test.walks == 0 && slept < serviceLeasePublishBudget {
				t.Fatal("tree walk retry ended before its budget")
			}
			if test.walks == 0 && slept-sleeps[len(sleeps)-1] >= serviceLeasePublishBudget {
				t.Fatal("tree walk retry continued past its budget")
			}
			if test.failures >= 0 {
				if err != nil {
					t.Fatal("tree walk retry failed after a walk passed")
				}
				return
			}
			if err != last {
				t.Fatal("tree walk retry did not return the last walk's error")
			}
			if err.Error() != test.failure.Error() {
				t.Fatal("tree walk retry changed the refusal text")
			}
			if runtimeconfig.WindowsPrivateFailureClass(err) != runtimeconfig.WindowsPrivateFailureClass(test.failure) {
				t.Fatal("tree walk retry changed the refusal class")
			}
		})
	}
}

// The boundary: validateRoot's first statement (production walk, predicate, clock) and validateRootTree.
const validateRootTreeSource = "package localcell\nfunc (c *Cell) validateRoot() {\n" +
	"if err := validateRootTree(c.cfg.Root, validatePrivateTree, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{}); err != nil { return err } }\n" +
	"func validateRootTree() { _, err := retryPublish(func() error { return validate(root) }, retryable, h); return err }\n"

func TestValidateRootTreeSourceContract(t *testing.T) {
	source := leasePublishReadSource(t, "files.go")
	want := validateRootTreeParts(validateRootTreeSource)
	if want == "" || validateRootTreeParts(source) != want {
		t.Fatal("tree walk retry boundary changed in files.go")
	}
	if validateRootTreeParts(strings.ReplaceAll(source, "\n", "\r\n")) != want {
		t.Fatal("tree walk retry contract refused a CRLF checkout")
	}
	for _, mutation := range []struct{ name, before, after string }{
		{"old_form", "if err := validateRootTree(c.cfg.Root, validatePrivateTree, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{}); err != nil {", "if err := validatePrivateTree(c.cfg.Root); err != nil {"},
		{"any_error", "validatePrivateTree, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{})", "validatePrivateTree, func(err error) bool { return err != nil }, leasePublishHooks{})"},
		{"not_there", "leasePublishHooks{}); err != nil {\n\t\treturn err\n\t}", "leasePublishHooks{}); runtimeconfig.WindowsPrivateTransient(err) {\n\t\treturn os.ErrNotExist\n\t} else if err != nil {\n\t\treturn err\n\t}"},
		{"forever", "_, err := retryPublish(func() error { return validate(root) }, retryable, h)", "err := validate(root)\n\tfor err != nil && retryable(err) {\n\t\terr = validate(root)\n\t}"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(source, mutation.before) != 1 {
				t.Fatal("tree walk retry mutation anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				if validateRootTreeParts(strings.ReplaceAll(strings.Replace(source, mutation.before, mutation.after, 1), "\n", ending)) == want {
					t.Fatal("tree walk retry contract accepted a mutation")
				}
			}
		})
	}
}

// validateRootTreeParts returns both parts whitespace-normalized, or "" when either is missing.
func validateRootTreeParts(source string) string {
	file, err := parser.ParseFile(token.NewFileSet(), "files.go", strings.ReplaceAll(source, "\r\n", "\n"), 0)
	if err != nil {
		return ""
	}
	var statement, body string
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil && len(function.Body.List) != 0 {
			switch {
			case function.Name.Name == "validateRoot" && function.Recv != nil:
				statement = serviceBootPublicationText(function.Body.List[0])
			case function.Name.Name == "validateRootTree" && function.Recv == nil:
				body = serviceBootPublicationText(function.Body)
			}
		}
	}
	if statement == "" || body == "" {
		return ""
	}
	return statement + "\n" + body
}
