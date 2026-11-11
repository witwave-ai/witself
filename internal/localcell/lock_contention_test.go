package localcell

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestOwnershipFailureProbe(t *testing.T) {
	original := errors.New("ownership probe original refusal")
	alreadyLocked := fmt.Errorf("x: %w", ErrLocked)
	unrelated := errors.New("ownership probe unrelated refusal")
	for _, test := range []struct {
		name          string
		original      error
		directoryErr  error
		openErr       error
		lockErr       error
		unlockErr     error
		want          error
		calls         []string
		resultFailure string
	}{
		{"nil_original", nil, nil, nil, ErrLocked, nil, nil, nil, "nil ownership refusal became an error"},
		{"already_locked", alreadyLocked, nil, nil, ErrLocked, nil, alreadyLocked, nil, "existing wrapped ownership refusal changed identity"},
		{"directory_refused", original, unrelated, nil, nil, nil, original, []string{"directory"}, "directory refusal changed original error identity"},
		{"open_refused", original, nil, unrelated, nil, nil, original, []string{"directory", "open"}, "open refusal changed original error identity"},
		{"owner_missing", original, nil, os.ErrNotExist, nil, nil, original, []string{"directory", "open"}, "missing owner changed original error identity"},
		{"held", original, nil, nil, ErrLocked, nil, ErrLocked, []string{"directory", "open", "lock"}, "held owner did not return bare ErrLocked"},
		{"held_wrapped", original, nil, nil, alreadyLocked, nil, ErrLocked, []string{"directory", "open", "lock"}, "wrapped held owner did not return bare ErrLocked"},
		{"lock_error", original, nil, nil, unrelated, nil, original, []string{"directory", "open", "lock"}, "unrelated lock refusal changed original error identity"},
		{"free", original, nil, nil, nil, nil, original, []string{"directory", "open", "lock", "unlock"}, "free owner changed original error identity"},
		{"unlock_error", original, nil, nil, nil, unrelated, original, []string{"directory", "open", "lock", "unlock"}, "unlock refusal changed original error identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			file, err := os.CreateTemp(t.TempDir(), "probe-file")
			if err != nil {
				t.Fatal("cannot create ownership probe fake file")
			}
			t.Cleanup(func() { _ = file.Close() })
			var calls []string
			opened := false
			checkDir := func(path string) error {
				calls = append(calls, "directory")
				if path != root {
					t.Fatal("ownership probe checked the wrong directory")
				}
				return test.directoryErr
			}
			open := func(path string, create bool) (*os.File, error) {
				calls = append(calls, "open")
				if path != filepath.Join(root, ".owner.lock") {
					t.Fatal("ownership probe opened the wrong pathname")
				}
				if create {
					t.Fatal("ownership probe requested file creation")
				}
				if test.openErr != nil {
					return nil, test.openErr
				}
				opened = true
				return file, nil
			}
			tryLock := func(got *os.File) error {
				calls = append(calls, "lock")
				if got != file {
					t.Fatal("ownership probe locked a different file")
				}
				if _, err := file.Stat(); err != nil {
					t.Fatal("ownership probe closed the file before locking")
				}
				return test.lockErr
			}
			unlock := func(got *os.File) error {
				calls = append(calls, "unlock")
				if got != file {
					t.Fatal("ownership probe unlocked a different file")
				}
				if _, err := file.Stat(); err != nil {
					t.Fatal("ownership probe closed the file before unlocking")
				}
				return test.unlockErr
			}
			got := probeOwnershipFailure(root, test.original, checkDir, open, tryLock, unlock)
			if got != test.want {
				ownershipFailureFatal(t, test.resultFailure)
			}
			if !reflect.DeepEqual(calls, test.calls) {
				t.Fatal("ownership probe dependency order or count changed")
			}
			if opened {
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("ownership probe left its opened file unclosed")
				}
			}
		})
	}
}

