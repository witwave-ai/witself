package localcell

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/runtimeconfig"
)

// FLAKE-63B. A walker's fixture returns its root and one run; outcome (nil: the same error) judges
// the result against the last walk's error, nil once a walk passed. Windows adds its own reader.
type treeWalker struct {
	name    string
	setup   func(*testing.T) (string, func() error)
	outcome func(err, last error) bool
}

var treeWalkers = []treeWalker{
	{"validate_root", func(t *testing.T) (string, func() error) {
		c, _ := unitCell(t)
		return c.cfg.Root, func() error { return c.validateRoot(false) }
	}, nil},
	{"acquire_lock", treeLockWalker(true), nil},
	{"probe_lock", treeLockWalker(false), nil}, // no owner lock: only a passed walk may find it absent
}

// A refused acquisition holds and creates nothing; one that passed holds a lock only if it creates.
func treeLockWalker(create bool) func(*testing.T) (string, func() error) {
	return func(t *testing.T) (string, func() error) {
		c, _ := unitCell(t)
		return c.cfg.Root, func() error {
			lock, err := acquireLock(context.Background(), c.cfg.Root, create)
			if _, owner := os.Lstat(c.path(".owner.lock")); (lock != nil) != (create && err == nil) || err != nil && owner == nil || lock.Close() != nil {
				t.Fatal("a lock acquisition held or created a lock against its result")
			}
			return err
		}
	}
}

// Each walker through the seam (the retry over a fake walk, predicate and clock): refused three times
// then passing; refused past the budget (one budget, the last error); non-transient (the snapshot's
// churn) or a cut, one walk.
func TestPrivateTreeWalkersRetry(t *testing.T) {
	transient := errors.New("synthetic transient tree refusal")
	for _, walker := range treeWalkers {
		for _, row := range []struct {
			name            string
			failures, walks int // failures: refused walks before one passes (-1: all); walks 0: one budget
			failure         error
		}{{"released", 3, 4, transient}, {"held", -1, 0, transient},
			{"refusal", -1, 1, fmt.Errorf("synthetic tree churn refusal: %w", fs.ErrNotExist)}, {"cut", -1, 1, context.Canceled}} {
			t.Run(walker.name+"/"+row.name, func(t *testing.T) {
				root, run := walker.setup(t)
				calls, walks, now, slept, sleeps, last, previous := 0, 0, time.Unix(0, 0), time.Duration(0), []time.Duration{}, error(nil), validatePrivateTreeRetried
				t.Cleanup(func() { validatePrivateTreeRetried = previous })
				validatePrivateTreeRetried = func(walked string) error {
					calls++
					return validateRootTree(walked, func(walked string) error {
						if walks++; walks > 64 || walked != root { // a runaway retry, or another root
							return errors.New("synthetic tree walker fuse")
						}
						if last = nil; row.failures < 0 || walks <= row.failures {
							last = fmt.Errorf("%w", row.failure)
						}
						return last
					}, func(err error) bool { return errors.Is(err, transient) || runtimeconfig.WindowsPrivateTransient(err) },
						leasePublishHooks{now: func() time.Time { return now }, sleep: func(delay time.Duration) { sleeps, now, slept = append(sleeps, delay), now.Add(delay), slept+delay }})
				}
				switch err := run(); {
				case calls != 1 || len(sleeps) != walks-1 || row.walks != 0 && walks != row.walks:
					t.Fatalf("a tree walker crossed its boundary %d times for %d walks", calls, walks)
				case row.walks == 0 && (slept < serviceLeasePublishBudget || slept-sleeps[len(sleeps)-1] >= serviceLeasePublishBudget):
					t.Fatal("a walk held past the budget did not cost its walker one budget")
				case walker.outcome == nil && err != last || walker.outcome != nil && !walker.outcome(err, last):
					t.Fatal("a tree walker changed what its walk's outcome maps to")
				}
			})
		}
	}
}

// privateTreeWalkerContract names every walk outside the boundary and every retry around a walker or in the walk.
func privateTreeWalkerContract(sources map[string]string) []string {
	walker, failures := regexp.MustCompile(`\b(validatePrivateTree|validatePrivateTreeRetried|validateRoot|acquireLock|readShutdownDescriptor)\b`), []string{}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(sources[name], "\r\n", "\n"), 0)
		if err != nil {
			return []string{"parse:" + name}
		}
		for _, declaration := range file.Decls {
			referencesDeclarations(declaration, func(owner string, part ast.Node) {
				site := name + ":" + owner
				ast.Inspect(part, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok && site != "files.go:validatePrivateTreeRetried" && slices.Contains([]string{"retryPublish",
						"publishAtomic", "retryCheckpointStep", "validateRootTree", "retryServiceContention", "writeAtomic", "writeAtomicBytes"}, types.ExprString(call.Fun)) &&
						(owner == "validatePrivateTree" || slices.ContainsFunc(call.Args, func(argument ast.Expr) bool {
							return walker.MatchString(serviceBootPublicationText(argument))
						})) {
						failures = append(failures, "nested:"+site)
					} else if word, ok := node.(*ast.Ident); ok && word.Name == "validatePrivateTree" && owner != "validatePrivateTree" && site != "files.go:validatePrivateTreeRetried" {
						failures = append(failures, "direct:"+site)
					}
					return true
				})
			})
		}
	}
	slices.Sort(failures)
	return slices.Compact(failures)
}

func TestPrivateTreeWalkerSourceContract(t *testing.T) {
	sources := atomicPublicationSources(t)
	if failures := privateTreeWalkerContract(sources); len(failures) != 0 {
		t.Fatalf("private tree walker boundary changed: %v", failures)
	}
	for _, mutation := range []struct{ name, file, before, after, want string }{
		{"validate_root_direct", "files.go", "if err := validatePrivateTreeRetried(c.cfg.Root); err != nil {", "if err := validatePrivateTree(c.cfg.Root); err != nil {", "direct:files.go:(*Cell).validateRoot"},
		{"acquire_lock_direct", "lock.go", "if err := validatePrivateTreeRetried(root); err != nil {", "if err := validatePrivateTree(root); err != nil {", "direct:lock.go:acquireLock"},
		{"shutdown_descriptor_direct", "service_shutdown_windows.go", "validatePrivateTreeRetried(root) != nil", "validatePrivateTree(root) != nil", "direct:service_shutdown_windows.go:readShutdownDescriptor"},
		{"nested_around", "files.go", "if err := validatePrivateTreeRetried(c.cfg.Root); err != nil {", "if err := validateRootTree(c.cfg.Root, validatePrivateTreeRetried, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{}); err != nil {", "nested:files.go:(*Cell).validateRoot"},
		{"nested_inside", "files_windows.go", "if err := validateRootPath(root); err != nil {", "if _, err := retryPublish(func() error { return validateRootPath(root) }, runtimeconfig.WindowsPrivateTransient, leasePublishHooks{}); err != nil {", "nested:files_windows.go:validatePrivateTree"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if strings.Count(sources[mutation.file], mutation.before) != 1 {
				t.Fatal("private tree walker mutation anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				mutated := maps.Clone(sources)
				mutated[mutation.file] = strings.ReplaceAll(strings.Replace(sources[mutation.file], mutation.before, mutation.after, 1), "\n", ending)
				if !slices.Contains(privateTreeWalkerContract(mutated), mutation.want) {
					t.Fatal("private tree walker contract accepted a mutation")
				}
			}
		})
	}
}
