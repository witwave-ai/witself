package localcell

import (
	"context"
	"errors"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Synthetic identifiers: the target a fixture plants and one never present.
const locklessTarget, locklessAbsent = "ckpt_aaaaaaaaaaaaaaaa", "ckpt_bbbbbbbbbbbbbbbb"

// locklessRoot is a fresh private directory that never was a cell.
func locklessRoot(t *testing.T) *Cell {
	t.Helper()
	c, err := New(Config{Root: testPrivateRoot(t), Timeout: time.Second})
	if err != nil {
		t.Fatal("cannot construct the lockless fixture cell")
	}
	return c
}

// locklessStopped is a synthetic stopped cell; plus plants the complete target.
func locklessStopped(t *testing.T, layout string, plus bool) (*Cell, *checkpointRecorder) {
	t.Helper()
	f, r := checkpointFixture(t, layout, false)
	if plus {
		checkpointPlant(t, f.cell, locklessTarget, locklessTarget, "data")
	}
	return f.cell, r
}

// locklessPurge completes a purging Remove, or interrupts it at cut.
func locklessPurge(t *testing.T, c *Cell, cut string) *Cell {
	t.Helper()
	c.cfg.Failpoint = checkpointFailpoint("remove", cut)
	err := c.Remove(t.Context(), RemoveOptions{Purge: true})
	if c.cfg.Failpoint = nil; (cut == "" && err != nil) || (cut != "" && !errors.Is(err, errInterrupted)) {
		t.Fatal("cannot prepare the purged fixture")
	}
	return c
}

// locklessLock creates an empty lock file; hold keeps it locked by this
// process (flock and LockFileEx belong to the open file) until cleanup.
func locklessLock(t *testing.T, c *Cell, name string, hold bool) {
	t.Helper()
	file, err := openLockFile(context.Background(), c.path(filepath.FromSlash(name)), true)
	if err != nil {
		t.Fatal("cannot prepare the lockless fixture lock")
	}
	if !hold {
		_ = file.Close()
		return
	}
	t.Cleanup(func() { _ = unlockFile(file); _ = file.Close() })
	if lockFile(file) != nil {
		t.Fatal("cannot hold the lockless fixture lock")
	}
}

func locklessDirs(t *testing.T, c *Cell, names ...string) *Cell {
	t.Helper()
	for _, name := range names {
		if privateDir(c.path(filepath.FromSlash(name)), true) != nil {
			t.Fatal("cannot prepare a lockless fixture directory")
		}
	}
	return c
}

// locklessStale plants a stale pg/postmaster.pid, which the admission refuses.
func locklessStale(t *testing.T, c *Cell) *Cell {
	checkpointWrite(t, c, "pg/postmaster.pid", strconv.Itoa(checkpointUnallocatedPID)+"\n"+c.path("pg")+"\n1\n")
	return c
}

// locklessStandIn points pg_controldata at a copy of this test binary, which
// runs as the stand-in (dispatchCheckpointControlHelper) and claims one call-N
// name in the returned directory per real run, each with a stopped reading.
func locklessStandIn(t *testing.T, c *Cell) string {
	t.Helper()
	dir, d, reading := testPrivateRoot(t), checkpointSyntheticControl, ""
	for index, value := range []string{d.ControlVersion, d.CatalogVersion, d.SystemIdentifier, d.State, d.CheckpointLSN} {
		reading += checkpointControlKeys[index] + ": " + value + "\n"
	}
	source, err := os.Open(os.Args[0])
	target, createErr := openPrivateFile(filepath.Join(dir, executable("pg_controldata")), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err = errors.Join(err, createErr); err == nil {
		_, err = io.Copy(target, source)
		err = errors.Join(err, target.Close(), source.Close())
	}
	step := map[string]string{"Out": reading}
	if c.cfg.Binaries.PostgresDir = dir; err != nil || writeAtomic(filepath.Join(dir, "checkpoint-control-script.json"), []map[string]string{step, step, step}, nil) != nil {
		t.Fatal("cannot prepare the stand-in control program")
	}
	return dir
}

// locklessCall runs one verb directly, without the command's before-read.
func locklessCall(t *testing.T, c *Cell, verb string) error {
	switch verb {
	case "create":
		return c.Checkpoint(t.Context(), CheckpointOptions{InstallationID: "lc_test"})
	case "delete_target":
		return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: locklessTarget})
	}
	return c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: locklessAbsent})
}