func TestOwnershipFailureProbeRealLock(t *testing.T) {
	root := testPrivateRoot(t)
	owner, err := acquireLock(root, true)
	if err != nil {
		t.Fatal("cannot acquire ownership probe real fixture lock")
	}
	t.Cleanup(func() { _ = owner.Close() })
	original := errors.New("real ownership probe original refusal")
	t.Run("held", func(t *testing.T) {
		if got := probeOwnershipFailure(root, original, lockDirectory, openLockFile, lockFile, unlockFile); got != ErrLocked {
			t.Fatal("real held owner did not return bare ErrLocked")
		}
	})
	if err := owner.Close(); err != nil {
		t.Fatal("cannot release ownership probe real fixture lock")
	}
	t.Run("free", func(t *testing.T) {
		if got := probeOwnershipFailure(root, original, lockDirectory, openLockFile, lockFile, unlockFile); got != original {
			t.Fatal("real free owner changed original error identity")
		}
		next, err := acquireLock(root, true)
		if err != nil {
			t.Fatal("real free probe retained the stable owner lock")
		}
		if err := next.Close(); err != nil {
			t.Fatal("cannot release ownership probe following owner lock")
		}
	})
	t.Run("owner_missing", func(t *testing.T) {
		empty := testPrivateRoot(t)
		if got := probeOwnershipFailure(empty, original, lockDirectory, openLockFile, lockFile, unlockFile); got != original {
			t.Fatal("real missing owner changed original error identity")
		}
		if _, err := os.Lstat(filepath.Join(empty, ".owner.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("real missing owner probe created a stable lock file")
		}
		if _, err := os.Lstat(filepath.Join(empty, "run")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("real missing owner probe created a run directory")
		}
	})
}

func TestClassifyOwnershipFailureGate(t *testing.T) {
	root := testPrivateRoot(t)
	owner, err := acquireLock(root, true)
	if err != nil {
		t.Fatal("cannot acquire ownership classifier gate fixture lock")
	}
	t.Cleanup(func() { _ = owner.Close() })
	original := errors.New("ownership classifier gate original refusal")
	want := original
	if runtime.GOOS == "windows" {
		want = ErrLocked
	}
	if got := classifyOwnershipFailure(root, original); got != want {
		t.Fatal("ownership classifier platform gate changed")
	}
	if got := classifyOwnershipFailure(root, nil); got != nil {
		t.Fatal("ownership classifier turned success into refusal")
	}
}

func ownershipWiringSources(t *testing.T) map[string]string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal("cannot enumerate ownership wiring source files")
	}
	sources := make(map[string]string)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("cannot read ownership wiring source file")
		}
		sources[name] = strings.ReplaceAll(string(data), "\r\n", "\n")
	}
	return sources
}

func ownershipIdent(node ast.Expr, name string) bool {
	ident, ok := node.(*ast.Ident)
	return ok && ident.Name == name
}

func ownershipRoot(node ast.Expr) bool {
	root, ok := node.(*ast.SelectorExpr)
	if !ok || root.Sel.Name != "Root" {
		return false
	}
	cfg, ok := root.X.(*ast.SelectorExpr)
	return ok && cfg.Sel.Name == "cfg" && ownershipIdent(cfg.X, "c")
}

func ownershipClassifierCall(node ast.Node) (*ast.CallExpr, bool) {
	call, ok := node.(*ast.CallExpr)
	return call, ok && ownershipIdent(call.Fun, "classifyOwnershipFailure")
}

func ownershipWrappedReturn(statement ast.Stmt) bool {
	returned, ok := statement.(*ast.ReturnStmt)
	if !ok || len(returned.Results) != 2 || !ownershipIdent(returned.Results[0], "nil") {
		return false
	}
	call, ok := ownershipClassifierCall(returned.Results[1])
	return ok && len(call.Args) == 2 && ownershipRoot(call.Args[0]) && ownershipIdent(call.Args[1], "err")
}

func ownershipRootGuard(statement ast.Stmt) bool {
	guard, ok := statement.(*ast.IfStmt)
	if !ok || guard.Init == nil {
		return false
	}
	assign, ok := guard.Init.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "validateRoot" && ownershipIdent(selector.X, "c")
}

func ownershipClosedBeforeReturn(block *ast.BlockStmt) bool {
	for i, statement := range block.List {
		assign, ok := statement.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || !ownershipIdent(assign.Lhs[0], "_") || len(assign.Rhs) != 1 {
			continue
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Close" || !ownershipIdent(selector.X, "lock") {
			continue
		}
		if i+1 >= len(block.List) {
			return false
		}
		returned, ok := block.List[i+1].(*ast.ReturnStmt)
		return ok && len(returned.Results) == 2 && ownershipIdent(returned.Results[0], "nil") && ownershipIdent(returned.Results[1], "err")
	}
	return false
}

