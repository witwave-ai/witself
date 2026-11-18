package localcell

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
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
	file, err := openLockFile(c.path(filepath.FromSlash(name)), true)
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
			if c.begin("start") != nil {
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
			if _, complete := checkpointComplete(c.path("recovery", locklessTarget), locklessTarget); strings.HasPrefix(row.name, "mid_purge") && !complete {
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
	`acquireLock(c.cfg.Root, false)`, `defer held.Close()`, `c.validateRoot(false)`, `os.Lstat(c.path("journal", "maintenance.json"))`,
	`os.Lstat(c.recoveryDir())`, `admit()`}

var locklessWriters = []string{"own", "ownExisting", "layout", "begin", "step", "journal", "journalCause", "makePrivateDir",
	"openLockFile", "lockFile", "writeAtomic", "writeAtomicBytes", "createPrivateTemp", "publishPrivateFile", "syncDirectory",
	"holdCheckpoint", "closeCheckpointLeftover", "maintenanceFenceAllows", "readMaintenanceFence", "classifyOwnershipFailure"}

// The os calls each dry function may make; admitDeleteCheckpoint keeps the
// exact-spelling listing (os.ReadDir) it carries unchanged from the locked path.
var locklessReads = map[string][]string{"refuseBeforeLockNames": {"Lstat"}, "admitCheckpointDry": {"Lstat"}, "admitDeleteCheckpoint": {"Lstat", "ReadDir"}}

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

// locklessContract returns the keys checkpoint.go's dry admission violates.
func locklessContract(sources map[string]string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "checkpoint.go", strings.ReplaceAll(sources["checkpoint.go"], "\r\n", "\n"), 0)
	if err != nil {
		return []string{"parse:checkpoint.go"}
	}
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
	for name, reads := range locklessReads {
		writes := methods[name] == nil
		for _, call := range locklessAllCalls(methods[name]) {
			callee := types.ExprString(call.Fun)
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				callee = selector.Sel.Name
				writes = writes || (strings.HasPrefix(types.ExprString(call.Fun), "os.") && !slices.Contains(reads, callee))
			}
			writes = writes || slices.Contains(locklessWriters, callee) || (callee == "privateDir" && (len(call.Args) != 2 || types.ExprString(call.Args[1]) != "false"))
		}
		if writes {
			failures = append(failures, "lockless_writer:"+name)
		}
	}
	var assigned, admitted []token.Pos
	if fn := methods["admitCheckpointDry"]; fn != nil {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if assignment, ok := n.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && types.ExprString(assignment.Lhs[0]) == "ops.control" {
				assigned = append(assigned, assignment.Pos())
			}
			return true
		})
	}
	for _, call := range locklessAllCalls(methods["admitCheckpointDry"]) {
		if types.ExprString(call.Fun) == "c.admitCheckpoint" {
			admitted = append(admitted, map[bool]token.Pos{true: call.Pos()}[types.ExprString(call) == "c.admitCheckpoint(ctx, options, ops)"])
		}
	}
	if len(assigned) != 1 || len(admitted) != 1 || admitted[0] == token.NoPos || assigned[0] > admitted[0] {
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
	// Every early return is nil but the one after the lock probe that returns
	// the second validateRoot's refusal.
	acquired, allowed := token.NoPos, map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "acquireLock" {
			acquired = call.Pos()
		} else if branch, ok := n.(*ast.IfStmt); ok && branch.Init != nil && acquired != token.NoPos && len(branch.Body.List) == 1 &&
			admissionNodeText(branch.Init) == "err := c.validateRoot(false)" && admissionNodeText(branch.Body.List[0]) == "return err" {
			allowed[branch.Body.List[0]] = true
		}
		return true
	})
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if result, ok := n.(*ast.ReturnStmt); ok && n != fn.Body.List[len(fn.Body.List)-1] && admissionNodeText(result) != "return nil" && !allowed[n] {
			failures = append(failures, "lockless_defer")
		}
		return true
	})
	return failures
}

func TestCheckpointLocklessSourceContract(t *testing.T) {
	source := strings.ReplaceAll(admissionReadSources(t)["checkpoint.go"], "\r\n", "\n")
	for _, row := range []struct{ name, before, after, key string }{
		{"control", "", "", ""},
		{"gate_removed", "\tif (owner == nil && run == nil) || c.validateRoot(false) != nil {\n\t\treturn nil\n\t}\n", "", "lockless_order"},
		{"acquire_creates", "held, err := acquireLock(c.cfg.Root, false)", "held, err := acquireLock(c.cfg.Root, true)", "lockless_order"},
		{"fence_helper", "if _, err := os.Lstat(c.path(\"journal\", \"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn nil", "if err := c.maintenanceFenceAllows(0); err != nil {\n\t\treturn err", "lockless_writer:refuseBeforeLockNames"},
		{"classified", "acquireLock(c.cfg.Root, false)\n\tif err != nil {\n\t\treturn nil", "acquireLock(c.cfg.Root, false)\n\tif err != nil {\n\t\treturn classifyOwnershipFailure(c.cfg.Root, err)", "lockless_writer:refuseBeforeLockNames"},
		{"fence_refuses", "\"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn nil", "\"maintenance.json\")); !errors.Is(err, fs.ErrNotExist) {\n\t\treturn errMaintenanceFenceActive", "lockless_defer"},
		{"control_kept", "\tops.control = func(context.Context) (pgControlData, error) { return pgControlData{}, errCheckpointDryAdmitted }\n", "", "lockless_control"},
		{"hold_in_admission", "\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "\t_, _, _ = c.holdCheckpoint(options.CheckpointID)\n\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "lockless_writer:admitDeleteCheckpoint"},
		{"delete_unsplit", "DeleteCheckpointOptions) error {\n\tif err := c.admitDeleteCheckpoint(options); err != nil {\n\t\treturn err\n\t}\n", "DeleteCheckpointOptions) error {\n", "lockless_split"},
		{"admission_refuses", "\treturn nil\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "\treturn errCheckpointIdentity\n}\n\nfunc (c *Cell) deleteCheckpointLocked(", "lockless_split"},
		{"create_unguarded", "\tif err := c.refuseBeforeLockNames(func() error { return c.admitCheckpointDry(ctx, options) }); err != nil {\n\t\treturn err\n\t}\n", "", "lockless_first:Checkpoint"},
		{"delete_unguarded", "\tif err := c.refuseBeforeLockNames(func() error { return c.admitDeleteCheckpoint(options) }); err != nil {\n\t\treturn err\n\t}\n", "", "lockless_first:DeleteCheckpoint"},
		{"unparsable", "package localcell\n", "package localcell {\n", "parse:checkpoint.go"},
	} {
		if strings.Count(source, row.before) != 1 && row.before != "" {
			t.Fatal("lockless contract anchor is not unique: " + row.name)
		}
		mutated := strings.Replace(source, row.before, row.after, 1)
		for _, ending := range []string{"LF", "CRLF"} {
			t.Run(row.name+"/"+ending, func(t *testing.T) {
				input := map[string]string{"LF": mutated, "CRLF": strings.ReplaceAll(mutated, "\n", "\r\n")}[ending]
				failures := locklessContract(map[string]string{"checkpoint.go": input})
				if (row.key == "" && len(failures) != 0) || (row.key != "" && !slices.Contains(failures, row.key)) {
					t.Fatalf("lockless contract reported %v, want %q", failures, row.key)
				}
			})
		}
	}
}