// locklessDiff lists by slash name what a call added, removed and changed.
func locklessDiff(t *testing.T, c *Cell, before map[string]retainedEntry) (added, removed, changed []string) {
	t.Helper()
	after := checkpointTreeState(t, c)
	for name, entry := range after {
		if old, ok := before[name]; !ok {
			added = append(added, name)
		} else if old != entry {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return added, removed, changed
}

// A refusal decided by what stood in the root creates no lock name. Before the
// dry admission each of these created .owner.lock, run/ or run/operation.lock.
func TestCheckpointLocklessRefusal(t *testing.T) {
	identity, state := "local_checkpoint_identity_mismatch", "local_checkpoint_state_unsupported"
	purged := func(plus bool, dirs ...string) func(*testing.T) *Cell {
		return func(t *testing.T) *Cell {
			c, _ := locklessStopped(t, "legacy", plus)
			return locklessPurge(t, locklessDirs(t, c, dirs...), "")
		}
	}
	without := func(plus bool, names ...string) func(*testing.T) *Cell {
		return func(t *testing.T) *Cell {
			c, _ := locklessStopped(t, "legacy", plus)
			for _, name := range names {
				if removePrivateFile(c.path(filepath.FromSlash(name))) != nil {
					t.Fatal("cannot remove a lockless fixture lock")
				}
			}
			return c
		}
	}
	type row struct {
		name, verb, want string
		build            func(*testing.T) *Cell
	}
	rows := []row{
		{"never_cell/empty", "delete_absent", identity, locklessRoot},
		{"never_cell/unrelated_files", "delete_absent", identity, func(t *testing.T) *Cell {
			c := locklessDirs(t, locklessRoot(t), "sub")
			checkpointWrite(t, c, "notes.txt", "synthetic note")
			checkpointWrite(t, c, "sub/data", "synthetic data")
			return c
		}},
		{"never_cell/journal_pending", "delete_absent", state, func(t *testing.T) *Cell {
			c := locklessDirs(t, locklessRoot(t), "journal")
			if c.begin(context.Background(), "start") != nil {
				t.Fatal("cannot open the pending fixture operation")
			}
			return c
		}},
		{"owner_lock_only", "delete_absent", identity, func(t *testing.T) *Cell {
			c := locklessRoot(t)
			locklessLock(t, c, ".owner.lock", false)
			return c
		}},
		{"purged/none", "delete_absent", identity, purged(false)},
		{"purged/empty_recovery", "delete_absent", identity, purged(false, "recovery")},
		{"purged/one", "delete_absent", identity, purged(true)},
		{"hand_made/no_owner_lock/delete_absent", "delete_absent", identity, without(false, ".owner.lock", "run/operation.lock")},
		{"hand_made/no_run_lock/delete_absent", "delete_absent", identity, without(true, "run/operation.lock")},
		{"hand_made/no_owner_lock/create", "create", state, func(t *testing.T) *Cell {
			return locklessStale(t, without(false, ".owner.lock", "run/operation.lock")(t))
		}},
	}
	for _, cut := range []string{"run", "journal"} {
		for _, layout := range []string{"legacy", "selected"} {
			for _, verb := range []string{"create", "delete_absent", "delete_target"} {
				rows = append(rows, row{"mid_purge/" + cut + "_gone/" + layout + "/" + verb, verb, state, func(t *testing.T) *Cell {
					c, _ := locklessStopped(t, layout, true)
					return locklessPurge(t, c, "cleanup_"+cut)
				}})
			}
		}
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := row.build(t)
			before := checkpointTreeState(t, c)
			err := locklessCall(t, c, row.verb)
			if checkpointUnchanged(t, c, before); !checkpointSameError(err, row.want) {
				t.Fatal("lockless refusal returned another class")
			}
			if _, complete, err := checkpointComplete(context.Background(), c.path("recovery", locklessTarget), locklessTarget); strings.HasPrefix(row.name, "mid_purge") && (err != nil || !complete) {
				t.Fatal("refused call left the target checkpoint incomplete")
			}
		})
	}
}