// This checker accepts only supplied source text and deliberately parses files
// for every platform. Normalizing here also covers directly supplied CRLF text.
func ownershipFailureWiring(sources map[string]string) string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	functions := make(map[string]*ast.FuncDecl)
	var parsed []*ast.File
	for _, name := range names {
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(sources[name], "\r\n", "\n"), 0)
		if err != nil {
			return "ownership wiring source did not parse"
		}
		parsed = append(parsed, file)
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && (function.Name.Name == "own" || function.Name.Name == "ownExisting") {
				if functions[function.Name.Name] != nil {
					return "ownership wiring repeated an ownership function"
				}
				functions[function.Name.Name] = function
			}
		}
	}
	for _, name := range []string{"own", "ownExisting"} {
		function := functions[name]
		if function == nil || function.Body == nil || len(function.Body.List) < 3 {
			return "ownership wiring lacks a complete ownership function"
		}
		rootGuard, rootOK := function.Body.List[0].(*ast.IfStmt)
		rootWrapped := rootOK && ownershipRootGuard(rootGuard) && len(rootGuard.Body.List) == 1 && ownershipWrappedReturn(rootGuard.Body.List[0])
		if !rootWrapped {
			if name == "own" {
				return "own initial validation refusal lacks its classifier"
			}
			return "ownExisting initial validation refusal lacks its classifier"
		}
		lockGuard, lockOK := function.Body.List[2].(*ast.IfStmt)
		lockWrapped := lockOK && len(lockGuard.Body.List) == 1 && ownershipWrappedReturn(lockGuard.Body.List[0])
		if !lockWrapped {
			if name == "own" {
				return "own lock acquisition refusal lacks its classifier"
			}
			return "ownExisting lock acquisition refusal lacks its classifier"
		}
		postFound := false
		postValid := true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			guard, ok := node.(*ast.IfStmt)
			if ok && guard != rootGuard && ownershipRootGuard(guard) {
				postFound = true
				postValid = postValid && ownershipClosedBeforeReturn(guard.Body)
			}
			return true
		})
		if !postFound || !postValid {
			return "post acquisition validation refusal must retain its original return"
		}
	}
	counts := make(map[string]int)
	probeCount := 0
	for _, file := range parsed {
		parents := make(map[ast.Node]ast.Node)
		var stack []ast.Node
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) != 0 {
				parents[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
		failure := ""
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || failure != "" {
				return true
			}
			classifier := ownershipIdent(call.Fun, "classifyOwnershipFailure")
			probe := ownershipIdent(call.Fun, "probeOwnershipFailure")
			if !classifier && !probe {
				return true
			}
			var enclosing *ast.FuncDecl
			for parent := parents[node]; parent != nil; parent = parents[parent] {
				if function, ok := parent.(*ast.FuncDecl); ok {
					enclosing = function
					break
				}
			}
			if classifier {
				if enclosing == nil || (enclosing.Name.Name != "own" && enclosing.Name.Name != "ownExisting") {
					failure = "ownership classifier call appeared outside ownership wrappers"
					return true
				}
				returned, ok := parents[node].(*ast.ReturnStmt)
				if !ok || !ownershipWrappedReturn(returned) || returned.Results[1] != call {
					failure = "ownership classifier call changed return shape or arguments"
					return true
				}
				counts[enclosing.Name.Name]++
			} else {
				probeCount++
				if enclosing == nil || enclosing.Name.Name != "classifyOwnershipFailure" {
					failure = "ownership probe call appeared outside its classifier"
					return true
				}
				want := []string{"root", "original", "lockDirectory", "openLockFile", "lockFile", "unlockFile"}
				if len(call.Args) != len(want) {
					failure = "ownership probe dependency argument count changed"
					return true
				}
				for i, name := range want {
					if !ownershipIdent(call.Args[i], name) {
						failure = "ownership probe literal dependency list changed"
						break
					}
				}
			}
			return true
		})
		if failure != "" {
			return failure
		}
	}
	if counts["own"] != 2 || counts["ownExisting"] != 2 {
		return "ownership classifier wrapper counts changed"
	}
	if probeCount != 1 {
		return "ownership classifier must be the single probe caller"
	}
	return ""
}

func TestOwnershipFailureWrapperWiring(t *testing.T) {
	if failure := ownershipFailureWiring(ownershipWiringSources(t)); failure != "" {
		ownershipFailureFatal(t, failure)
	}
}