// An admitted verb creates exactly what own(false) created before, and
// pg_controldata still runs twice: the dry admission stops at its boundary.
func TestCheckpointLocklessWork(t *testing.T) {
	for _, row := range []struct {
		name  string
		plus  bool
		gone  []string
		added []string
	}{
		{"no_owner_lock", false, []string{".owner.lock", "run/operation.lock"}, []string{".owner.lock", "recovery", "run/operation.lock"}},
		{"no_run_lock", true, []string{"run/operation.lock"}, []string{"run/operation.lock"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, r := locklessStopped(t, "legacy", row.plus)
			for _, name := range row.gone {
				if removePrivateFile(c.path(filepath.FromSlash(name))) != nil {
					t.Fatal("cannot remove a lockless fixture lock")
				}
			}
			before, count := checkpointTreeState(t, c), len(checkpointIDs(t, c))
			if locklessCall(t, c, "create") != nil || r.calls["control"] != 2 || len(checkpointIDs(t, c)) != count+1 {
				t.Fatal("admitted create failed or ran the control program other than twice")
			}
			added, removed, changed := locklessDiff(t, c, before)
			added = slices.DeleteFunc(added, func(name string) bool { return strings.HasPrefix(name, "recovery/") })
			if !slices.Equal(added, row.added) || len(removed) != 0 || !slices.Equal(changed, []string{"journal/operations.jsonl"}) {
				t.Fatal("admitted create wrote other names than before")
			}
		})
	}
	t.Run("purged_one/delete_target", func(t *testing.T) {
		c, _ := locklessStopped(t, "legacy", true)
		before := checkpointTreeState(t, locklessPurge(t, c, ""))
		err := locklessCall(t, c, "delete_target")
		added, removed, changed := locklessDiff(t, c, before)
		stray := slices.ContainsFunc(removed, func(name string) bool { return !strings.HasPrefix(name, "recovery/"+locklessTarget) })
		if err != nil || len(removed) != 3 || stray || len(changed) != 0 || len(checkpointIDs(t, c)) != 0 ||
			!slices.Equal(added, []string{"journal", "journal/operations.jsonl", "run", "run/operation.lock"}) {
			t.Fatal("purged-root delete wrote other names than before")
		}
	})
}

// The real control program runs exactly twice for an admitted dry-path create
// (no owner lock, no run lock) and never for one its dry admission refuses.
func TestCheckpointLocklessControlRuns(t *testing.T) {
	for _, row := range []struct {
		name, want string
		runs       int
	}{{"admitted", "", 2}, {"refused", "local_checkpoint_state_unsupported", 0}} {
		t.Run(row.name, func(t *testing.T) {
			c, r := locklessStopped(t, "legacy", false)
			for _, name := range []string{".owner.lock", "run/operation.lock"} {
				if removePrivateFile(c.path(filepath.FromSlash(name))) != nil {
					t.Fatal("cannot remove a lockless fixture lock")
				}
			}
			if r.production = true; row.want != "" {
				locklessStale(t, c)
			}
			// A race-built stand-in launches slowly: each run may use the test's deadline.
			if deadline, ok := t.Deadline(); ok {
				c.cfg.Timeout = time.Until(deadline)
			}
			dir := locklessStandIn(t, c)
			err := locklessCall(t, c, "create")
			entries, readErr := os.ReadDir(dir)
			runs := len(slices.DeleteFunc(entries, func(entry os.DirEntry) bool { return !strings.HasPrefix(entry.Name(), "call-") }))
			if readErr != nil || runs != row.runs || (err == nil) != (row.want == "") || (err != nil && !checkpointSameError(err, row.want)) {
				t.Fatal("dry-path create ran the real control program otherwise")
			}
		})
	}
}

// Deferrals reach own(false) and keep its writes: a fence beside a missing name
// (OQ1(a)), a run lock without the owner lock (OQ3(a)), recovery/ without the
// owner lock (never read unowned), and an owner lock another holds.
func TestCheckpointLocklessDeferred(t *testing.T) {
	same := func(want string) func(error) bool {
		return func(err error) bool { return checkpointSameError(err, want) }
	}
	locked := func(err error) bool { return errors.Is(err, ErrLocked) }
	fenced := func(t *testing.T) *Cell {
		c := locklessDirs(t, locklessRoot(t), "journal", "recovery", "recovery/"+locklessTarget)
		checkpointWrite(t, c, "journal/maintenance.json", "{}")
		checkpointWrite(t, c, "recovery/"+locklessTarget+"/inventory.json", "{}")
		locklessLock(t, c, ".owner.lock", false)
		return c
	}
	runLock := func(hold bool) func(*testing.T) *Cell {
		return func(t *testing.T) *Cell {
			c := locklessDirs(t, locklessRoot(t), "run")
			locklessLock(t, c, "run/operation.lock", hold)
			return c
		}
	}
	ownerHeld := func(t *testing.T) *Cell {
		c, _ := locklessStopped(t, "legacy", false)
		locklessLock(t, locklessPurge(t, c, ""), ".owner.lock", true)
		return c
	}
	unowned := func(t *testing.T) *Cell {
		c := locklessRoot(t)
		checkpointPlant(t, c, locklessTarget, locklessTarget, "data")
		return c
	}
	for _, row := range []struct {
		name, verb string
		build      func(*testing.T) *Cell
		want       func(error) bool
		added      []string
	}{
		{"fenced_no_run/delete_absent", "delete_absent", fenced, same("local_maintenance_fence_active"), []string{"run", "run/operation.lock"}},
		{"fenced_no_run/delete_target", "delete_target", fenced, same("local_maintenance_fence_active"), []string{"run", "run/operation.lock"}},
		{"run_lock_held_no_owner_lock", "delete_absent", runLock(true), locked, []string{".owner.lock"}},
		{"run_lock_free_no_owner_lock", "delete_absent", runLock(false), same("local_checkpoint_identity_mismatch"), []string{".owner.lock"}},
		{"unowned_recovery", "delete_absent", unowned, same("local_checkpoint_identity_mismatch"), []string{".owner.lock", "run", "run/operation.lock"}},
		{"owner_lock_held/create", "create", ownerHeld, locked, nil},
		{"owner_lock_held/delete", "delete_absent", ownerHeld, locked, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := row.build(t)
			before := checkpointTreeState(t, c)
			err := locklessCall(t, c, row.verb)
			added, removed, changed := locklessDiff(t, c, before)
			if !row.want(err) || !slices.Equal(added, row.added) || len(removed)+len(changed) != 0 {
				t.Fatal("deferred call differs from the locked path")
			}
		})
	}
}

// On Windows a regular file where a directory is expected reads as not-exist
// below it. validateRoot refuses it before the preflight reads any name below
// it, so no absence is concluded and nothing is created, on every OS.
func TestCheckpointLocklessNotADirectory(t *testing.T) {
	for _, name := range []string{"run", "journal", "recovery"} {
		for _, verb := range []string{"create", "delete_absent"} {
			t.Run(name+"_file/"+strings.TrimSuffix(verb, "_absent"), func(t *testing.T) {
				var c *Cell
				if name == "recovery" {
					c = locklessRoot(t)
					locklessLock(t, c, ".owner.lock", false)
				} else {
					c, _ = locklessStopped(t, "legacy", false)
					locklessPurge(t, c, "")
				}
				checkpointWrite(t, c, name, "synthetic file at a directory name")
				before := checkpointTreeState(t, c)
				err := locklessCall(t, c, verb)
				if checkpointUnchanged(t, c, before); !errors.Is(err, errLocalDirectory) {
					t.Fatal("file at a layout name was not refused as a layout")
				}
			})
		}
	}
}

// With both lock names present the gate leaves today's path alone.
func TestCheckpointLocklessHealthy(t *testing.T) {
	c, _ := locklessStopped(t, "legacy", false)
	before := checkpointTreeState(t, c)
	err := locklessCall(t, c, "delete_absent")
	if checkpointUnchanged(t, c, before); !checkpointSameError(err, "local_checkpoint_identity_mismatch") {
		t.Fatal("healthy cell refused an absent identifier otherwise")
	}
}

var locklessFirst = map[string]string{
	"Checkpoint":       `if err := c.refuseBeforeLockNames(func() error { return c.admitCheckpointDry(ctx, options) }); err != nil { return err }`,
	"DeleteCheckpoint": `if err := c.refuseBeforeLockNames(func() error { return c.admitDeleteCheckpoint(options) }); err != nil { return err }`,
}

// refuseBeforeLockNames's calls in source order, errors.Is aside.
var locklessOrder = []string{`os.Lstat(c.path(".owner.lock"))`, `os.Lstat(c.path("run", "operation.lock"))`, `c.validateRoot(false)`,
	`acquireLock(context.Background(), c.cfg.Root, false)`, `defer held.Close()`, `c.validateRoot(false)`, `os.Lstat(c.path("journal", "maintenance.json"))`,
	`os.Lstat(c.recoveryDir())`, `admit()`}