func TestOwnershipFailureWrapperWiringRejectsMutations(t *testing.T) {
	sources := ownershipWiringSources(t)
	original := sources["lifecycle.go"]
	wrapper := "return nil, classifyOwnershipFailure(c.cfg.Root, err)"
	for _, test := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{"own_validation", "if err := c.validateRoot(create); err != nil {\n\t\t" + wrapper, "if err := c.validateRoot(create); err != nil {\n\t\treturn nil, err", "own initial validation refusal lacks its classifier"},
		{"own_lock", "lock, err := acquireLock(c.cfg.Root, true)\n\tif err != nil {\n\t\t" + wrapper, "lock, err := acquireLock(c.cfg.Root, true)\n\tif err != nil {\n\t\treturn nil, err", "own lock acquisition refusal lacks its classifier"},
		{"existing_validation", "func (c *Cell) ownExisting() (*operationLock, error) {\n\tif err := c.validateRoot(false); err != nil {\n\t\t" + wrapper, "func (c *Cell) ownExisting() (*operationLock, error) {\n\tif err := c.validateRoot(false); err != nil {\n\t\treturn nil, err", "ownExisting initial validation refusal lacks its classifier"},
		{"existing_lock", "lock, err := acquireLock(c.cfg.Root, false)\n\tif err != nil {\n\t\t" + wrapper, "lock, err := acquireLock(c.cfg.Root, false)\n\tif err != nil {\n\t\treturn nil, err", "ownExisting lock acquisition refusal lacks its classifier"},
		{"post_acquisition", "_ = lock.Close()\n\t\treturn nil, err", "_ = lock.Close()\n\t\t" + wrapper, "post acquisition validation refusal must retain its original return"},
		{"inspect", "func (c *Cell) Inspect(_ context.Context) (Inspection, error) {", "func (c *Cell) Inspect(_ context.Context) (Inspection, error) {\n\t_ = classifyOwnershipFailure(c.cfg.Root, nil)", "ownership classifier call appeared outside ownership wrappers"},
		{"direct_probe", "\treturn lock, nil\n}\n\nfunc (c *Cell) ownExisting", "\t_ = probeOwnershipFailure(c.cfg.Root, nil, lockDirectory, openLockFile, lockFile, unlockFile)\n\treturn lock, nil\n}\n\nfunc (c *Cell) ownExisting", "ownership probe call appeared outside its classifier"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(original, test.from) {
				t.Fatal("ownership wiring mutation source was absent")
			}
			mutant := make(map[string]string, len(sources))
			for name, source := range sources {
				mutant[name] = source
			}
			mutant["lifecycle.go"] = strings.Replace(original, test.from, test.to, 1)
			if got := ownershipFailureWiring(mutant); got != test.want {
				t.Fatal("ownership wiring mutation rejection differed")
			}
		})
	}
	crlf := make(map[string]string, len(sources))
	for name, source := range sources {
		crlf[name] = strings.ReplaceAll(source, "\n", "\r\n")
	}
	if failure := ownershipFailureWiring(crlf); failure != "" {
		t.Fatal("ownership wiring rejected equivalent CRLF source")
	}
}

// Keep fixed table and checker failures visible to the CI diagnostic allowlist.
func ownershipFailureFatal(t *testing.T, failure string) {
	t.Helper()
	switch failure {
	case "nil ownership refusal became an error":
		t.Fatal("nil ownership refusal became an error")
	case "existing wrapped ownership refusal changed identity":
		t.Fatal("existing wrapped ownership refusal changed identity")
	case "directory refusal changed original error identity":
		t.Fatal("directory refusal changed original error identity")
	case "open refusal changed original error identity":
		t.Fatal("open refusal changed original error identity")
	case "missing owner changed original error identity":
		t.Fatal("missing owner changed original error identity")
	case "held owner did not return bare ErrLocked":
		t.Fatal("held owner did not return bare ErrLocked")
	case "wrapped held owner did not return bare ErrLocked":
		t.Fatal("wrapped held owner did not return bare ErrLocked")
	case "unrelated lock refusal changed original error identity":
		t.Fatal("unrelated lock refusal changed original error identity")
	case "free owner changed original error identity":
		t.Fatal("free owner changed original error identity")
	case "unlock refusal changed original error identity":
		t.Fatal("unlock refusal changed original error identity")
	case "ownership wiring source did not parse":
		t.Fatal("ownership wiring source did not parse")
	case "ownership wiring repeated an ownership function":
		t.Fatal("ownership wiring repeated an ownership function")
	case "ownership wiring lacks a complete ownership function":
		t.Fatal("ownership wiring lacks a complete ownership function")
	case "own initial validation refusal lacks its classifier":
		t.Fatal("own initial validation refusal lacks its classifier")
	case "ownExisting initial validation refusal lacks its classifier":
		t.Fatal("ownExisting initial validation refusal lacks its classifier")
	case "own lock acquisition refusal lacks its classifier":
		t.Fatal("own lock acquisition refusal lacks its classifier")
	case "ownExisting lock acquisition refusal lacks its classifier":
		t.Fatal("ownExisting lock acquisition refusal lacks its classifier")
	case "post acquisition validation refusal must retain its original return":
		t.Fatal("post acquisition validation refusal must retain its original return")
	case "ownership classifier call appeared outside ownership wrappers":
		t.Fatal("ownership classifier call appeared outside ownership wrappers")
	case "ownership classifier call changed return shape or arguments":
		t.Fatal("ownership classifier call changed return shape or arguments")
	case "ownership probe call appeared outside its classifier":
		t.Fatal("ownership probe call appeared outside its classifier")
	case "ownership probe dependency argument count changed":
		t.Fatal("ownership probe dependency argument count changed")
	case "ownership probe literal dependency list changed":
		t.Fatal("ownership probe literal dependency list changed")
	case "ownership classifier wrapper counts changed":
		t.Fatal("ownership classifier wrapper counts changed")
	case "ownership classifier must be the single probe caller":
		t.Fatal("ownership classifier must be the single probe caller")
	default:
		t.Fatal("ownership failure diagnostic is outside its fixed vocabulary")
	}
}