// locklessCallees closes what each dry function may call, resolved by type: a
// function or method by its full name, a builtin, or the parameter admit. No
// package variable, field or other local is called, nothing unresolved is, and
// no function or package variable holding one is named as a value.
var locklessCallees = map[string][]string{
	"refuseBeforeLockNames": {"os.Lstat", "errors.Is", "context.Background", "localcell.acquireLock", "(*localcell.operationLock).Close", "(*localcell.Cell).path",
		"(*localcell.Cell).validateRoot", "(*localcell.Cell).recoveryDir", "param admit"},
	"admitCheckpointDry": {"errors.Is", "(*localcell.Cell).checkpointOps", "(*localcell.Cell).admitCheckpoint"},
	"admitDeleteCheckpoint": {"os.Lstat", "os.ReadDir", "errors.Is", "context.Background", "localcell.privateDir", "localcell.checkpointLeftover",
		"localcell.recoveryEntryCanonical", "(*localcell.Cell).readJournal", "(*localcell.Cell).checkpointCold", "(*localcell.Cell).validateRecovery",
		"(*localcell.Cell).checkpointDir", "(*localcell.Cell).recoveryDir", "(*regexp.Regexp).MatchString", "(io/fs.DirEntry).Name", "append", "len", "make"},
}

// The dry create's one control boundary, exactly: it returns the sentinel.
const locklessSentinel = `ops.control = func(context.Context) (pgControlData, error) { return pgControlData{}, errCheckpointDryAdmitted }`

// locklessCalls lists outermost call texts in source order; a function
// literal's body is read in place and a deferred call is marked.
func locklessCalls(node ast.Node, prefix string, calls *[]string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.DeferStmt:
			locklessCalls(n.Call, "defer ", calls)
			return false
		case *ast.CallExpr:
			if literal, ok := n.Fun.(*ast.FuncLit); ok {
				locklessCalls(literal.Body, prefix, calls)
			} else if types.ExprString(n.Fun) != "errors.Is" {
				*calls = append(*calls, prefix+types.ExprString(n))
			}
			return false
		}
		return true
	})
}

// locklessAllCalls is every call in fn, nested ones and closures included.
func locklessAllCalls(fn *ast.FuncDecl) []*ast.CallExpr {
	var calls []*ast.CallExpr
	if fn != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				calls = append(calls, call)
			}
			return true
		})
	}
	return calls
}

// locklessClosed reports whether fn calls only allowed callees, privateDir
// only with false, and names no function or package function variable else.
func locklessClosed(info *types.Info, fn *ast.FuncDecl, allowed []string) bool {
	closed, callees := fn != nil, map[*ast.Ident]bool{}
	for _, call := range locklessAllCalls(fn) {
		fun, name := ast.Unparen(call.Fun), ""
		if _, literal := fun.(*ast.FuncLit); literal {
			continue
		} else if selector, ok := fun.(*ast.SelectorExpr); ok {
			fun = selector.Sel
		}
		identifier, _ := fun.(*ast.Ident)
		switch object := info.Uses[identifier].(type) {
		case *types.Func:
			name = object.FullName()
		case *types.Builtin:
			name = object.Name()
		case *types.Var:
			if object.Pos() > fn.Type.Pos() && object.Pos() < fn.Type.End() {
				name = "param " + object.Name()
			}
		}
		callees[identifier] = true
		closed = closed && slices.Contains(allowed, name) && (name != "localcell.privateDir" || len(call.Args) == 2 && types.ExprString(call.Args[1]) == "false")
	}
	if fn != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if identifier, ok := n.(*ast.Ident); ok && !callees[identifier] {
				switch object := info.Uses[identifier].(type) {
				case *types.Func:
					closed = false
				case *types.Var:
					_, function := object.Type().Underlying().(*types.Signature)
					closed = closed && (!function || object.Pkg() == nil || object.Parent() != object.Pkg().Scope())
				}
			}
			return true
		})
	}
	return closed
}

// locklessContract returns the keys checkpoint.go's dry admission violates.
// It type-checks the given package sources (as built here) against
// checkpoint.go's standard imports, read from source as readers4 does.
func locklessContract(sources map[string]string) []string {
	var file *ast.File
	var files []*ast.File
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		parsed, err := parser.ParseFile(readers4Fset, name, strings.ReplaceAll(sources[name], "\r\n", "\n"), 0)
		if name == "checkpoint.go" && err != nil {
			return []string{"parse:checkpoint.go"}
		} else if name == "checkpoint.go" {
			file = parsed
		}
		files = append(files, parsed)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
	config := types.Config{Error: func(error) {}, Importer: readers4Importer(func(path string) (*types.Package, error) {
		if strings.Contains(path, ".") || !slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool { return spec.Path.Value == strconv.Quote(path) }) {
			return nil, errors.New("import not admitted")
		}
		return readers4Std.Import(path)
	})}
	_, _ = config.Check("localcell", readers4Fset, files, info)
	methods := map[string]*ast.FuncDecl{}
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Body != nil {
			methods[fn.Name.Name] = fn
		}
	}
	var failures []string
	statement := func(name string, index int) string {
		if fn := methods[name]; fn != nil && len(fn.Body.List) > 0 {
			return admissionNodeText(fn.Body.List[(index+len(fn.Body.List))%len(fn.Body.List)])
		}
		return ""
	}
	for name, want := range locklessFirst {
		if statement(name, 0) != admissionStatements(want)[0] {
			failures = append(failures, "lockless_first:"+name)
		}
	}
	if statement("deleteCheckpointLocked", 0) != admissionStatements(`if err := c.admitDeleteCheckpoint(options); err != nil { return err }`)[0] || statement("admitDeleteCheckpoint", -1) != "return nil" {
		failures = append(failures, "lockless_split")
	}
	for name, allowed := range locklessCallees {
		if !locklessClosed(info, methods[name], allowed) {
			failures = append(failures, "lockless_writer:"+name)
		}
	}
	var assigned, admitted []token.Pos
	if fn := methods["admitCheckpointDry"]; fn != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if assignment, ok := n.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && types.ExprString(assignment.Lhs[0]) == "ops.control" {
				assigned = append(assigned, map[bool]token.Pos{true: assignment.Pos()}[admissionNodeText(assignment) == admissionStatements(locklessSentinel)[0]])
			}
			return true
		})
	}
	for _, call := range locklessAllCalls(methods["admitCheckpointDry"]) {
		if types.ExprString(call.Fun) == "c.admitCheckpoint" {
			admitted = append(admitted, map[bool]token.Pos{true: call.Pos()}[types.ExprString(call) == "c.admitCheckpoint(ctx, options, ops)"])
		}
	}
	if len(assigned) != 1 || len(admitted) != 1 || assigned[0] == token.NoPos || admitted[0] == token.NoPos || assigned[0] > admitted[0] {
		failures = append(failures, "lockless_control")
	}
	fn := methods["refuseBeforeLockNames"]
	var calls []string
	if fn != nil {
		locklessCalls(fn.Body, "", &calls)
	}
	if !slices.Equal(calls, locklessOrder) {
		return append(failures, "lockless_order")
	}
	// Every early return defers: it is nil.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if result, ok := n.(*ast.ReturnStmt); ok && n != fn.Body.List[len(fn.Body.List)-1] && admissionNodeText(result) != "return nil" {
			failures = append(failures, "lockless_defer")
		}
		return true
	})
	return failures
}

func TestCheckpointLocklessSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	maps.DeleteFunc(sources, func(name, _ string) bool {
		built, err := build.Default.MatchFile(".", name)
		return err != nil || !built
	})
	source := strings.ReplaceAll(sources["checkpoint.go"], "\r\n", "\n")
	dry, shape := "\tops := c.checkpointOps()\n\tops.control", "MatchString(options.CheckpointID) {\n\t\treturn errCheckpointIdentity\n\t}\n"
	for _, row := range []struct{ name, before, after, key string }{
		{"control", "", "", ""},
		{"gate_removed", "\tif (owner == nil && run == nil) || c.validateRoot(false) != nil {\n\t\treturn nil\n\t}\n", "", "lockless_order"},
		{"acquire_creates", "held, err := acquireLock(context.Background(), c.cfg.Root, false)", "held, err := acquireLock(context.Background(), c.cfg.Root, true)", "lockless_order"},
		{"fence_helper", "if _, err := os.Lstat(c.path(\"journal\", \"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn nil", "if err := c.maintenanceFenceAllows(0); err != nil {\n\t\treturn err", "lockless_writer:refuseBeforeLockNames"},
		{"classified", "acquireLock(context.Background(), c.cfg.Root, false)\n\tif err != nil {\n\t\treturn nil", "acquireLock(context.Background(), c.cfg.Root, false)\n\tif err != nil {\n\t\treturn classifyOwnershipFailure(c.cfg.Root, err)", "lockless_writer:refuseBeforeLockNames"},
		{"fence_refuses", "\"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn nil", "\"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn errMaintenanceFenceActive", "lockless_defer"},
		{"control_kept", "\tops.control = func(context.Context) (pgControlData, error) { return pgControlData{}, errCheckpointDryAdmitted }\n", "", "lockless_control"},
		{"hold_in_admission", "\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "\t_, _, _ = c.holdCheckpoint(options.CheckpointID)\n\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "lockless_writer:admitDeleteCheckpoint"},
		{"delete_unsplit", "DeleteCheckpointOptions) error {\n\tif err := c.admitDeleteCheckpoint(options); err != nil {\n\t\treturn err\n\t}\n", "DeleteCheckpointOptions) error {\n", "lockless_split"},
		{"admission_refuses", "\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "\treturn errCheckpointIdentity\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "lockless_split"},
		{"create_unguarded", "\tif err := c.refuseBeforeLockNames(func() error { return c.admitCheckpointDry(ctx, options) }); err != nil {\n\t\treturn err\n\t}\n", "", "lockless_first:Checkpoint"},
		{"delete_unguarded", "\tif err := c.refuseBeforeLockNames(func() error { return c.admitDeleteCheckpoint(options) }); err != nil {\n\t\treturn err\n\t}\n", "", "lockless_first:DeleteCheckpoint"},
		{"unparsable", "package localcell\n", "package localcell {\n", "parse:checkpoint.go"},
		{"second_validate_returns", "if err := c.validateRoot(false); err != nil {\n\t\treturn nil", "if err := c.validateRoot(false); err != nil {\n\t\treturn err", "lockless_defer"},
		{"dry_lock_names", dry, "\tif l, err := acquireLock(c.cfg.Root, true); err == nil {\n\t\t_ = l.Close()\n\t}\n" + dry, "lockless_writer:admitCheckpointDry"},
		{"dry_control_run", dry, "\t_, _ = c.checkpointControl(ctx)\n" + dry, "lockless_writer:admitCheckpointDry"},
		{"sync_variable", dry, "\t_ = syncJournalDirectory(c.cfg.Root)\n" + dry, "lockless_writer:admitCheckpointDry"},
		{"sync_file", dry, "\tif f, err := openPrivateReader(c.cfg.Root); err == nil {\n\t\t_ = syncFile(f)\n\t\t_ = f.Close()\n\t}\n" + dry, "lockless_writer:admitCheckpointDry"},
		{"sync_parent", shape, shape + "\t_ = syncPrivateDirParent(c.recoveryDir())\n", "lockless_writer:admitDeleteCheckpoint"},
		{"sentinel_control", "{ return pgControlData{}, errCheckpointDryAdmitted }", "{ _, _ = c.checkpointControl(ctx); return pgControlData{}, errCheckpointDryAdmitted }", "lockless_control"},
		{"control_value", "func(context.Context) (pgControlData, error) { return pgControlData{}, errCheckpointDryAdmitted }", "c.checkpointControl", "lockless_writer:admitCheckpointDry"},
	} {
		if strings.Count(source, row.before) != 1 && row.before != "" {
			t.Fatal("lockless contract anchor is not unique: " + row.name)
		}
		mutated := strings.Replace(source, row.before, row.after, 1)
		for _, ending := range []string{"LF", "CRLF"} {
			t.Run(row.name+"/"+ending, func(t *testing.T) {
				input := maps.Clone(sources)
				input["checkpoint.go"] = map[string]string{"LF": mutated, "CRLF": strings.ReplaceAll(mutated, "\n", "\r\n")}[ending]
				failures := locklessContract(input)
				if (row.key == "" && len(failures) != 0) || (row.key != "" && !slices.Contains(failures, row.key)) {
					t.Fatalf("lockless contract reported %v, want %q", failures, row.key)
				}
			})
		}
	}
}
