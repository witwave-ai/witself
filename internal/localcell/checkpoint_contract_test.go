package localcell

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// checkpointFiles are the files that implement checkpoints.
var checkpointFiles = []string{"checkpoint.go", "checkpoint_unix.go", "checkpoint_windows.go", "checkpoint_other.go", "checkpoint_verify.go", "drill_root.go",
	"checkpoint_root.go", "drill_storage.go", "drill_storage_linux.go", "drill_storage_darwin.go", "drill_storage_windows.go", "drill_storage_other.go",
	"drill_reserve.go", "drill_reserve_linux.go", "drill_reserve_windows.go", "drill_reserve_other.go",
	"checkpoint_root_windows.go", "checkpoint_tree_unix.go", "checkpoint_tree_windows.go", "checkpoint_tree_other.go"}

// checkpointForbiddenOS are calls the checkpoint files may not make (C7(e));
// os.RemoveAll is counted by (a); os.OpenRoot is the Unix opener's, the drill
// root's and the held checkpoint roots'.
var checkpointForbiddenOS = []string{"os.Create", "os.CreateTemp", "os.WriteFile", "os.Mkdir", "os.MkdirAll", "os.OpenFile",
	"os.Rename", "os.Remove", "os.Truncate", "os.Chmod", "os.Chown", "os.Chtimes", "os.Link", "os.Symlink",
	"exec.Command", "exec.CommandContext"}

// checkpointForbiddenHelpers mint credentials, run PostgreSQL, stop, remove or
// change service intent (C7(c)).
var checkpointForbiddenHelpers = []string{"makePassword", "makePrincipalPasswords", "pgCommand", "databaseCommand", "startPostgres",
	"startServicePostgres", "stopAll", "retainRemoval", "finishRemoval", "setServiceDesiredStateLocked", "Repair", "ownExisting",
	"discardJournalTail", "stopSelected", "signalTerminate", "stopPostgres", "terminateWindowsPending", "removePrivateFile"}

// checkpointReaders may receive a path below pg/ or run/, or the manifest;
// filepath.Rel is pure path arithmetic. None waits on what it opens, and
// readRegular counts only through openPostgresReader.
var checkpointReaders = []string{"os.Lstat", "os.OpenRoot", "filepath.WalkDir", "filepath.Rel", "readRegular", "openPrivateReader",
	"runtimeconfig.WindowsPrivatePostgresOpen", "c.command"}

// checkpointWaitingReaders are the readers that wait on what they open; (j)
// refuses each in checkpointWaitingFiles, called or not, as spelled or as
// declared: an import's member under any local or dot name by its package.
var checkpointWaitingReaders = []string{"openPrivateFile", "readPrivateFile", "readPostgresFile", "os.Open", "os.ReadFile", "os.OpenInRoot", "os.DirFS",
	"ioutil.ReadFile", "fs.ReadFile"}
var checkpointWaitingFiles = []string{"checkpoint.go", "checkpoint_unix.go"}

// checkpointSourceRootCalls are the only uses (j) admits of the name that
// checkpoint_unix.go assigns its PGDATA root, opened as a directory only:
// os.OpenRoot(c.path("pg") + checkpointRootHere). The open only inside the
// function literal passed to waitOutLease, root.Lstat only as its lstat.
var checkpointSourceRootCalls = []string{"root.OpenFile(name, os.O_RDONLY | syscall.O_NONBLOCK, 0)", "root.Close()"}

// drillRootCalls are the only calls drill_root.go makes on a root receiver, by
// method, argument text and exact count.
var drillRootCalls = map[string][]string{"Stat": {`"."`}, "Lstat": {"drillID"}, "Open": {`"."`},
	"OpenFile": {"drillRootRecordName, os.O_RDONLY | syscall.O_NONBLOCK, 0"}, "OpenRoot": {"drillID + drillRootHere"},
	"RemoveAll": {"name"}, "Remove": {"drillRootRecordName", "drillID"}, "Close": {""}}
var drillRootCounts = map[string]int{"Stat": 2, "Lstat": 1, "Open": 2, "OpenFile": 1, "OpenRoot": 1, "RemoveAll": 1, "Remove": 2, "Close": 2}

// drillRootImports are drill_root.go's only imports, none renamed; from os and
// syscall it names only drillRootSelectors, called or not, and a root's
// removal, write or open method (drillRootMutators) only as a typed root call.
var drillRootImports = []string{"context", "errors", "io", "os", "path/filepath", "runtime", "strings", "syscall"}
var drillRootSelectors = map[string][]string{"os": {"DirEntry", "ErrNotExist", "Lstat", "O_RDONLY", "OpenRoot", "ReadDir", "Root", "SameFile"}, "syscall": {"O_NONBLOCK"}}
var drillRootMutators = []string{"Chmod", "Chown", "Chtimes", "Create", "Lchown", "Link", "Mkdir", "MkdirAll", "Open", "OpenFile", "OpenRoot", "Remove", "RemoveAll", "Rename", "Symlink", "WriteFile"}

// drillRootReceivers are drill_root.go's identifiers assigned once from
// os.OpenRoot or a root receiver's OpenRoot, and its *os.Root parameters;
// deferred holds every node inside a defer statement.
func drillRootReceivers(file *ast.File, values map[any][]ast.Expr) (receivers map[any]bool, deferred map[ast.Node]bool) {
	receivers, deferred = map[any]bool{}, map[ast.Node]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncType:
			for _, field := range node.Params.List {
				for _, identifier := range field.Names {
					if identifier.Obj != nil && types.ExprString(field.Type) == "*os.Root" {
						receivers[identifier.Obj] = true
					}
				}
			}
		case *ast.DeferStmt:
			ast.Inspect(node, func(inner ast.Node) bool { deferred[inner] = true; return true })
		}
		return true
	})
	for changed := true; changed; {
		changed = false
		for object, assigned := range values {
			call, ok := assigned[0].(*ast.CallExpr)
			if !ok || len(assigned) != 1 || receivers[object] {
				continue
			}
			selector, _ := call.Fun.(*ast.SelectorExpr)
			if selector != nil && selector.Sel.Name == "OpenRoot" && (types.ExprString(selector.X) == "os" || drillRootReceiver(receivers, selector.X)) {
				receivers[object], changed = true, true
			}
		}
	}
	return receivers, deferred
}

func drillRootReceiver(receivers map[any]bool, expression ast.Expr) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Obj != nil && receivers[identifier.Obj]
}

// drillRootIdentity is the one call assigned to an identity operand, with a
// root receiver spelled "root".
func drillRootIdentity(values map[any][]ast.Expr, receivers map[any]bool, expression ast.Expr) string {
	identifier, ok := expression.(*ast.Ident)
	if !ok || identifier.Obj == nil || len(values[identifier.Obj]) != 1 {
		return ""
	}
	call, ok := values[identifier.Obj][0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok && drillRootReceiver(receivers, selector.X) {
		return "root." + strings.TrimPrefix(types.ExprString(call), types.ExprString(selector.X)+".")
	}
	return types.ExprString(call)
}

func checkpointLiteral(node ast.Node) string {
	if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		value, _ := strconv.Unquote(literal.Value)
		return value
	}
	return ""
}

// checkpointAssignments maps each object to every value assigned to it; the
// first name of a multi-value assignment receives the call, later names nil.
func checkpointAssignments(file *ast.File) map[any][]ast.Expr {
	values := map[any][]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		for index := 0; ok && index < len(assign.Lhs); index++ {
			if identifier, isIdent := assign.Lhs[index].(*ast.Ident); isIdent && identifier.Obj != nil {
				var value ast.Expr
				if len(assign.Rhs) == len(assign.Lhs) || index == 0 {
					value = assign.Rhs[min(index, len(assign.Rhs)-1)]
				}
				values[identifier.Obj] = append(values[identifier.Obj], value)
			}
		}
		return true
	})
	return values
}

// checkpointRoot names where a path is rooted: "checkpoint" for a value of
// checkpointDir, "recovery" for c.recoveryDir() and "journal" for
// c.path("journal"). bare is false once joined; a joined ".." roots nothing.
func checkpointRoot(values map[any][]ast.Expr, expression ast.Expr, depth int) (root string, bare bool) {
	if identifier, ok := expression.(*ast.Ident); ok && identifier.Obj != nil && depth < 8 {
		for index, value := range values[identifier.Obj] {
			next, nextBare := checkpointRoot(values, value, depth+1)
			if next == "" || (index > 0 && next != root) {
				return "", false
			}
			root, bare = next, (index == 0 || bare) && nextBare
		}
		return root, bare
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok || depth >= 8 {
		return "", false
	}
	switch function := types.ExprString(call.Fun); {
	case (function == "c.checkpointDir" || function == "t.c.checkpointDir") && len(call.Args) == 1:
		return "checkpoint", true
	case function == "c.recoveryDir":
		return "recovery", true
	case function == "c.path" && len(call.Args) == 1 && checkpointLiteral(call.Args[0]) == "journal":
		return "journal", true
	case function == "filepath.Join" && !slices.ContainsFunc(call.Args[1:], func(argument ast.Expr) bool {
		return strings.Contains(checkpointLiteral(argument), "..")
	}):
		root, _ = checkpointRoot(values, call.Args[0], depth+1)
		return root, false
	case strings.HasSuffix(function, ".Name") && len(call.Args) == 0:
		// A temporary's name is rooted where createPrivateTemp created it.
		if base, ok := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident); ok && base.Obj != nil && len(values[base.Obj]) == 1 {
			if created, ok := values[base.Obj][0].(*ast.CallExpr); ok && types.ExprString(created.Fun) == "createPrivateTemp" {
				root, _ = checkpointRoot(values, created.Args[0], depth+1)
				return root, false
			}
		}
	}
	return "", false
}

// checkpointTreeBypassRooted reports whether path is rooted in a checkpointDir value, or joins recovery/ with a name
// that is not a literal (filepath.Join(c.recoveryDir(), id, ...) or c.path("recovery", id, ...)), through aliases.
func checkpointTreeBypassRooted(values map[any][]ast.Expr, path ast.Expr, depth int) bool {
	if root, _ := checkpointRoot(values, path, 0); root == "checkpoint" {
		return true
	} else if identifier, ok := path.(*ast.Ident); ok && identifier.Obj != nil && depth < 8 {
		return slices.ContainsFunc(values[identifier.Obj], func(value ast.Expr) bool { return checkpointTreeBypassRooted(values, value, depth+1) })
	}
	call, ok := path.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 || !slices.ContainsFunc(call.Args[1:], func(name ast.Expr) bool { return checkpointLiteral(name) == "" }) {
		return false
	}
	root, bare := checkpointRoot(values, call.Args[0], 0)
	return types.ExprString(call.Fun) == "filepath.Join" && root == "recovery" && bare || types.ExprString(call.Fun) == "c.path" && checkpointLiteral(call.Args[0]) == "recovery"
}

// checkpointTreeNilWants are the walk calls of checkpoint_tree_unix.go that pass no identity, by owner and count: the
// listing of names directly in recovery/, discard's single-component calls at the checkpoint's own name, and delete's
// admission and adoption of the directory at that name. Every other walk call with a want passes t.want.
var checkpointTreeNilWants = map[string]int{"discard:privateBeneathLstat": 2, "checkpointTreeRemoveDirectory:privateBeneathAt": 1, "listing:privateBeneathLstat": 1,
	"target:privateBeneathLstat": 1, "adopt:privateBeneathDirectory": 1}

// checkpointTreeBinds are the t.bound() calls of checkpoint_tree_unix.go by owner: each operation binds recovery/ first.
var checkpointTreeBinds = map[string]int{"hold": 1, "lstat": 1, "mkdir": 1, "createTemp": 1, "publish": 1, "sync": 1, "discard": 3, "unseal": 1, "sweep": 2,
	"open": 1, "check": 1, "record": 3, "listing": 1, "witness": 1, "snapshot": 1}

// checkpointTreeOrders are, for open, check and record (LC-CKPT-WALK-1C1), their binds, walk calls, temporary, file sync
// and lease wait in source order: each bind stands before the walk calls it guards, record's before its no-follow stat,
// its rename and its directory sync.
var checkpointTreeOrders = map[string]string{"open": "t.bound waitOutLease privateBeneathLstat privateBeneathOpen", "check": "t.bound privateBeneathList",
	"record": "t.bound privateBeneathLstat t.createTemp privateBeneathRemove syncFile t.bound privateBeneathReplace t.bound privateBeneathDirectory privateBeneathSync",
	"close":  "unix.Close", "listing": "t.hold t.bound t.recovery.Open dir.Close dir.Readdirnames unix.Fstat slices.Sort privateBeneathLstat privateBeneathIdentity privateBeneathDirectory t.retain",
	"witness":  "t.open t.bound privateBeneathDirectory unix.Fstatat f.Close f.Stat checkpointWitnessRead",
	"snapshot": "t.bound unix.Fstat dir.Readdirnames checkpointTreeListed unix.Fstatat privateBeneathIdentity t.recovery.Open scan dir.Close privateBeneathDirectory privateBeneathDup",
	// delete's admission, adoption, unseal and sweep, and the failed create's discard (LC-CKPT-WALK-1C3B): each binds
	// recovery/ (t.hold binds) before the walk calls it guards; the directory is removed after its identity is checked.
	"target":  "t.hold t.c.recoveryAbsent privateBeneathLstat os.Geteuid t.recovery.Open dir.Readdirnames dir.Close recoveryEntryCanonical",
	"adopt":   "t.c.checkpointStage t.hold privateBeneathDirectory t.retain",
	"unseal":  "t.c.checkpointStage t.bound privateBeneathRemove t.sync",
	"sweep":   "t.c.checkpointStage t.bound privateBeneathDirectory privateBeneathRemoveBelow t.c.checkpointStage t.bound checkpointTreeRemoveDirectory syncCheckpointRoot",
	"discard": "t.c.checkpointStage t.bound privateBeneathLstat syncCheckpointRoot privateBeneathIdentity privateBeneathLstat t.bound t.sweep privateBeneathLstat t.bound"}

// checkpointTreeWindowsCheck are the Windows twin's check conditions and results by text and count: today's walk of the
// inventory, an entry not planned or not of its planned kind (a directory, else a regular file) refused (N8 of 1C1R).
var checkpointTreeWindowsCheck = map[string]int{"if:err != nil": 1, `if:err != nil || relErr != nil || rel == "."`: 1,
	"if:!known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular())": 1, "return:0, err": 1,
	"return:errors.Join(err, relErr)": 1, "return:errCheckpointWitness": 1, "return:nil": 1, "return:seen, err": 1}

// The reader operations whose arguments or error results the order and identity counts
// alone cannot distinguish. The snippets match Go syntax printed from the parsed body
// (without comments), independent of source formatting and checkout line endings.
var checkpointTreeReaderPins = map[string]map[string]string{
	"checkpoint.go:recoveryAbsent": {
		"parent": "root, err := os.OpenRoot(c.cfg.Root + checkpointRootHere) if err != nil { return false } defer func() { _ = root.Close() }()",
		"name":   "_, err = root.Lstat(filepath.Base(c.recoveryDir())) return errors.Is(err, fs.ErrNotExist)",
	},
	"checkpoint.go:listCheckpoints": {
		"tree":   `tree := c.checkpointTree("") defer tree.close() names, directories, err := tree.listing()`,
		"absent": "if errors.Is(err, fs.ErrNotExist) && c.recoveryAbsent() { return nil, 0, nil }",
		"listed": `if err != nil { return nil, 0, checkpointStateError{reason: "boundary_unreadable"} } c.checkpointStage("listed")`,
	},
	"checkpoint.go:validateRecovery": {
		"absent": "if _, err := os.Lstat(c.recoveryDir()); errors.Is(err, fs.ErrNotExist) && c.recoveryAbsent() { return nil }",
		"tree":   `tree := c.checkpointTree("") defer tree.close() if tree.hold() != nil`,
		"hold":   `if tree.hold() != nil { return checkpointStateError{reason: "boundary_unreadable"} } return recoveryTreeFailure(tree.snapshot())`,
	},
	"checkpoint_tree_unix.go:close":  {"release": "for _, fd := range t.held { _ = unix.Close(fd) } t.held = nil"},
	"checkpoint_tree_unix.go:retain": {"dup": "{ dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0) if err == nil { t.held = append(t.held, dup) } return err }"},
	"checkpoint_tree_unix.go:listing": {
		"boundary":  `if t.recovery == nil { if err := t.hold(); err != nil { return nil, nil, err } } else if err := t.bound(); err != nil { return nil, nil, err }`,
		"open":      `dir, err := t.recovery.Open(".") if err != nil { return nil, nil, errPrivateLayout }`,
		"list":      "names, err := dir.Readdirnames(-1) var root unix.Stat_t if err != nil || unix.Fstat(int(dir.Fd()), &root) != nil { return nil, nil, errPrivateLayout } slices.Sort(names)",
		"type":      "info, err := privateBeneathLstat(t.recovery, name, nil) if err != nil { return nil, nil, errPrivateLayout }",
		"device":    "if stat.Dev != root.Dev { return nil, nil, errCheckpointState } directories[i] = info.IsDir()",
		"record":    "if directories[i] && checkpointIDPattern.MatchString(name) { t.made[name] = privateBeneathIdentity(stat)",
		"directory": "privateBeneathDirectory(t.recovery, name, t.want, func(fd int, _ privateBeneathID) error",
		"retain":    "return t.retain(fd) }) if err != nil { return nil, nil, errPrivateLayout }",
	},
	"checkpoint_tree_unix.go:witness": {
		"cut":       "f, err := t.open(ctx, path.Join(id, checkpointWitnessName)) if cut := (*leaseWaitCut)(nil); errors.As(err, &cut) { return checkpointWitness{}, false, cut } if err != nil {",
		"bound":     "if err != nil { if t.bound() != nil { return checkpointWitness{}, false, errCheckpointWitnessUnread }",
		"directory": "privateBeneathDirectory(t.recovery, id, t.want, func(fd int, parent privateBeneathID) error",
		"leaf":      "statErr := unix.Fstatat(fd, checkpointWitnessName, &stat, unix.AT_SYMLINK_NOFOLLOW)",
		"kind":      "if errors.Is(statErr, unix.ENOENT) || statErr == nil && uint64(stat.Dev) == parent.dev && stat.Mode&unix.S_IFMT != unix.S_IFREG { return nil } return errCheckpointWitnessUnread",
		"unread":    "}) if err != nil { return checkpointWitness{}, false, errCheckpointWitnessUnread } return checkpointWitness{}, false, nil",
		"handle":    "info, err := f.Stat() if err != nil { return checkpointWitness{}, false, errCheckpointWitnessUnread } if !info.Mode().IsRegular() { return checkpointWitness{}, false, nil } return checkpointWitnessRead(f, id)",
	},
	"checkpoint_tree_unix.go:snapshot": {
		"bind":      "if t.bound() != nil { return errPrivateLayout }",
		"parent":    "if unix.Fstat(fd, &self) != nil { return errPrivateLayout } uid := uint32(os.Geteuid()) if self.Uid != uid || self.Mode&0o777 != 0o700 { return errCheckpointState }",
		"list":      "names, err := dir.Readdirnames(-1) if err != nil { return errPrivateLayout } if checkpointTreeListed != nil { checkpointTreeListed(name) } for _, entry := range names",
		"entry":     "unix.Fstatat(fd, entry, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) { vanished = true return nil } else if err != nil || stat.Dev != self.Dev || stat.Uid != uid { return errCheckpointState }",
		"directory": "if stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&0o777 == 0o700 { child := path.Join(name, entry) t.made[child] = privateBeneathIdentity(&stat) directories = append(directories, child) } else if stat.Mode != unix.S_IFREG|0o600 { return errCheckpointState }",
		"root":      `if name == "." { dir, err := t.recovery.Open(".") if err != nil { return errPrivateLayout } err = scan(dir) _ = dir.Close() if err != nil { return err }`,
		"child":     "privateBeneathDirectory(t.recovery, name, t.want, func(fd int, _ privateBeneathID) error",
		"walk":      "return privateBeneathDup(fd, scan) }); err != nil { if errors.Is(err, errCheckpointState) || errors.Is(err, unix.EXDEV) || errors.Is(err, errPrivateBeneathMoved) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) { return errCheckpointState } return errPrivateLayout }",
	},
	// delete through the tree (LC-CKPT-WALK-1C3B), and the failed create's discard.
	"checkpoint.go:admitDeleteCheckpoint": {"tree": "tree := c.checkpointTree(options.CheckpointID) defer tree.close() if err := tree.target(); err != nil { return err } return nil }"},
	"checkpoint.go:deleteCheckpointLocked": {
		"adopt": "tree := c.checkpointTree(options.CheckpointID) defer tree.close() if tree.adopt() != nil { return errCheckpointState }",
		"steps": `{{"unseal", tree.unseal}, {"remove", tree.sweep}}`,
	},
	"checkpoint_tree_unix.go:target": {
		"boundary": "if err := t.hold(); errors.Is(err, fs.ErrNotExist) && t.c.recoveryAbsent() { return errCheckpointIdentity } else if err != nil { return errCheckpointState }",
		"absent":   "info, err := privateBeneathLstat(t.recovery, t.id, nil) if errors.Is(err, fs.ErrNotExist) { return errCheckpointIdentity } else if err != nil { return errCheckpointState }",
		"private":  "if stat := info.Sys().(*unix.Stat_t); stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o777 != 0o700 { return errCheckpointState }",
		"spelling": `dir, err := t.recovery.Open(".") if err != nil { return errCheckpointIdentity } names, err := dir.Readdirnames(-1) if closed := dir.Close(); err != nil || closed != nil || !recoveryEntryCanonical(names, t.id) { return errCheckpointIdentity } return nil }`,
	},
	"checkpoint_tree_unix.go:adopt":  {"record": "if err := t.hold(); err != nil { return err } return privateBeneathDirectory(t.recovery, t.id, nil, func(fd int, id privateBeneathID) error { if err := t.retain(fd); err != nil { return err } t.made[t.id] = id return nil }) }"},
	"checkpoint_tree_unix.go:unseal": {"remove": "else if err := privateBeneathRemove(t.recovery, t.name(checkpointWitnessName), t.want); err != nil { return err } return t.sync(\".\") }"},
	"checkpoint_tree_unix.go:sweep": {
		"recorded":  `{ if _, ok := t.made[t.id]; !ok { return errPrivateBeneathMoved } t.c.checkpointStage("sweep")`,
		"below":     "else if err := privateBeneathDirectory(t.recovery, t.id, t.want, func(fd int, _ privateBeneathID) error { return privateBeneathRemoveBelow(fd) }); err != nil { return err }",
		"directory": "else if err := checkpointTreeRemoveDirectory(t.recovery, t.id, t.made[t.id]); err != nil { return err } return syncCheckpointRoot(t.recovery) }",
	},
	"checkpoint_tree_unix.go:discard": {
		"empty":   "case errors.Is(err, fs.ErrNotExist) && !made: return syncCheckpointRoot(t.recovery) case err != nil || !made ||",
		"witness": "privateBeneathLstat(t.recovery, t.name(checkpointWitnessName), t.want); err == nil && witness.Mode().IsRegular() { if t.bound() != nil { return errCheckpointCopy } return nil } else if err != nil && !errors.Is(err, fs.ErrNotExist) { return errCheckpointCopy } if t.sweep() != nil { return errCheckpointCopy }",
	},
	"checkpoint_tree_other.go:target":   {"refuse": "return errCheckpointState"},
	"checkpoint_tree_other.go:adopt":    {"refuse": "return errCheckpointState"},
	"checkpoint_tree_other.go:unseal":   {"refuse": "return errCheckpointState"},
	"checkpoint_tree_other.go:sweep":    {"refuse": "return errCheckpointState"},
	"checkpoint_tree_windows.go:close":  {"roots": "closeCheckpointRoots(t.recovery, t.checkpoint)"},
	"checkpoint_tree_windows.go:adopt":  {"held": "t.recovery, t.checkpoint, err = t.c.holdCheckpoint(t.id) return err"},
	"checkpoint_tree_windows.go:unseal": {"held": "return t.c.unsealCheckpointHeld(t.recovery, t.checkpoint, t.id)"},
	"checkpoint_tree_windows.go:sweep":  {"held": "return t.c.removeCheckpointHeld(t.recovery, t.checkpoint, t.id)"},
	"checkpoint_tree_windows.go:target": {
		"name":     "if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) { return errCheckpointIdentity } else if err != nil || privateDir(dir, false) != nil { return errCheckpointState }",
		"spelling": "entries, err := os.ReadDir(t.c.recoveryDir())",
		"canon":    "if err != nil || !recoveryEntryCanonical(names, t.id) { return errCheckpointIdentity } return nil",
	},
	"checkpoint_tree_other.go:listing":  {"refuse": "return nil, nil, errCheckpointState"},
	"checkpoint_tree_other.go:witness":  {"refuse": "return checkpointWitness{}, false, errCheckpointState"},
	"checkpoint_tree_other.go:snapshot": {"refuse": "return errCheckpointState"},
	"checkpoint_tree_windows.go:listing": {
		"list": "entries, err := os.ReadDir(t.c.recoveryDir()) if err != nil { return nil, nil, err }", "type": "names[i], directories[i] = entry.Name(), entry.IsDir()",
	},
	"checkpoint_tree_windows.go:witness": {
		"read": "dir, err := t.c.checkpointDir(id) if err != nil { return checkpointWitness{}, false, err } return checkpointComplete(ctx, dir, id)",
	},
	"checkpoint_tree_windows.go:snapshot": {
		"walk": "return validatePrivateTreeSnapshot(t.c.recoveryDir(), filepath.WalkDir, validateRecoveryObject, os.Lstat)",
	},
}

func checkpointTreeReaders(parsed map[string]*ast.File, failures map[string]bool) {
	for site, pins := range checkpointTreeReaderPins {
		name, owner, _ := strings.Cut(site, ":")
		var body bytes.Buffer
		for _, decl := range checkpointTreeDecls(parsed, name, failures) {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == owner {
				_ = printer.Fprint(&body, token.NewFileSet(), fn.Body)
			}
		}
		text := strings.Join(strings.Fields(body.String()), " ")
		for key, snippet := range pins {
			failures["checkpoint_reader:"+site+":"+key] = strings.Count(text, snippet) != 1
		}
	}
}

// checkpointTreeSyncs reports whether expression is a function literal whose descriptor parameter fd is its first and
// whose one statement returns privateBeneathSync(fd).
func checkpointTreeSyncs(expression ast.Expr) bool {
	body, ok := expression.(*ast.FuncLit)
	if !ok || len(body.Type.Params.List) == 0 || len(body.Type.Params.List[0].Names) == 0 || body.Type.Params.List[0].Names[0].Name != "fd" || len(body.Body.List) != 1 {
		return false
	}
	result, ok := body.Body.List[0].(*ast.ReturnStmt)
	return ok && len(result.Results) == 1 && types.ExprString(result.Results[0]) == "privateBeneathSync(fd)"
}

// checkpointInventoryReader: inventoryCheckpoint reads by pathname (ops.open) only when a test set checkpointTest.open,
// and through its tree otherwise. opened is defined once, as exactly that condition, and never assigned again; the one
// if on opened holds the one ops.open in its body and the one tree.open in its else (O10b of 1C1R).
func checkpointInventoryReader(parsed map[string]*ast.File, failures map[string]bool) {
	held := false
	for _, declaration := range checkpointTreeDecls(parsed, "checkpoint.go", failures) {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "inventoryCheckpoint" || fn.Body == nil {
			continue
		}
		readers := func(node ast.Node) (pathname, tree int) {
			for inner := range ast.Preorder(node) {
				if selector, ok := inner.(*ast.SelectorExpr); ok {
					pathname, tree = pathname+map[bool]int{true: 1}[types.ExprString(selector) == "ops.open"], tree+map[bool]int{true: 1}[types.ExprString(selector) == "tree.open"]
				}
			}
			return pathname, tree
		}
		defined, assigned, branches := 0, 0, 0
		for node := range ast.Preorder(fn.Body) {
			if assign, ok := node.(*ast.AssignStmt); ok && slices.ContainsFunc(assign.Lhs, func(target ast.Expr) bool { return types.ExprString(target) == "opened" }) {
				assigned++
				if assign.Tok == token.DEFINE && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 && types.ExprString(assign.Rhs[0]) == "c.checkpointTest != nil && c.checkpointTest.open != nil" {
					defined++
				}
			} else if branch, ok := node.(*ast.IfStmt); ok && types.ExprString(branch.Cond) == "opened" && branch.Init == nil {
				pathname, tree := readers(branch.Body)
				otherwise, isBlock := branch.Else.(*ast.BlockStmt)
				if isBlock && pathname == 1 && tree == 0 {
					if pathname, tree = readers(otherwise); pathname == 0 && tree == 1 {
						branches++
					}
				}
			}
		}
		pathname, tree := readers(fn.Body)
		held = defined == 1 && assigned == 1 && branches == 1 && pathname == 1 && tree == 1
	}
	failures["checkpoint_inventory_reader"] = !held
}

// checkpointTreeWindowsCalls is every call of checkpoint_tree_windows.go by function and count. The twin keeps today's
// guarded pathname calls by design, so no source rule can show they never follow a link or leave the volume; this pins
// which functions it calls and how often, so a new or different call (os.Stat for os.Lstat) fails until the count is
// changed on purpose, and rule (b) roots each writer's path in a checkpointDir value. Arguments are not pinned.
var checkpointTreeWindowsCalls = map[string]int{"path.Join": 2, "t.c.checkpointDir": 10, "os.Lstat": 2, "filepath.Join": 8, "filepath.FromSlash": 8, "privateDir": 2,
	"createPrivateTemp": 1, "filepath.Base": 1, "created.Name": 1, "publishPrivateFile": 1, "syncCheckpointTreeDirectory": 1, "t.name": 1, "syncDirectory": 1,
	"t.c.discardOwnCheckpoint": 1, "openPrivateReader": 1, "filepath.WalkDir": 1, "filepath.Rel": 1, "errors.Join": 1, "filepath.ToSlash": 1, "entry.IsDir": 2, "entry.Name": 2, "len": 3, "make": 3, "os.ReadDir": 2,
	"t.c.recoveryDir": 3, "checkpointComplete": 1, "validatePrivateTreeSnapshot": 1,
	"entry.Type": 1, "entry.Type().IsRegular": 1, "writeAtomicBytes": 1,
	"errors.Is": 1, "append": 1, "recoveryEntryCanonical": 1, "closeCheckpointRoots": 1, "t.c.holdCheckpoint": 1, "t.c.unsealCheckpointHeld": 1, "t.c.removeCheckpointHeld": 1}

// checkpointTreeContract: each walk call of the tree on Linux and macOS passes t.want but checkpointTreeNilWants, and
// names no walk function with a want as a value; it binds as checkpointTreeBinds, open, check and record in the order of
// checkpointTreeOrders; the tree's mkdir hands its parent sync to the seam exactly once; record syncs its temporary and
// the directory it reached once each; the Windows twin calls exactly checkpointTreeWindowsCalls and its check decides as
// checkpointTreeWindowsCheck.
func checkpointTreeContract(parsed map[string]*ast.File, failures map[string]bool) {
	wants, nils, calls, binds, parentSyncs := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, 0
	orders, recordSyncs, windowsCheck := map[string][]string{}, map[string]int{}, map[string]int{}
	for _, declaration := range checkpointTreeDecls(parsed, "private_beneath_unix.go", failures) {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv == nil {
			index := 0
			for _, field := range fn.Type.Params.List {
				for range max(len(field.Names), 1) {
					if types.ExprString(field.Type) == "privateBeneathWant" {
						wants[fn.Name.Name] = index
					}
					index++
				}
			}
		}
	}
	for _, name := range []string{"checkpoint_tree_unix.go", "checkpoint_tree_windows.go"} {
		for _, declaration := range checkpointTreeDecls(parsed, name, failures) {
			owner, functions := "", map[ast.Expr]bool{}
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
			}
			for node := range ast.Preorder(declaration) {
				call, ok := node.(*ast.CallExpr)
				if identifier, named := node.(*ast.Ident); named && !functions[identifier] && name == "checkpoint_tree_unix.go" {
					_, walks := wants[identifier.Name]
					failures["checkpoint_tree_want_value:"+owner+":"+identifier.Name] = failures["checkpoint_tree_want_value:"+owner+":"+identifier.Name] || walks
				}
				if branch, decides := node.(*ast.IfStmt); decides && name == "checkpoint_tree_windows.go" && owner == "check" {
					windowsCheck["if:"+types.ExprString(branch.Cond)]++
				} else if result, returns := node.(*ast.ReturnStmt); returns && name == "checkpoint_tree_windows.go" && owner == "check" {
					results := []string{}
					for _, value := range result.Results {
						results = append(results, types.ExprString(value))
					}
					windowsCheck["return:"+strings.Join(results, ", ")]++
				}
				if !ok {
					continue
				}
				function := types.ExprString(call.Fun)
				functions[call.Fun] = true
				if order, ordered := checkpointTreeOrders[owner]; ordered && name == "checkpoint_tree_unix.go" && (strings.HasPrefix(function, "privateBeneath") || slices.Contains(strings.Fields(order), function) ||
					slices.Contains([]string{"t.bound", "t.createTemp", "syncFile", "waitOutLease"}, function)) {
					orders[owner] = append(orders[owner], function)
				}
				if owner == "record" && name == "checkpoint_tree_unix.go" && function == "syncFile" && len(call.Args) == 1 && types.ExprString(call.Args[0]) == "temporary" {
					recordSyncs["file"]++
				} else if owner == "record" && name == "checkpoint_tree_unix.go" && function == "privateBeneathDirectory" && len(call.Args) == 4 &&
					types.ExprString(call.Args[0]) == "t.recovery" && types.ExprString(call.Args[1]) == "t.name(path.Dir(name))" && types.ExprString(call.Args[2]) == "t.want" &&
					checkpointTreeSyncs(call.Args[3]) {
					recordSyncs["directory"]++
				}
				if name == "checkpoint_tree_windows.go" {
					calls[function]++
				} else if function == "t.bound" {
					binds[owner]++
				} else if index, walks := wants[function]; walks && index < len(call.Args) && types.ExprString(call.Args[index]) == "nil" {
					nils[owner+":"+function]++
				} else if walks && (index >= len(call.Args) || types.ExprString(call.Args[index]) != "t.want") {
					failures["checkpoint_tree_want:"+owner+":"+function] = true
				}
				if owner != "mkdir" || function != "syncCheckpointTreeParent" || len(call.Args) != 2 {
					continue
				}
				if body, ok := call.Args[1].(*ast.FuncLit); ok && len(body.Body.List) == 1 {
					if result, ok := body.Body.List[0].(*ast.ReturnStmt); ok && len(result.Results) == 1 && types.ExprString(result.Results[0]) == "privateBeneathSync(fd)" {
						parentSyncs++
					}
				}
			}
		}
	}
	for _, key := range slices.Concat(slices.Collect(maps.Keys(nils)), slices.Collect(maps.Keys(checkpointTreeNilWants))) {
		failures["checkpoint_tree_want:"+key] = failures["checkpoint_tree_want:"+key] || nils[key] != checkpointTreeNilWants[key]
	}
	for _, function := range slices.Concat(slices.Collect(maps.Keys(calls)), slices.Collect(maps.Keys(checkpointTreeWindowsCalls))) {
		failures["checkpoint_tree_windows_call:"+function] = calls[function] != checkpointTreeWindowsCalls[function]
	}
	for _, owner := range slices.Concat(slices.Collect(maps.Keys(binds)), slices.Collect(maps.Keys(checkpointTreeBinds))) {
		failures["checkpoint_tree_bind:"+owner] = binds[owner] != checkpointTreeBinds[owner]
	}
	failures["checkpoint_tree_parent_sync"] = parentSyncs != 1
	for owner, order := range checkpointTreeOrders {
		failures["checkpoint_tree_order:"+owner] = strings.Join(orders[owner], " ") != order
	}
	failures["checkpoint_tree_record_sync:file"], failures["checkpoint_tree_record_sync:directory"] = recordSyncs["file"] != 1, recordSyncs["directory"] != 1
	for _, text := range slices.Concat(slices.Collect(maps.Keys(windowsCheck)), slices.Collect(maps.Keys(checkpointTreeWindowsCheck))) {
		failures["checkpoint_tree_windows_check:"+text] = windowsCheck[text] != checkpointTreeWindowsCheck[text]
	}
	checkpointTreeReaders(parsed, failures)
	checkpointInventoryReader(parsed, failures)
}

// checkpointTreeDecls are the declarations of the parsed file name; a missing file is a key.
func checkpointTreeDecls(parsed map[string]*ast.File, name string, failures map[string]bool) []ast.Decl {
	if failures["checkpoint_tree_parse:"+name] = parsed[name] == nil; parsed[name] == nil {
		return nil
	}
	return parsed[name].Decls
}

// checkpointDocumentReference follows local aliases of a document name.
func checkpointDocumentReference(values map[any][]ast.Expr, expression ast.Expr, document string, depth int) bool {
	if depth >= 8 {
		return false
	}
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || found {
			return !found
		}
		if identifier.Name == document {
			found = true
		} else if identifier.Obj != nil {
			for _, value := range values[identifier.Obj] {
				if value != nil && checkpointDocumentReference(values, value, document, depth+1) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// checkpointRootMethods are os.Root and os.File method names. In
// the root files every call of one, or of an os function, is one of
// checkpointRootCalls, by text and exact count.
var checkpointRootMethods = []string{"Open", "Create", "OpenFile", "OpenRoot", "Chmod", "Mkdir", "MkdirAll", "Chown", "Lchown", "Chtimes",
	"Remove", "RemoveAll", "Stat", "Lstat", "Readlink", "Rename", "Link", "Symlink", "ReadFile", "WriteFile", "Close", "Name", "FS",
	"Write", "WriteAt", "WriteString", "Truncate"}
var checkpointRootCalls = map[string]int{"os.OpenRoot(c.recoveryDir() + checkpointRootHere)": 1,
	"recovery.OpenRoot(checkpointID + checkpointRootHere)": 1, `recovery.Stat(".")`: 1, "os.Lstat(c.recoveryDir())": 1,
	"os.SameFile(recoveryHeld, recoveryNow)": 1, `checkpoint.Stat(".")`: 1, "recovery.Lstat(checkpointID)": 2,
	"os.SameFile(checkpointHeld, checkpointNow)": 1, "checkpoint.Remove(checkpointWitnessName)": 1, "checkpoint.Lstat(checkpointWitnessName)": 1,
	`checkpoint.Open(".")`: 1, "listing.Close()": 1, "checkpoint.RemoveAll(names[index])": 1, "recovery.Remove(checkpointID)": 2,
	`held.Open(".")`: 1, "handle.Close()": 1, "checkpoint.Close()": 1, "recovery.Close()": 1}

// The root files import only checkpointRootImports, none renamed, names of
// a package only checkpointRootMembers, calls besides typed root calls (f) only
// checkpointRootCallees, each by the declaration of its first identifier, assigns
// only to identifiers, and names other declarations only as checkpointRootNames.
var checkpointRootImports = []string{"errors", "io/fs", "os", "path/filepath", "runtime"}
var checkpointRootMembers = map[string][]string{"errors": {"Is", "New"}, "fs": {"ErrNotExist", "ModeSymlink"}, "filepath": {"Separator"},
	"os": {"Lstat", "OpenRoot", "Root", "SameFile"}, "runtime": {"GOOS"}}
var checkpointRootFiles = []string{"checkpoint_root.go", "checkpoint_root_windows.go"}

// checkpointTreeBypassed are the pathname creators, publishers, syncs, stats, removers, walks, readers and record
// writers the checkpoint tree replaces: checkpoint_tree_unix.go names none, and checkpoint.go calls none (privateDir
// creating) on a path rooted in a checkpointDir value, or joining recovery/ with a name that is not a literal
// (checkpointTreeBypassRooted), but checkpointTreeBypassKept, by exact count: the allocation refusal before recovery/ is
// held (delete's admission reads through the tree). A key is checkpoint_tree_bypass:<file>:<function>.
var checkpointReaderBypassed = []string{"os.ReadDir", "filepath.WalkDir", "openRegular", "checkpointComplete", "validatePrivateTreeSnapshot"}
var checkpointTreeBypassed = slices.Concat(checkpointReaderBypassed, []string{"privateDir", "createPrivateTemp", "publishPrivateFile", "syncDirectory", "os.Lstat", "os.Stat", "os.Mkdir", "os.Rename", "os.Remove",
	"openPrivateReader", "writeAtomic", "writeAtomicBytes"})
var checkpointTreeBypassKept = map[string]int{"checkpointLocked:os.Lstat": 1}
var checkpointRootCallees = map[string]string{"c.checkpointStage": "receiver", "c.checkpointTest.beforeStep": "receiver", "c.recoveryDir": "receiver", "c.holdRecovery": "receiver",
	"c.checkpointDir": "receiver", "c.holdCheckpoint": "receiver", "c.checkpointRootsBound": "receiver", "c.removeCheckpointHeld": "receiver",
	"closeCheckpointRoots": "package", "syncCheckpointRoot": "package", "string": "universe", "handle.Sync": `held.Open(".")`,
	"listing.Readdirnames": `checkpoint.Open(".")`, "left.Mode": "recovery.Lstat(checkpointID)"}
var checkpointRootNames = []string{"Cell", "checkpointWitnessName", "errCheckpointCopy", "checkpointRootHere", "errCheckpointRootMoved"}

// checkpointRootReceivers are the declarations, besides an *os.Root field, of a
// typed root call's receiver; "(ast: <nil>)" is a later name of a multi-value one.
var checkpointRootReceivers = []string{"c.holdCheckpoint(checkpointID)", "(ast: <nil>)", `checkpoint.Open(".")`, `held.Open(".")`}

// checkpointRootDeclared is how the first operand of expression is declared:
// "universe", "package" (a root file's scope), "receiver" (the enclosing
// *Cell method's, never assigned), else its type if a field, then every value
// assigned to it; an operand that is not an identifier is its own text.
func checkpointRootDeclared(parsed map[string]*ast.File, file *ast.File, declaration ast.Decl, expression ast.Expr) string {
	for selector, ok := expression.(*ast.SelectorExpr); ok; selector, ok = expression.(*ast.SelectorExpr) {
		expression = selector.X
	}
	identifier, ok := expression.(*ast.Ident)
	declared, values := "", checkpointAssignments(file)
	switch {
	case !ok:
		return types.ExprString(expression)
	case identifier.Obj == nil && slices.ContainsFunc(checkpointRootFiles, func(root string) bool { return parsed[root].Scope.Lookup(identifier.Name) != nil }):
		return "package"
	case identifier.Obj == nil && slices.ContainsFunc(slices.Collect(maps.Values(parsed)), func(file *ast.File) bool { return file.Scope.Lookup(identifier.Name) != nil }):
		return "elsewhere"
	case identifier.Obj == nil:
		return "universe"
	case file.Scope.Lookup(identifier.Name) == identifier.Obj:
		return "package"
	}
	if field, ok := identifier.Obj.Decl.(*ast.Field); ok {
		declared = types.ExprString(field.Type)
		if function, _ := declaration.(*ast.FuncDecl); function != nil && function.Recv != nil && function.Recv.List[0] == field && declared == "*Cell" && len(values[identifier.Obj]) == 0 {
			return "receiver"
		}
	}
	for _, value := range values[identifier.Obj] {
		declared = strings.TrimSpace(declared + " " + types.ExprString(value))
	}
	return declared
}

// checkpointRootClosed applies the closed lists above to a root file.
func checkpointRootClosed(parsed map[string]*ast.File, file *ast.File, failures map[string]bool) {
	methods := map[string]bool{}
	for _, file := range parsed {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Recv != nil {
				methods[function.Name.Name] = true
			}
		}
	}
	isPackage := func(base ast.Expr) bool { identifier, ok := base.(*ast.Ident); return ok && identifier.Obj == nil }
	for _, spec := range file.Imports {
		if path := checkpointLiteral(spec.Path); spec.Name != nil || !slices.Contains(checkpointRootImports, path) {
			failures["checkpoint_root_import:"+path] = true
		}
	}
	for _, declaration := range file.Decls {
		owner, parts, functions := "", map[ast.Node]bool{}, map[ast.Node]bool{}
		if function, ok := declaration.(*ast.FuncDecl); ok {
			owner = function.Name.Name
		}
		// A parent is visited first: a call marks its function, a selector its parts.
		ast.Inspect(declaration, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if parts[node.X], parts[node.Sel] = true, true; isPackage(node.X) {
					if !slices.Contains(checkpointRootMembers[types.ExprString(node.X)], node.Sel.Name) {
						failures["checkpoint_root_member:"+owner+":"+types.ExprString(node)] = true
					}
				} else if methods[node.Sel.Name] && !functions[node] {
					failures["checkpoint_root_name:"+owner+":"+types.ExprString(node)] = true
				}
			case *ast.CallExpr:
				functions[node.Fun] = true
				selector, typed := node.Fun.(*ast.SelectorExpr)
				if typed {
					typed = isPackage(selector.X) || slices.Contains(checkpointRootMethods, selector.Sel.Name)
				}
				if typed && !isPackage(selector.X) {
					declared := checkpointRootDeclared(parsed, file, declaration, selector.X)
					failures["checkpoint_root_receiver:"+owner+":"+types.ExprString(node.Fun)+"="+declared] = !strings.HasPrefix(declared, "*os.Root") &&
						!slices.Contains(checkpointRootReceivers, declared)
				}
				_, literal := node.Fun.(*ast.FuncLit)
				callee := types.ExprString(node.Fun)
				if want, listed := checkpointRootCallees[callee]; !literal && !typed && !listed {
					failures["checkpoint_root_callee:"+owner+":"+callee] = true
				} else if listed && !typed {
					declared := checkpointRootDeclared(parsed, file, declaration, node.Fun)
					failures["checkpoint_root_declared:"+owner+":"+callee+"="+declared] = declared != want
				}
			case *ast.AssignStmt:
				for _, target := range node.Lhs {
					if _, plain := target.(*ast.Ident); !plain {
						failures["checkpoint_root_assign:"+owner+":"+types.ExprString(target)] = true
					}
				}
			case *ast.RangeStmt:
				failures["checkpoint_root_assign:"+owner+":range"] = failures["checkpoint_root_assign:"+owner+":range"] || node.Tok == token.ASSIGN
			case *ast.Ident:
				if node.Obj == nil && !parts[node] && !functions[node] && !slices.Contains(checkpointRootNames, node.Name) &&
					slices.ContainsFunc(slices.Collect(maps.Values(parsed)), func(other *ast.File) bool { return other.Scope.Lookup(node.Name) != nil }) {
					failures["checkpoint_root_name:"+owner+":"+node.Name] = true
				}
			}
			return true
		})
	}
}

// checkpointSystemSelectors are the only syscall, unix and windows selectors
// but the functions of drillReserveCalls' listed calls.
var checkpointSystemSelectors = map[string][]string{"checkpoint_unix.go": {"syscall.Statfs_t", "syscall.Statfs", "syscall.Stat_t", "syscall.O_NONBLOCK"},
	"checkpoint_windows.go":  {"windows.UTF16PtrFromString", "windows.GetDiskFreeSpaceEx"},
	"drill_storage_linux.go": {"unix.Statfs_t", "unix.Fstatfs"}, "drill_storage_darwin.go": {"unix.Statfs_t", "unix.Fstatfs"},
	"drill_storage_windows.go": {"windows.Handle", "windows.NewLazySystemDLL", "windows.ByHandleFileInformation", "windows.GetVolumeInformationByHandle",
		"windows.GetFileInformationByHandle", "windows.IO_STATUS_BLOCK", "windows.UTF16ToString", "windows.FILE_ATTRIBUTE_COMPRESSED"},
	"drill_reserve_linux.go": {"unix.ENOSPC", "unix.EDQUOT", "unix.EFBIG", "unix.EOPNOTSUPP", "unix.SYS_IOCTL", "unix.EINVAL", "unix.EBADF", "unix.EXDEV",
		"unix.Stat_t", "unix.O_NOFOLLOW", "unix.O_DIRECTORY", "unix.O_CLOEXEC", "unix.O_WRONLY", "unix.O_CREAT", "unix.O_EXCL", "unix.O_RDONLY", "unix.O_NONBLOCK",
		"unix.S_IFMT", "unix.S_IFREG", "unix.O_NOCTTY"},
	"drill_reserve_windows.go": {"windows.Handle", "windows.ERROR_INVALID_NAME", "windows.ERROR_DISK_FULL", "windows.ERROR_HANDLE_DISK_FULL",
		"windows.ERROR_DISK_QUOTA_EXCEEDED", "windows.ERROR_FILE_TOO_LARGE", "windows.ByHandleFileInformation", "windows.FileStandardInfo"},
	"checkpoint_tree_unix.go": {"unix.EINVAL", "unix.Stat_t", "unix.Fstatat", "unix.AT_SYMLINK_NOFOLLOW", "unix.S_IFMT", "unix.S_IFDIR", "unix.Unlinkat", "unix.AT_REMOVEDIR",
		"unix.Close", "unix.ELOOP", "unix.ENOENT", "unix.ENOTDIR", "unix.ENOTEMPTY", "unix.EXDEV", "unix.F_DUPFD_CLOEXEC", "unix.FcntlInt", "unix.Fstat", "unix.S_IFREG"}}

// drillReserveCalls are the reservation files' system, runtimeconfig and name-resolving calls by file, exact
// text and count: rule (d) admits a system function there only as the function
// of a listed call, and a count that differs is drill_reserve_call:<file>:<text>.
var drillReserveCalls = map[string]map[string]int{"drill_reserve_linux.go": {"unix.Fstat(dir, &device)": 1, "unix.Fstat(next, &stat)": 1, "unix.Close(next)": 1,
	"unix.Openat(dir, part, unix.O_NOFOLLOW | unix.O_DIRECTORY | unix.O_CLOEXEC, 0)": 1, "unix.Mkdirat(dir, last, 0o700)": 1,
	"unix.Fallocate(int(f.Fd()), 0, int64(offset), int64(length))": 1, "unix.Syscall(unix.SYS_IOCTL, uintptr(fd), drillFiemapIoctl, uintptr(unsafe.Pointer(&m)))": 1,
	"unix.Openat(dir, last, unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC, 0o600)": 1, "unix.Fstat(dir, &parent)": 1, "unix.Fstat(fd, &stat)": 1,
	"unix.Openat(dir, last, unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC | kind, 0)": 1, "unix.Close(fd)": 1,
	`root.Open(".")`: 1, "f.Stat()": 1, `os.NewFile(uintptr(fd), "drill reserve file")`: 2},
	"drill_reserve_windows.go": {"runtimeconfig.WindowsPrivateBeneath(root, name, true, true)": 1, "runtimeconfig.WindowsPrivateBeneath(drill, name, true, false)": 1,
		"runtimeconfig.WindowsPrivateBeneath(root, name, false, directory)": 1, "windows.Ftruncate(windows.Handle(f.Fd()), int64(offset + length))": 1,
		"windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard)))": 1,
		"windows.GetFileInformationByHandle(handle, &info)": 1, "windows.GetFileInformationByHandle(windows.Handle(held.Fd()), &info)": 1},
	"drill_reserve.go": {"parent.OpenRoot(filepath.FromSlash(name) + drillRootHere)": 1, `root.Stat(".")`: 1, "f.Stat()": 1, "os.Lstat(c.recoveryDir())": 1,
		`boundary.Open(".")`: 1, "reopened.Stat()": 1, "drill.Lstat(drillReservePending)": 1, "drill.Lstat(drillRootRecordName)": 1, "f.Write(data)": 1,
		"drill.Rename(drillReservePending, drillRootRecordName)": 1, "drill.Remove(drillReservePending)": 1, "drillRootNames(drill)": 1,
		"f.Sync()": 2, "drillRootSync(boundary)": 1, "drillRootSync(drill)": 2, "drillRootSync(sub)": 1}} // G1: every fsync and directory sync

// drillReserveNamed are the methods that resolve or change a name; in every reservation
// file each such call must be a listed text and none is a value (drill_reserve_unlisted:<file>:<text>).
var drillReserveNamed = []string{"Open", "OpenFile", "OpenRoot", "Stat", "Lstat", "Readlink", "ReadDir", "ReadFile", "Rename", "Remove", "RemoveAll",
	"Mkdir", "MkdirAll", "Create", "Link", "Symlink", "Chmod", "Chown", "Lchown", "Chtimes", "Truncate", "Write", "WriteAt", "WriteString", "WriteFile"}

// drillReserveRootSteps are drillReserveRoot's assignments of err and its returns, in order (P1): only
// the anchor's error is returned as it came, every other refusal is fixed (drill_reserve_root_returns).
var drillReserveRootSteps = []string{"drillPlatformOpen(parent, name, true)", "return nil, err", "parent.OpenRoot(filepath.FromSlash(name) + drillRootHere)",
	`return nil, drillStorageError{reason: "boundary_refused"}`, `root.Stat(".")`, `return nil, drillStorageError{reason: "boundary_refused"}`, "return root, nil"}

// drillReserveFileProof are drillReserveFile's last statements, in order (G5): the reopen through the walk, the identity
// compared while the writer is open, the writer closed, then the proof on the reopened handle (drill_reserve_file_proof).
var drillReserveFileProof = []string{`c.drillRootStage("reserve_verify")`, "reopened, err := drillPlatformOpen(drill, name, false)",
	`if err != nil { _ = f.Close() return "allocation_unproved" }`, "defer func() { _ = reopened.Close() }()", "info, err := reopened.Stat()",
	"same := drillReserveSame(f, info, err)", `if closed := f.Close(); !same || closed != nil || drillPlatformVerify(reopened, size) != nil { return "allocation_unproved" }`, `return ""`}

// drillReserveFileTail is the last n statements of drillReserveFile, each with its white space collapsed.
func drillReserveFileTail(file *ast.File, source string, n int) (tail []string) {
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "drillReserveFile" && fn.Body != nil {
			for _, statement := range fn.Body.List[max(0, len(fn.Body.List)-n):] {
				tail = append(tail, strings.Join(strings.Fields(strings.ReplaceAll(source, "\r\n", "\n")[statement.Pos()-1:statement.End()-1]), " "))
			}
		}
	}
	return tail
}

// drillRootShared are the drill_root.go declarations another checkpoint file may name.
var drillRootShared = map[string][]string{"drill_storage.go": {"drillRootHere", "drillRootNames"},
	"drill_reserve.go": {"drillRootHere", "drillRootNames", "drillRootRecordName", "drillRootStage", "drillRootSync", "deleteDrillLocked"}}

// (d) no drill storage file selects a drillStorageWrites name, called or not, but
// (b)'s os.OpenRoot, names a drillStorageHelpers name, or resolves another ntdll procedure.
var drillStorageWrites = []string{"Mkdir", "MkdirAll", "OpenFile", "OpenRoot", "Create", "CreateTemp", "WriteFile", "Write", "WriteAt", "WriteString",
	"Rename", "Remove", "RemoveAll", "Truncate", "Ftruncate", "Fallocate", "FcntlFstore", "SetFileInformationByHandle", "Sync", "Chmod", "Chown", "Link", "Symlink"}
var drillStorageHelpers = []string{"writeAtomic", "writeAtomicBytes", "createPrivateTemp", "privateDir", "publishPrivateFile", "deleteDrillLocked", "journal",
	"journalCause", "begin", "step"}

const drillStorageProc = `windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryVolumeInformationFile")`

// checkpointNamesRemoval reports a pathname removal selector inside node.
func checkpointNamesRemoval(node ast.Node) (found bool) {
	ast.Inspect(node, func(inner ast.Node) bool {
		selector, ok := inner.(*ast.SelectorExpr)
		found = found || ok && slices.Contains([]string{"os.Remove", "os.RemoveAll", "runtimeconfig.WindowsPrivateRemove"}, types.ExprString(selector))
		return !found
	})
	return found
}

// checkpointTreeRecordWrite is the one write rule (h) admits: in checkpoint_tree_unix.go, (*checkpointTree).record's
// temporary.Write, temporary defined once there as the file t.createTemp made and never assigned again; nil otherwise.
func checkpointTreeRecordWrite(name string, file *ast.File) (written *ast.SelectorExpr) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if name != "checkpoint_tree_unix.go" || !ok || function.Name.Name != "record" || updateReceiverName(function) != "checkpointTree" || function.Body == nil {
			continue
		}
		writes, assigned, made := []*ast.SelectorExpr(nil), 0, (*ast.AssignStmt)(nil)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && types.ExprString(selector) == "temporary.Write" {
				writes = append(writes, selector)
			} else if assign, ok := node.(*ast.AssignStmt); ok && slices.ContainsFunc(assign.Lhs, func(target ast.Expr) bool { return types.ExprString(target) == "temporary" }) {
				assigned, made = assigned+1, assign
			}
			return true
		})
		if len(writes) != 1 || assigned != 1 || made.Tok != token.DEFINE || len(made.Rhs) != 1 || types.ExprString(made.Lhs[0]) != "temporary" {
			return nil
		}
		identifier, ok := writes[0].X.(*ast.Ident)
		if call, isCall := made.Rhs[0].(*ast.CallExpr); ok && isCall && types.ExprString(call.Fun) == "t.createTemp" && identifier.Obj != nil && identifier.Obj.Decl == made {
			written = writes[0]
		}
	}
	return written
}

// checkpointHeldContract applies the held-root rules (b)-(h) to every
// checkpoint file but drill_root.go, whose own rules stand.
func checkpointHeldContract(parsed map[string]*ast.File, failures map[string]bool) {
	// (b) package functions, Cell methods and package variables declared
	// outside the checkpoint files that name a pathname removal.
	removers := map[string]bool{}
	for name, file := range parsed {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && !slices.Contains(checkpointFiles, name) && (function.Recv == nil || updateReceiverName(function) == "Cell") {
				removers[function.Name.Name] = removers[function.Name.Name] || checkpointNamesRemoval(function)
			} else if group, ok := declaration.(*ast.GenDecl); ok && !slices.Contains(checkpointFiles, name) {
				for _, spec := range group.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok && checkpointNamesRemoval(value) {
						for _, identifier := range value.Names {
							removers[identifier.Name] = true
						}
					}
				}
			}
		}
	}
	counted := map[string]int{}
	for name, file := range parsed {
		if !slices.Contains(checkpointFiles, name) || name == "drill_root.go" {
			continue
		}
		for _, spec := range file.Imports {
			if path := checkpointLiteral(spec.Path); spec.Name != nil && (slices.Contains([]string{"os", "syscall", "golang.org/x/sys/unix", "golang.org/x/sys/windows"}, path) ||
				path == "github.com/witwave-ai/witself/internal/runtimeconfig" && strings.HasPrefix(name, "drill_reserve")) {
				failures["system_import:"+name+":"+path] = true // (d)
			}
		}
		rootFile, uses, written := slices.Contains(checkpointRootFiles, name), map[string]int{}, checkpointTreeRecordWrite(name, file)
		selected, inventoried, opens, reserved := map[*ast.Ident]bool{}, map[ast.Node]bool{}, map[ast.Node]bool{}, map[ast.Node]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				// Only the first assignment of the boundary absence proof opens the cell root.
				if name == "checkpoint.go" && node.Name.Name == "recoveryAbsent" && updateReceiverName(node) == "Cell" && node.Body != nil && len(node.Body.List) > 0 {
					if assignment, ok := node.Body.List[0].(*ast.AssignStmt); ok && len(assignment.Rhs) == 1 {
						if call, ok := assignment.Rhs[0].(*ast.CallExpr); ok && types.ExprString(call) == "os.OpenRoot(c.cfg.Root + checkpointRootHere)" {
							opens[call.Fun] = true
						}
					}
				}
			case *ast.CallExpr:
				text := types.ExprString(node)
				opens[node.Fun] = opens[node.Fun] || name == "checkpoint_unix.go" && text == `os.OpenRoot(c.path("pg") + checkpointRootHere)` || rootFile && text == "os.OpenRoot(c.recoveryDir() + checkpointRootHere)" ||
					(name == "drill_storage.go" || name == "drill_reserve.go") && text == "os.OpenRoot(c.recoveryDir() + drillRootHere)"
				reserved[node.Fun] = drillReserveCalls[name][text] != 0 // a listed call marks its function for (d)
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok && rootFile && (types.ExprString(selector.X) == "os" || slices.Contains(checkpointRootMethods, selector.Sel.Name)) {
					counted[text]++
					inventoried[selector] = checkpointRootCalls[text] != 0
				}
			case *ast.SelectorExpr:
				text, receiver := types.ExprString(node), types.ExprString(node.X)
				selected[node.Sel] = true
				if slices.Contains([]string{"os.Remove", "os.RemoveAll", "runtimeconfig.WindowsPrivateRemove"}, text) {
					failures["pathname_removal:"+name+":"+text] = true // (b)
				} else if receiver == "c" && removers[node.Sel.Name] {
					failures["pathname_removal:"+name+":"+node.Sel.Name] = true
				}
				failures["forbidden_call:"+name+":"+text] = failures["forbidden_call:"+name+":"+text] || slices.Contains(checkpointForbiddenOS, text) // (c)
				if (slices.Contains([]string{"syscall", "unix", "windows"}, receiver) || receiver == "runtimeconfig" && strings.HasPrefix(name, "drill_reserve")) &&
					!slices.Contains(checkpointSystemSelectors[name], text) && !reserved[node] {
					failures["system_call:"+name+":"+text] = true // (d)
				}
				failures["root_open:"+name] = failures["root_open:"+name] || text == "os.OpenRoot" && !opens[node] // (e)
				if rootFile && slices.Contains(checkpointRootMethods, node.Sel.Name) && !inventoried[node] {
					failures["checkpoint_root_selector:"+text] = true // (f)
				}
				if !rootFile && receiver != "os" && slices.Contains([]string{"Remove", "RemoveAll", "Rename", "Link", "Symlink", "Mkdir", "MkdirAll", "Create",
					"WriteFile", "Chmod", "Chown", "Lchown", "Chtimes", "Truncate", "Write", "WriteAt", "WriteString"}, node.Sel.Name) && !reserved[node] && node != written {
					failures["checkpoint_mutating_method:"+name+":"+text] = true // (h)
				}
			case *ast.Ident:
				if !selected[node] && removers[node.Name] {
					failures["pathname_removal:"+name+":"+node.Name] = true // (b)
				}
				if node.Name == "recovery" || node.Name == "checkpoint" {
					uses[node.Name]++
				}
			}
			return true
		})
		for _, identifier := range []string{"recovery", "checkpoint"} {
			failures["checkpoint_root_uses:"+name+":"+identifier] = !rootFile && uses[identifier] != map[string]int{"checkpoint_tree_unix.go:recovery": 36,
				"checkpoint_tree_windows.go:recovery": 5, "checkpoint_tree_windows.go:checkpoint": 5}[name+":"+identifier] // (g)
		}
	}
	for _, text := range slices.Concat(slices.Collect(maps.Keys(counted)), slices.Collect(maps.Keys(checkpointRootCalls))) {
		failures["checkpoint_root_call:"+text] = counted[text] != checkpointRootCalls[text] // (f), the root files together
	}
	for _, root := range checkpointRootFiles {
		checkpointRootClosed(parsed, parsed[root], failures)
	}
}

// checkpointWaitingReader is the waiting reader node names, else "": as spelled,
// or as declared, an import's member under any local or dot name by the import
// path's last element. A selector marks its name in parts before the name is
// visited, so a member name never resolves alone.
func checkpointWaitingReader(file *ast.File, parts map[*ast.Ident]bool, node ast.Node) string {
	var names []string
	qualifier, member := "", ""
	switch node := node.(type) {
	case *ast.SelectorExpr:
		parts[node.Sel], names, member = true, []string{types.ExprString(node)}, node.Sel.Name
		if base, ok := node.X.(*ast.Ident); ok && base.Obj == nil {
			qualifier = base.Name
		}
	case *ast.Ident:
		if names = []string{node.Name}; !parts[node] {
			qualifier, member = ".", node.Name
		}
	}
	for _, spec := range file.Imports {
		declared, local := path.Base(checkpointLiteral(spec.Path)), path.Base(checkpointLiteral(spec.Path))
		if spec.Name != nil {
			local = spec.Name.Name
		}
		if qualifier != "" && local == qualifier {
			names = append(names, declared+"."+member)
		}
	}
	for _, name := range names {
		if slices.Contains(checkpointWaitingReaders, name) {
			return name
		}
	}
	return ""
}

// checkpointDefaultOpen is the inventory's default open as leaseWaitLiteral
// prints it: openPrivateReader given the placeholder context (LC-LEASE-2A).
const checkpointDefaultOpen = "func(path string) (*os.File, error) { return openPrivateReader(ctx, path) }"

// checkpointWaitingContract is rule (j): checkpointWaitingFiles name no waiting
// reader, called or not, checkpoint.go gives the inventory's default open, by
// assignment or as a checkpointOperations key, only as checkpointDefaultOpen, and
// checkpoint_unix.go uses its PGDATA root only as checkpointSourceRootCalls.
func checkpointWaitingContract(parsed map[string]*ast.File, failures map[string]bool) {
	defaults := 0
	for _, name := range checkpointWaitingFiles {
		if parsed[name] == nil {
			continue
		}
		parts := map[*ast.Ident]bool{}
		ast.Inspect(parsed[name], func(node ast.Node) bool {
			var opened []string
			if reader := checkpointWaitingReader(parsed[name], parts, node); reader != "" {
				failures["waiting_reader:"+name+":"+reader] = true
			}
			switch node := node.(type) {
			case *ast.AssignStmt:
				for index, target := range node.Lhs {
					if types.ExprString(target) == "ops.open" && len(node.Rhs) == len(node.Lhs) {
						opened = append(opened, leaseWaitLiteral(node.Rhs[index]))
					} else if types.ExprString(target) == "ops.open" {
						opened = append(opened, "")
					}
				}
			case *ast.CompositeLit:
				for _, element := range node.Elts {
					if field, ok := element.(*ast.KeyValueExpr); ok && types.ExprString(node.Type) == "checkpointOperations" && types.ExprString(field.Key) == "open" {
						opened = append(opened, leaseWaitLiteral(field.Value))
					}
				}
			}
			for _, opener := range opened {
				if name == "checkpoint.go" {
					defaults++
				}
				failures["waiting_reader:"+name+":ops.open"] = failures["waiting_reader:"+name+":ops.open"] || opener != checkpointDefaultOpen
			}
			return true
		})
	}
	failures["waiting_reader:checkpoint.go:ops.open"] = failures["waiting_reader:checkpoint.go:ops.open"] || defaults == 0
	// Each os.OpenRoot(c.path("pg") + checkpointRootHere), the pinned PGDATA root
	// of (e), is assigned to a name used only as the receiver of
	// checkpointSourceRootCalls: another method, a method value, an alias or an
	// argument fails.
	if file := parsed["checkpoint_unix.go"]; file != nil {
		const open = `os.OpenRoot(c.path("pg") + checkpointRootHere)`
		roots, handled, calls, unassigned := map[any]bool{}, map[ast.Node]bool{}, map[ast.Node]string{}, 0
		lstats, waited := map[ast.Node]bool{}, []ast.Node(nil)
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				if root, ok := node.Lhs[0].(*ast.Ident); ok && root.Obj != nil && len(node.Rhs) == 1 && types.ExprString(node.Rhs[0]) == open {
					roots[root.Obj], handled[root], unassigned = true, true, unassigned-1
				}
			case *ast.CallExpr:
				if calls[node.Fun] = types.ExprString(node); calls[node.Fun] == open {
					unassigned++
				}
				if len(node.Args) == 4 && types.ExprString(node.Fun) == "waitOutLease" && types.ExprString(node.Args[2]) == "root.Lstat" {
					if _, literal := node.Args[3].(*ast.FuncLit); literal {
						lstats[node.Args[2]], waited = true, append(waited, node.Args[3])
					}
				}
			case *ast.SelectorExpr:
				if root, ok := node.X.(*ast.Ident); ok && root.Obj != nil && roots[root.Obj] {
					inside := slices.ContainsFunc(waited, func(literal ast.Node) bool { return literal.Pos() <= node.Pos() && node.End() <= literal.End() })
					admitted := calls[node] == checkpointSourceRootCalls[0] && inside || calls[node] == checkpointSourceRootCalls[1] || lstats[node] && calls[node] == ""
					if handled[root] = true; !admitted {
						failures["pgdata_root_use:checkpoint_unix.go:"+types.ExprString(node)] = true
					}
				}
			case *ast.Ident:
				if node.Obj != nil && roots[node.Obj] && !handled[node] {
					failures["pgdata_root_use:checkpoint_unix.go:"+node.Name] = true
				}
			}
			return true
		})
		failures["pgdata_root_unassigned:checkpoint_unix.go"] = unassigned != 0
		// G4: the opener's name is made once, as name := filepath.FromSlash(rel).
		names := 0
		ast.Inspect(file, func(node ast.Node) bool {
			if assign, ok := node.(*ast.AssignStmt); ok && slices.ContainsFunc(assign.Lhs, func(target ast.Expr) bool { return types.ExprString(target) == "name" }) {
				names += map[bool]int{true: 1, false: 2}[assign.Tok == token.DEFINE && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 && types.ExprString(assign.Rhs[0]) == "filepath.FromSlash(rel)"]
			} else if spec, ok := node.(*ast.ValueSpec); ok && slices.ContainsFunc(spec.Names, func(name *ast.Ident) bool { return name.Name == "name" }) {
				names += 2
			}
			return true
		})
		failures["pgdata_root_name:checkpoint_unix.go"] = names != 1
	}
}

// checkpointSourceContract returns every failed C7 check over production
// sources, independent of GOOS and build tags.
func checkpointSourceContract(sources map[string]string) []string {
	failures := map[string]bool{}
	declared, parsed := map[string]string{}, map[string]*ast.File{}
	packageFunctions := map[string]bool{}
	for name, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			failures["parse:"+name] = true
			continue
		}
		parsed[name] = file
		// Cell methods and package-level names of the checkpoint files; methods
		// of checkpoint types are reachable only through those types.
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && !strings.HasSuffix(name, "_test.go") {
					packageFunctions[node.Name.Name] = true
				}
				if slices.Contains(checkpointFiles, name) && (node.Recv == nil || updateReceiverName(node) == "Cell") {
					declared[node.Name.Name] = name
				}
			case *ast.TypeSpec:
				if slices.Contains(checkpointFiles, name) {
					declared[node.Name.Name] = name
				}
			case *ast.ValueSpec:
				for _, identifier := range node.Names {
					if slices.Contains(checkpointFiles, name) {
						declared[identifier.Name] = name
					}
				}
			}
			_, isFunction := node.(*ast.FuncDecl)
			return !isFunction
		})
	}
	removals := map[string]int{"checkpoint.go": 0, "lifecycle.go": 0, "drill_root.go": 0}
	listedReferences, listedUninitialized := 0, false
	repairNames := map[string]int{}
	const repair = "checkpoint_journal_repair.go:"
	repairRefs := map[string]int{
		repair + "RepairCheckpointJournal:checkpointRootHere": 1, repair + "RepairCheckpointJournal:checkpointCold": 1,
		repair + "settleCheckpointJournal:checkpointLeftover": 1, repair + "checkpointTailCompatible:checkpointLeftover": 1,
		repair + "settleCheckpointJournal:syncCheckpointRoot": 2, "checkpoint_journal_binding_unix.go:repairOwnedJournal:checkpointRootHere": 1,
	}
	documentReads := map[string]int{}
	wantedReads := map[string]int{"witness_read:checkpoint.go:checkpointComplete": 1,
		"witness_read:checkpoint_verify.go:verifyCheckpointDocuments": 1, "inventory_read:checkpoint_verify.go:verifyCheckpointDocuments": 1}
	documentReferences, verifierNames, bypasses := map[string]int{}, map[string]int{}, map[string]int{}
	wantedReferences := map[string]int{
		"checkpointWitnessName:checkpoint.go:witnessCheckpoint": 1, "checkpointWitnessName:checkpoint_root_windows.go:discardOwnCheckpoint": 1, "checkpointWitnessName:checkpoint_tree_unix.go:discard": 1,
		"checkpointWitnessName:checkpoint_root_windows.go:unsealCheckpointHeld": 1, "checkpointWitnessName:checkpoint_tree_unix.go:unseal": 1, "checkpointWitnessName:checkpoint.go:checkpointComplete": 1, "checkpointWitnessName:checkpoint_tree_unix.go:witness": 2,
		"checkpointInventoryName:checkpoint.go:inventoryCheckpoint":            1,
		"checkpointWitnessName:checkpoint_verify.go:verifyCheckpointDocuments": 1, "checkpointInventoryName:checkpoint_verify.go:verifyCheckpointDocuments": 1,
		"checkpointWitnessName:checkpoint_verify.go:checkpointVerifyPath": 1, "checkpointInventoryName:checkpoint_verify.go:checkpointVerifyPath": 1,
		"checkpointWitnessName:checkpoint_verify.go:checkpointVerifyTree": 1, "checkpointInventoryName:checkpoint_verify.go:checkpointVerifyTree": 1,
	}
	witnessSpellings, inventorySpellings, journalCreates, inspectSummaries, drillHooks, drillOpenRoots := 0, 0, 0, 0, 0, 0
	drillRootCalled, drillReserveCalled := map[string]int{}, map[string]int{}
	storageOpenRoots, storageCalls, storageProcs := 0, map[ast.Node]string{}, []string(nil)
	reserveOpenRoots, reserveFuns := 0, map[ast.Node]bool{}
	reserveSteps := []string(nil)
	for name, file := range parsed {
		checkpointFile := slices.Contains(checkpointFiles, name)
		values, read := checkpointAssignments(file), map[ast.Node]bool{}
		receivers, deferred := map[any]bool{}, map[ast.Node]bool{}
		if name == "drill_root.go" {
			receivers, deferred = drillRootReceivers(file, values)
			for _, spec := range file.Imports {
				if path := checkpointLiteral(spec.Path); spec.Name != nil || !slices.Contains(drillRootImports, path) {
					failures["drill_import:"+path] = true
				}
			}
		}
		selectorNames, readOnlyOpens, drillRootTyped := map[*ast.Ident]bool{}, map[ast.Node]bool{}, map[ast.Node]bool{}
		var markRead func([]ast.Expr)
		markRead = func(arguments []ast.Expr) {
			for _, argument := range arguments {
				if join, ok := argument.(*ast.CallExpr); ok && types.ExprString(join.Fun) == "filepath.Join" {
					markRead(join.Args)
				}
				// X + checkpointRootHere, exactly that right operand, opens X as a
				// directory only (the PGDATA root): X is read.
				if here, ok := argument.(*ast.BinaryExpr); ok && here.Op == token.ADD && types.ExprString(here.Y) == "checkpointRootHere" {
					markRead([]ast.Expr{here.X})
				}
				read[argument] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				selectorNames[selector.Sel] = true
			}
			if call, ok := node.(*ast.CallExpr); ok && len(call.Args) == 3 && types.ExprString(call.Args[1]) == "os.O_RDONLY" {
				readOnlyOpens[call], readOnlyOpens[call.Fun] = true, true
			}
			if call, ok := node.(*ast.CallExpr); ok && slices.Contains(checkpointReaders, types.ExprString(call.Fun)) &&
				(types.ExprString(call.Fun) != "readRegular" || len(call.Args) > 1 && types.ExprString(call.Args[1]) == "openPostgresReader") {
				markRead(call.Args)
			}
			return true
		})
		for _, declaration := range file.Decls {
			owner := ""
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				owner = fn.Name.Name
				// G13: an init could wire the dark reservation into a package variable another file calls through.
				failures["drill_reserve_init:"+name] = failures["drill_reserve_init:"+name] || strings.HasPrefix(name, "drill_reserve") && fn.Recv == nil && owner == "init"
			}
			constantNames := map[*ast.Ident]bool{}
			declarationNames := map[ast.Node]string{}
			if group, ok := declaration.(*ast.GenDecl); ok {
				for _, spec := range group.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						declarationNames[spec] = spec.Names[0].Name
						if group.Tok == token.CONST {
							for _, identifier := range spec.Names {
								constantNames[identifier] = true
							}
						}
					case *ast.TypeSpec:
						declarationNames[spec] = spec.Name.Name
					}
				}
			}
			referenceOwner := owner
			var forbiddenReaders []string
			if name == "checkpoint_tree_unix.go" {
				forbiddenReaders = checkpointTreeBypassed
			} else if name == "checkpoint.go" && (owner == "validateRecovery" || owner == "listCheckpoints") {
				forbiddenReaders = checkpointReaderBypassed
			}
			boundaryReaders := map[ast.Expr]bool{}
			ast.Inspect(declaration, func(node ast.Node) bool {
				if declaredName, ok := declarationNames[node]; ok {
					referenceOwner = declaredName
				}
				if value, ok := node.(*ast.ValueSpec); ok && name == "checkpoint_tree_unix.go" && len(value.Names) == 1 && value.Names[0].Name == "checkpointTreeListed" {
					listedUninitialized = len(value.Values) == 0
				}
				if identifier, ok := node.(*ast.Ident); ok {
					if slices.Contains([]string{"RepairCheckpointJournal", "repairOwnedJournal", "settleCheckpointJournal", "beforeJournalSettlement"}, identifier.Name) {
						repairNames[name+":"+owner+":"+identifier.Name]++
					}
					if identifier.Name == "checkpointTreeListed" {
						listedReferences++
					}
					if !constantNames[identifier] && (identifier.Name == "checkpointWitnessName" || identifier.Name == "checkpointInventoryName") {
						documentReferences[identifier.Name+":"+name+":"+referenceOwner]++
					}
					if name == "checkpoint_verify.go" && identifier.Name == "verifyCheckpoint" {
						verifierNames[referenceOwner]++
					}
				}
				// The existing path checks at the cell and recovery boundaries are
				// direct calls only. A reader value passed to a walker is a bypass too.
				if call, ok := node.(*ast.CallExpr); ok && name == "checkpoint.go" && owner == "validateRecovery" && types.ExprString(call) == "os.ReadDir(c.cfg.Root)" {
					boundaryReaders[call.Fun] = true
				}
				if expr, ok := node.(ast.Expr); ok && len(forbiddenReaders) != 0 && !boundaryReaders[expr] {
					if function := types.ExprString(expr); slices.Contains(forbiddenReaders, function) {
						failures["checkpoint_tree_bypass:"+name+":"+function] = true
					}
				}
				if identifier, ok := node.(*ast.Ident); ok && name != "checkpoint_verify.go" && identifier.Name == "verifyCheckpoint" {
					failures["verifier_called:"+name+":"+owner] = true
				}
				if identifier, ok := node.(*ast.Ident); ok && declared[identifier.Name] != "" {
					origin := declared[identifier.Name]
					if checkpointFile && name != "drill_root.go" && origin == "drill_root.go" && !slices.Contains(drillRootShared[name], identifier.Name) {
						failures["drill_reference:"+name+":"+identifier.Name] = true
					}
					if name == "drill_root.go" && origin != "drill_root.go" && !slices.Contains([]string{"recoveryDir", "validateRecovery", "checkpointIDPattern"}, identifier.Name) {
						failures["drill_checkpoint_reference:"+name+":"+identifier.Name] = true
					}
					// The reservation is dark: no production file outside it names its declarations (N10).
					if !strings.HasPrefix(name, "drill_reserve") && strings.HasPrefix(origin, "drill_reserve") {
						failures["drill_reserve_reference:"+name+":"+identifier.Name] = true
					}
				}
				if identifier, ok := node.(*ast.Ident); ok && !checkpointFile && declared[identifier.Name] != "" {
					// (f) exact references from inspection, test hooks and dark journal repair.
					key := name + ":" + owner + ":" + identifier.Name
					switch {
					case repairRefs[key] > 0:
						repairRefs[key]--
					case name == "types.go" && (identifier.Name == "checkpointOperations" || identifier.Name == "CheckpointSummary"):
					case name == "types.go" && identifier.Name == "drillRootOperations":
						drillHooks++
					case name == "lifecycle.go" && identifier.Name == "checkpointSummaries" && owner == "Inspect":
						inspectSummaries++
					default:
						failures["reference:"+name+":"+identifier.Name] = true
					}
				} else if ok && checkpointFile && slices.Contains(checkpointForbiddenHelpers, identifier.Name) {
					failures["forbidden_helper:"+name+":"+identifier.Name] = true
				}
				if checkpointLiteral(node) == checkpointWitnessName {
					witnessSpellings++
				}
				if checkpointLiteral(node) == checkpointInventoryName {
					inventorySpellings++
				}
				if name == "checkpoint_verify.go" {
					member := ""
					if call, ok := node.(*ast.CallExpr); ok {
						function := types.ExprString(call.Fun)
						member = function[strings.LastIndex(function, ".")+1:]
					} else if selector, ok := node.(*ast.SelectorExpr); ok {
						member = selector.Sel.Name
					} else if identifier, ok := node.(*ast.Ident); ok && !selectorNames[identifier] && packageFunctions[identifier.Name] && (identifier.Obj == nil || identifier.Obj.Kind == ast.Fun) {
						member = identifier.Name
					}
					switch {
					case member == "checkpointComplete":
						failures["verifier_shortcut"] = true
					case member == "begin" || member == "step" || member == "closeCheckpointFailure" || member == "closeCheckpointLeftover" || strings.Contains(strings.ToLower(member), "journal"):
						failures["verifier_journal"] = true
					case slices.Contains([]string{"Remove", "RemoveAll", "removePrivateFile", "discardOwnCheckpoint", "discard", "deleteCheckpointLocked", "DeleteCheckpoint"}, member):
						failures["verifier_remove"] = true
					case slices.Contains([]string{"Write", "WriteAt", "WriteString", "WriteFile", "Create", "CreateTemp", "Mkdir", "MkdirAll", "Rename", "Chmod", "Chown", "Chtimes", "Truncate", "Sync",
						"writeAtomic", "writeAtomicBytes", "createPrivateTemp", "publishPrivateFile", "publish", "syncDirectory", "syncCheckpointTreeDirectory", "syncFile", "privateDir", "mkdir", "createTemp", "sync", "record", "checkpointTree", "failCheckpoint", "Checkpoint", "copyCheckpoint", "inventoryCheckpoint", "witnessCheckpoint"}, member):
						failures["verifier_write"] = true
					}
					if member == "openPrivateFile" && !readOnlyOpens[node] {
						failures["verifier_write"] = true
					}
				}
				// (h) by name, called or not: an os or syscall name outside
				// drillRootSelectors, a root's removal, write or open method off a
				// typed root call (os.OpenRoot off its one call), and another
				// file's function other than as a call's function. A call is
				// visited before its function and marks it typed below.
				if name == "drill_root.go" && !drillRootTyped[node] {
					if selector, ok := node.(*ast.SelectorExpr); ok {
						allowed, listed := drillRootSelectors[types.ExprString(selector.X)]
						if listed && !slices.Contains(allowed, selector.Sel.Name) || slices.Contains(drillRootMutators, selector.Sel.Name) {
							failures["drill_selector:"+referenceOwner+":"+types.ExprString(selector)] = true
						}
					} else if identifier, ok := node.(*ast.Ident); ok && identifier.Obj == nil && !selectorNames[identifier] && packageFunctions[identifier.Name] {
						failures["drill_selector:"+referenceOwner+":"+identifier.Name] = true
					}
				}
				// (d) by name, called or not; NewProc only as drillStorageProc's call.
				if strings.HasPrefix(name, "drill_storage") {
					selector, isSelector := node.(*ast.SelectorExpr)
					identifier, isIdentifier := node.(*ast.Ident)
					if call, isCall := node.(*ast.CallExpr); isCall {
						storageCalls[call.Fun] = types.ExprString(call)
					}
					switch {
					case isSelector && slices.Contains(drillStorageWrites, selector.Sel.Name) &&
						(name != "drill_storage.go" || storageCalls[selector] == "" || types.ExprString(selector) != "os.OpenRoot"):
						failures["drill_storage_write:"+name+":"+selector.Sel.Name] = true
					case isIdentifier && slices.Contains(drillStorageHelpers, identifier.Name):
						failures["drill_storage_write:"+name+":"+identifier.Name] = true
					case isSelector && selector.Sel.Name == "NewProc":
						storageProcs = append(storageProcs, storageCalls[selector])
					}
				}
				if call, isCall := node.(*ast.CallExpr); isCall {
					reserveFuns[call.Fun] = true
				}
				if selector, isSelector := node.(*ast.SelectorExpr); isSelector && strings.HasPrefix(name, "drill_reserve") && !reserveFuns[selector] &&
					slices.Contains(drillReserveNamed, selector.Sel.Name) && !slices.Contains([]string{"os", "filepath", "path", "strings", "errors", "json"}, types.ExprString(selector.X)) {
					failures["drill_reserve_unlisted:"+name+":"+types.ExprString(selector)] = true // a method value escapes the call rule
				}
				if assign, isAssign := node.(*ast.AssignStmt); isAssign && name == "drill_reserve.go" && owner == "drillReserveRoot" && types.ExprString(assign.Lhs[len(assign.Lhs)-1]) == "err" {
					reserveSteps = append(reserveSteps, types.ExprString(assign.Rhs[0]))
				} else if returned, isReturn := node.(*ast.ReturnStmt); isReturn && name == "drill_reserve.go" && owner == "drillReserveRoot" { // the parse's offsets
					reserveSteps = append(reserveSteps, strings.ReplaceAll(sources[name], "\r\n", "\n")[returned.Pos()-1:returned.End()-1])
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				function := types.ExprString(call.Fun)
				if _, bare := call.Fun.(*ast.Ident); bare || function == "os.OpenRoot" {
					drillRootTyped[call.Fun] = true
				}
				if _, listed := drillReserveCalls[name][types.ExprString(call)]; listed {
					drillReserveCalled[name+":"+types.ExprString(call)]++
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(name, "drill_reserve") && slices.Contains(drillReserveNamed, selector.Sel.Name) &&
					!slices.Contains([]string{"os", "filepath", "path", "strings", "errors", "json"}, types.ExprString(selector.X)) && drillReserveCalls[name][types.ExprString(call)] == 0 ||
					strings.HasPrefix(name, "drill_reserve") && strings.HasPrefix(function, "os.") && function != "os.SameFile" && function != "os.OpenRoot" && drillReserveCalls[name][types.ExprString(call)] == 0 {
					failures["drill_reserve_unlisted:"+name+":"+types.ExprString(call)] = true
				}
				if name == "drill_storage.go" && function == "os.OpenRoot" { // (b) the one held recovery/, as a directory only
					storageOpenRoots++
					failures["drill_storage_openroot_target"] = failures["drill_storage_openroot_target"] || types.ExprString(call) != "os.OpenRoot(c.recoveryDir() + drillRootHere)"
				}
				if name == "drill_reserve.go" && function == "os.OpenRoot" { // the reservation's one held recovery/, as G4A's
					reserveOpenRoots++
					failures["drill_reserve_openroot_target"] = failures["drill_reserve_openroot_target"] || types.ExprString(call) != "os.OpenRoot(c.recoveryDir() + drillRootHere)"
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && name == "drill_root.go" && drillRootReceiver(receivers, selector.X) {
					// (b) a root call is typed by method, argument text and count.
					drillRootTyped[selector] = true
					var arguments []string
					for _, argument := range call.Args {
						arguments = append(arguments, types.ExprString(argument))
					}
					if allowed, known := drillRootCalls[selector.Sel.Name]; !known {
						failures["drill_root_method:"+selector.Sel.Name] = true
					} else if !slices.Contains(allowed, strings.Join(arguments, ", ")) {
						failures["drill_root_call:"+selector.Sel.Name] = true
					}
					drillRootCalled[selector.Sel.Name]++
					failures["drill_root_close"] = failures["drill_root_close"] || selector.Sel.Name == "Close" && !deferred[call]
				} else if ok && name == "drill_root.go" && (selector.Sel.Name == "RemoveAll" || selector.Sel.Name == "Remove") && types.ExprString(selector.X) != "os" {
					failures["forbidden_call:"+name+":"+function] = true
				}
				if name == "drill_root.go" && function == "os.OpenRoot" {
					// (c) the one pathname root holds recovery/ as a directory only.
					drillOpenRoots++
					if len(call.Args) != 1 || types.ExprString(call.Args[0]) != "c.recoveryDir() + drillRootHere" {
						failures["drill_call_target:os.OpenRoot"] = true
					}
				}
				// (d) a held identity comes from a root handle, a current one from
				// recovery/ or the drill's name without following its last element.
				if name == "drill_root.go" && (function == "os.Stat" || function == "os.Lstat" && (len(call.Args) != 1 || types.ExprString(call.Args[0]) != "c.recoveryDir()") ||
					function == "os.SameFile" && (len(call.Args) != 2 || drillRootIdentity(values, receivers, call.Args[0]) != `root.Stat(".")` ||
						!slices.Contains([]string{"os.Lstat(c.recoveryDir())", "root.Lstat(drillID)"}, drillRootIdentity(values, receivers, call.Args[1])))) {
					failures["drill_identity:"+owner] = true
				}
				if function == "os.RemoveAll" {
					// (a) each checkpoint deletion targets a local checkpointDir value.
					removals[name]++
					root, bare := checkpointRoot(values, call.Args[0], 0)
					if _, local := call.Args[0].(*ast.Ident); name == "checkpoint.go" && (!local || root != "checkpoint" || !bare) {
						failures["removeall_target:"+name] = true
					}
				}
				// (g) exact document reader inventories, including repeated reads
				// within an otherwise permitted owner and local path aliases.
				if slices.Contains([]string{"readJSON", "openRegular", "openPrivateFile", "readPrivateFile", "readPostgresFile", "os.ReadFile", "os.Open", "ops.open", "checkpointVerifyRead"}, function) {
					argument := 0
					switch function {
					case "checkpointVerifyRead":
						argument = 3
					case "readJSON", "openRegular":
						argument = 1 // after its context
					}
					for document, kind := range map[string]string{"checkpointWitnessName": "witness_read:", "checkpointInventoryName": "inventory_read:"} {
						if len(call.Args) > argument && checkpointDocumentReference(values, call.Args[argument], document, 0) {
							documentReads[kind+name+":"+owner]++
						}
					}
				}
				if !checkpointFile {
					return true
				}
				cellRootOpen := name == "checkpoint.go" && owner == "recoveryAbsent" && types.ExprString(call) == "os.OpenRoot(c.cfg.Root + checkpointRootHere)"
				if slices.Contains(checkpointForbiddenOS, function) || (function == "os.OpenRoot" && name != "checkpoint_unix.go" && name != "drill_root.go" && name != "checkpoint_root.go" &&
					name != "drill_storage.go" && name != "drill_reserve.go" && !cellRootOpen) {
					failures["forbidden_call:"+name+":"+function] = true
				}
				// (e) a root open is read-only; the PGDATA source's in checkpoint_unix.go
				// is exactly read-only and non-blocking, and the drill record's may be.
				if flags := ""; strings.HasSuffix(function, ".OpenFile") && function != "os.OpenFile" {
					if len(call.Args) == 3 {
						flags = types.ExprString(call.Args[1])
					}
					nonblocking := flags == "os.O_RDONLY | syscall.O_NONBLOCK"
					if name == "checkpoint_unix.go" && !nonblocking || name != "checkpoint_unix.go" && flags != "os.O_RDONLY" && (name != "drill_root.go" || !nonblocking) {
						failures["root_open_not_read_only:"+name] = true
					}
				}
				// (b) each writer's path is rooted in a checkpointDir value, or is
				// exactly recovery/ (create or sync) or journal/ (one create).
				paths := map[string]int{"writeAtomic": 1, "writeAtomicBytes": 1, "createPrivateTemp": 1, "removePrivateFile": 1, "syncDirectory": 1,
					"syncCheckpointTreeDirectory": 1, "os.RemoveAll": 1, "publishPrivateFile": 2, "ops.publish": 2, "privateDir": 1}[function]
				if function == "privateDir" && types.ExprString(call.Args[1]) == "false" {
					paths = 0
				}
				for _, argument := range call.Args[:min(paths, len(call.Args))] {
					switch root, bare := checkpointRoot(values, argument, 0); {
					case root == "checkpoint" && name != "drill_root.go":
					// The tree's sync seam names the slash path below recovery/ of a directory in its checkpoint.
					case function == "syncCheckpointTreeDirectory" && strings.HasPrefix(name, "checkpoint_tree_") && types.ExprString(argument) == "t.name(name)":
					case root == "recovery" && bare && name != "drill_root.go" && (function == "privateDir" || function == "syncDirectory"):
					case root == "journal" && bare && function == "privateDir" && owner == "deleteCheckpointLocked":
						journalCreates++
					default:
						failures["writer_root:"+name+":"+function] = true
					}
				}
				if name == "checkpoint.go" && slices.Contains(checkpointTreeBypassed, function) && len(call.Args) > 0 && (function != "privateDir" || types.ExprString(call.Args[1]) != "false") {
					if checkpointTreeBypassRooted(values, call.Args[0], 0) {
						bypasses[owner+":"+function]++
					}
				}
				// (b) a path below pg/ or run/, or the manifest, reaches only a reader.
				if first := ""; function == "c.path" && len(call.Args) > 0 {
					if first = checkpointLiteral(call.Args[0]); (first == "pg" || first == "run" || first == "manifest.json") && !read[call] {
						failures["live_path_not_read:"+name+":"+first] = true
					}
				}
				return true
			})
		}
	}
	for key := range wantedReferences {
		failures["document_reference:"+key] = documentReferences[key] != wantedReferences[key]
	}
	// Count declarations as well as calls and method values, across all build tags.
	failures["journal_repair_callers"] = !maps.Equal(repairNames, map[string]int{
		repair + "RepairCheckpointJournal:RepairCheckpointJournal": 1, repair + "RepairCheckpointJournal:repairOwnedJournal": 1,
		repair + "settleCheckpointJournal:settleCheckpointJournal": 1, "checkpoint_journal_binding_other.go:repairOwnedJournal:repairOwnedJournal": 1,
		// A system without a native binding keeps the shared settlement referenced; it calls nothing.
		"checkpoint_journal_binding_other.go::settleCheckpointJournal":             1,
		"checkpoint_journal_binding_unix.go:repairOwnedJournal:repairOwnedJournal": 1, "checkpoint_journal_binding_unix.go:repairOwnedJournal:settleCheckpointJournal": 1,
		// The test hook: its field and the one boundary that reads it. Production assigns it nowhere.
		"types.go::beforeJournalSettlement": 1, repair + "journalSettlementBoundary:beforeJournalSettlement": 2,
	})
	// The package reference contract pins the descriptor truncate to settleCheckpointJournal.
	failures["journal_repair_truncate"] = strings.Count(sources["checkpoint_journal_repair.go"], "f.Truncate(progress.durableBytes)") != 1
	for key, remaining := range repairRefs {
		failures["journal_repair_reference:"+key] = remaining != 0
	}
	for key := range documentReferences {
		failures["document_reference:"+key] = documentReferences[key] != wantedReferences[key]
	}
	if !maps.Equal(verifierNames, map[string]int{"verifyCheckpoint": 1}) {
		for owner := range verifierNames {
			failures["verifier_called:checkpoint_verify.go:"+owner] = true
		}
		if len(verifierNames) == 0 {
			failures["verifier_called:checkpoint_verify.go:"] = true
		}
	}
	for _, key := range slices.Concat(slices.Collect(maps.Keys(bypasses)), slices.Collect(maps.Keys(checkpointTreeBypassKept))) {
		failures["checkpoint_tree_bypass:checkpoint.go:"+key[strings.Index(key, ":")+1:]] = failures["checkpoint_tree_bypass:checkpoint.go:"+key[strings.Index(key, ":")+1:]] || bypasses[key] != checkpointTreeBypassKept[key]
	}
	failures["inventory_name_spelling"] = inventorySpellings != 1
	for reader := range wantedReads {
		failures[reader] = documentReads[reader] != wantedReads[reader]
	}
	for reader := range documentReads {
		failures[reader] = documentReads[reader] != wantedReads[reader]
	}
	for name, count := range removals {
		failures["removeall_count:"+name] = count != map[string]int{"checkpoint.go": 0, "lifecycle.go": 3, "drill_root.go": 0}[name]
	}
	for method, count := range drillRootCounts {
		failures["drill_root_count:"+method] = drillRootCalled[method] != count
	}
	for file, calls := range drillReserveCalls {
		for text, count := range calls {
			failures["drill_reserve_call:"+file+":"+text] = drillReserveCalled[file+":"+text] != count
		}
	}
	failures["drill_call_count:os.OpenRoot"], failures["drill_hook_count"] = drillOpenRoots != 1, drillHooks != 1
	failures["drill_storage_openroot_count"], failures["drill_storage_proc"] = storageOpenRoots != 1, !slices.Equal(storageProcs, []string{drillStorageProc})
	failures["drill_reserve_openroot_count"] = reserveOpenRoots != 1
	failures["drill_reserve_root_returns"] = !slices.Equal(reserveSteps, drillReserveRootSteps)
	failures["drill_reserve_file_proof"] = parsed["drill_reserve.go"] == nil ||
		!slices.Equal(drillReserveFileTail(parsed["drill_reserve.go"], sources["drill_reserve.go"], len(drillReserveFileProof)), drillReserveFileProof)
	failures["journal_create_count"], failures["witness_name_spelling"], failures["inspect_summaries"] = journalCreates != 1, witnessSpellings != 1, inspectSummaries != 1
	failures["checkpoint_tree_listed_seam"] = !listedUninitialized || listedReferences != 3
	checkpointHeldContract(parsed, failures)
	checkpointWaitingContract(parsed, failures)
	checkpointTreeContract(parsed, failures)
	var result []string
	for failure, failed := range failures {
		if failed {
			result = append(result, failure)
		}
	}
	slices.Sort(result)
	return result
}

func TestCheckpointSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"\n", "\r\n"} {
		input := maps.Clone(sources)
		for name, source := range input {
			input[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", ending)
		}
		if failures := checkpointSourceContract(input); len(failures) != 0 {
			t.Fatalf("checkpoint source contract (%q endings): %v", ending, failures)
		}
	}
	type mutation struct{ name, file, before, after, want string }
	const tail = "func checkpointDigits("
	const verifyTail = "func (c *Cell) verifyCheckpoint("
	const drillTail = "const drillRootRecordName"
	const rootTail = "func syncRootDirectory("
	const storageTail, platformTail = "func drillStorageAdd(", "func drillPlatformVolume("
	const reserveTail, allocate = "func drillPlatformAllocate(", "unix.Fallocate(int(f.Fd()), 0, int64(offset), int64(length))"
	const reserveSame = "// drillReserveSame reports"
	const createCall = "unix.Openat(dir, last, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)"
	mutations := []mutation{
		{"drill_reserve_mutating", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(r *os.Root) { _ = r.Remove(\"x\") }\n\n" + reserveTail, "checkpoint_mutating_method:drill_reserve_linux.go:r.Remove"},
		{"drill_reserve_second_create", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(dir int, last string) { _, _ = " + createCall + " }\n\n" + reserveTail,
			"drill_reserve_call:drill_reserve_linux.go:unix.Openat(dir, last, unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC, 0o600)"},
		{"drill_reserve_fallocate", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(f *os.File) { _ = unix.Fallocate(int(f.Fd()), 1, 0, 4096) }\n\n" + reserveTail, "system_call:drill_reserve_linux.go:unix.Fallocate"},
		{"drill_reserve_ioctl", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(f *os.File) { _, _, _ = unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0, 0) }\n\n" + reserveTail, "system_call:drill_reserve_linux.go:unix.Syscall"},
		{"drill_reserve_syscall", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe() { _, _, _ = unix.Syscall(0, 0, 0, 0) }\n\n" + reserveTail, "system_call:drill_reserve_linux.go:unix.Syscall"},
		{"drill_reserve_value", "drill_reserve_linux.go", reserveTail, "var drillReserveProbe = unix.Openat\n\n" + reserveTail, "system_call:drill_reserve_linux.go:unix.Openat"},
		{"drill_reserve_mode", "drill_reserve_linux.go", allocate, strings.Replace(allocate, ", 0,", ", 1,", 1), "drill_reserve_call:drill_reserve_linux.go:" + allocate},
		{"drill_reserve_system", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(f *os.File) { _ = unix.Ftruncate(int(f.Fd()), 0) }\n\n" + reserveTail, "system_call:drill_reserve_linux.go:unix.Ftruncate"},
		{"drill_reserve_caller", "lifecycle.go", "func (c *Cell) begin(", "func (c *Cell) drillReserveCaller() { _ = drillPlatformAllocate(nil, 0, 0) }\n\nfunc (c *Cell) begin(", "reference:lifecycle.go:drillPlatformAllocate"},
		{"drill_reserve_openroot", "drill_reserve_linux.go", reserveTail, "func (c *Cell) drillReserveProbe() { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + reserveTail, "forbidden_call:drill_reserve_linux.go:os.OpenRoot"},
		{"drill_reserve_windows_system", "drill_reserve_windows.go", reserveTail, "func drillReserveProbe() { _ = windows.CloseHandle(0) }\n\n" + reserveTail, "system_call:drill_reserve_windows.go:windows.CloseHandle"},
		{"drill_reserve_pathname_creator", "drill_reserve_windows.go", reserveTail, "func drillReserveProbe() { _ = runtimeconfig.WindowsPrivateMkdir(\"x\") }\n\n" + reserveTail,
			"system_call:drill_reserve_windows.go:runtimeconfig.WindowsPrivateMkdir"},
		{"drill_reserve_windows_mode", "drill_reserve_windows.go", "runtimeconfig.WindowsPrivateBeneath(root, name, false, directory)", "runtimeconfig.WindowsPrivateBeneath(root, name, true, directory)",
			"drill_reserve_call:drill_reserve_windows.go:runtimeconfig.WindowsPrivateBeneath(root, name, false, directory)"},
		{"drill_reserve_open_follow", "drill_reserve_linux.go", "unix.O_RDONLY|unix.O_NOFOLLOW|", "unix.O_RDONLY|",
			"drill_reserve_call:drill_reserve_linux.go:unix.Openat(dir, last, unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC | kind, 0)"},
		{"drill_reserve_follow_root", "drill_reserve.go", "// drillReserveSame reports", "func drillReserveProbe(r *os.Root) { _, _ = r.OpenRoot(\"x\") }\n\n// drillReserveSame reports",
			"drill_reserve_unlisted:drill_reserve.go:r.OpenRoot(\"x\")"},
		{"drill_reserve_platform_stat", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(r *os.Root) { _, _ = r.Stat(\"x\") }\n\n" + reserveTail, "drill_reserve_unlisted:drill_reserve_linux.go:r.Stat(\"x\")"},
		{"drill_reserve_platform_stat_value", "drill_reserve_linux.go", reserveTail, "func drillReserveProbe(r *os.Root) { stat := r.Stat; _, _ = stat(\"x\") }\n\n" + reserveTail,
			"drill_reserve_unlisted:drill_reserve_linux.go:r.Stat"},
		{"drill_reserve_stat_value", "drill_reserve.go", "// drillReserveSame reports", "func drillReserveProbe(r *os.Root) { stat := r.Stat; _, _ = stat(\"x\") }\n\n// drillReserveSame reports",
			"drill_reserve_unlisted:drill_reserve.go:r.Stat"},
		{"drill_reserve_windows_lstat", "drill_reserve_windows.go", reserveTail, "func drillReserveProbe(r *os.Root) { _, _ = r.Lstat(\"x\") }\n\n" + reserveTail, "drill_reserve_unlisted:drill_reserve_windows.go:r.Lstat(\"x\")"},
		{"drill_reserve_renamed_walk", "drill_reserve_windows.go", "\t\"github.com/witwave-ai/witself/internal/runtimeconfig\"\n",
			"\t\"github.com/witwave-ai/witself/internal/runtimeconfig\"\n\trc \"github.com/witwave-ai/witself/internal/runtimeconfig\"\n",
			"system_import:drill_reserve_windows.go:github.com/witwave-ai/witself/internal/runtimeconfig"},
		{"drill_reserve_root_raw", "drill_reserve.go", "drillRootHere)\n\tif err != nil {\n\t\treturn nil, drillStorageError{reason: \"boundary_refused\"}",
			"drillRootHere)\n\tif err != nil {\n\t\treturn nil, err", "drill_reserve_root_returns"},
		// The sequence (G4B2): one held recovery/, every name-resolving or mutating call by exact text, and no caller.
		{"drill_reserve_sync_dropped", "drill_reserve.go", "errors.Join(err, f.Sync())", "errors.Join(err, nil)", "drill_reserve_call:drill_reserve.go:f.Sync()"}, // G1
		{"drill_reserve_proof_writer", "drill_reserve.go", "if closed := f.Close(); !same || closed != nil || drillPlatformVerify(reopened, size) != nil {",
			"if verified := drillPlatformVerify(f, size); f.Close() != nil || !same || verified != nil {", "drill_reserve_file_proof"}, // G5
		{"drill_reserve_init", "drill_reserve.go", reserveSame, "func init() { _ = drillReserveChunk }\n\n" + reserveSame, "drill_reserve_init:drill_reserve.go"}, // G13
		{"drill_reserve_second_openroot", "drill_reserve.go", reserveSame, "func (c *Cell) drillReserveProbe() { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + reserveSame,
			"drill_reserve_openroot_count"},
		{"drill_reserve_openroot_plain", "drill_reserve.go", "os.OpenRoot(c.recoveryDir() + drillRootHere)", "os.OpenRoot(c.recoveryDir())", "drill_reserve_openroot_target"},
		{"drill_reserve_second_rename", "drill_reserve.go", reserveSame, "func drillReserveProbe(drill *os.Root) { _ = drill.Rename(drillReservePending, drillRootRecordName) }\n\n" + reserveSame,
			"drill_reserve_call:drill_reserve.go:drill.Rename(drillReservePending, drillRootRecordName)"},
		{"drill_reserve_removeall", "drill_reserve.go", reserveSame, "func drillReserveProbe(r *os.Root) { _ = r.RemoveAll(\"x\") }\n\n" + reserveSame, "checkpoint_mutating_method:drill_reserve.go:r.RemoveAll"},
		{"drill_reserve_held", "drill_reserve.go", reserveSame, "func drillReserveProbe() { _ = drillRootHeld }\n\n" + reserveSame, "drill_reference:drill_reserve.go:drillRootHeld"},
		{"drill_reserve_sequence_caller", "lifecycle.go", "func (c *Cell) begin(", "func (c *Cell) drillReserveCaller() { _, _ = c.reserveDrillLocked(nil, \"\", \"\", 0) }\n\nfunc (c *Cell) begin(",
			"reference:lifecycle.go:reserveDrillLocked"},
		{"drill_reserve_dark", "checkpoint.go", tail, "func (c *Cell) checkpointProbe() { _, _ = c.reserveDrillLocked(nil, \"\", \"\", 0) }\n\n" + tail, "drill_reserve_reference:checkpoint.go:reserveDrillLocked"},
		{"drill_reserve_follow_open", "drill_reserve.go", reserveSame, "func drillReserveProbe(r *os.Root) { _, _ = r.OpenFile(\"x\", os.O_RDONLY, 0) }\n\n" + reserveSame,
			"drill_reserve_unlisted:drill_reserve.go:r.OpenFile(\"x\", os.O_RDONLY, 0)"},
		{"drill_reserve_stat_name", "drill_reserve.go", "\tdefer release()\n", "\tdefer release()\n\tif _, err := os.Stat(c.recoveryDir()); err != nil {\n\t\treturn \"\", err\n\t}\n",
			"drill_reserve_unlisted:drill_reserve.go:os.Stat(c.recoveryDir())"},
		{"drill_reserve_sync_follow", "drill_reserve.go", "sub, err := drillReserveRoot(drill, created[index])", "sub, err := drill.OpenRoot(filepath.FromSlash(created[index]) + drillRootHere)",
			"drill_reserve_unlisted:drill_reserve.go:drill.OpenRoot(filepath.FromSlash(created[index]) + drillRootHere)"},
		{"drill_storage_write_seed", "drill_storage.go", storageTail, "func (c *Cell) drillStorageProbe(r *os.Root) { _ = r.Mkdir(\"x\", 0o700) }\n\n" + storageTail, "drill_storage_write:drill_storage.go:Mkdir"},
		{"drill_storage_second_openroot", "drill_storage.go", storageTail, "func drillStorageProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + storageTail, "drill_storage_openroot_count"},
		{"drill_storage_openroot_plain", "drill_storage.go", "os.OpenRoot(c.recoveryDir() + drillRootHere)", "os.OpenRoot(c.recoveryDir())", "drill_storage_openroot_target"},
		{"drill_storage_root_reference", "drill_storage.go", storageTail, "func drillStorageProbe() { _ = drillRootHeld }\n\n" + storageTail, "drill_reference:drill_storage.go:drillRootHeld"},
		{"drill_storage_caller", "lifecycle.go", "func (c *Cell) begin(", "func (c *Cell) drillStorageCaller() { _, _ = c.admitDrillStorageLocked(nil, \"\", 0) }\n\nfunc (c *Cell) begin(", "reference:lifecycle.go:admitDrillStorageLocked"},
		{"drill_storage_platform_openroot", "drill_storage_linux.go", platformTail, "func drillStorageProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + platformTail, "drill_storage_write:drill_storage_linux.go:OpenRoot"},
		{"drill_storage_helper", "drill_storage_other.go", platformTail, "func (c *Cell) drillStorageProbe() { _ = c.begin(\"x\") }\n\n" + platformTail, "drill_storage_write:drill_storage_other.go:begin"},
		{"drill_storage_proc", "drill_storage_windows.go", "NewProc(\"NtQueryVolumeInformationFile\")", "NewProc(\"NtSetVolumeInformationFile\")", "drill_storage_proc"},
		{"drill_storage_proc_value", "drill_storage_windows.go", platformTail, "func drillStorageProbe() { _ = windows.NewLazySystemDLL(\"kernel32.dll\").NewProc }\n\n" + platformTail, "drill_storage_proc"},
		{"drill_openroot", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _, _ = os.OpenRoot(dir) }\n\n" + drillTail, "drill_call_target:os.OpenRoot"},
		{"drill_remove", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = os.Remove(dir) }\n\n" + drillTail, "forbidden_call:drill_root.go:os.Remove"},
		{"drill_root_removeall", "drill_root.go", drillTail, "func drillRootProbe(root *os.Root) { _ = root.RemoveAll(\"pg\") }\n\n" + drillTail, "drill_root_call:RemoveAll"},
		{"drill_root_remove", "drill_root.go", drillTail, "func drillRootProbe(root *os.Root) { _ = root.Remove(drillRootRecordName) }\n\n" + drillTail, "drill_root_count:Remove"},
		{"drill_recovery_sync", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _ = syncDirectory(c.recoveryDir()) }\n\n" + drillTail, "writer_root:drill_root.go:syncDirectory"},
		{"drill_checkpoint_sync", "drill_root.go", drillTail, "func drillRootProbe() { _ = syncCheckpointTreeDirectory }\n\n" + drillTail, "drill_checkpoint_reference:drill_root.go:syncCheckpointTreeDirectory"},
		{"drill_second_openroot", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir() + drillRootHere) }\n\n" + drillTail, "drill_call_count:os.OpenRoot"},
		{"drill_openroot_plain", "drill_root.go", "os.OpenRoot(c.recoveryDir() + drillRootHere)", "os.OpenRoot(c.recoveryDir())", "drill_call_target:os.OpenRoot"},
		{"drill_child_openroot_plain", "drill_root.go", "recovery.OpenRoot(drillID + drillRootHere)", "recovery.OpenRoot(drillID)", "drill_root_call:OpenRoot"},
		{"drill_identity_from_lstat", "drill_root.go", "drillHeld, err := drill.Stat(\".\")", "drillHeld, err := recovery.Lstat(drillID)", "drill_identity:drillRootBound"},
		{"drill_identity_follows", "drill_root.go", "drillNow, nowErr := recovery.Lstat(drillID)", "drillNow, nowErr := recovery.Stat(drillID)", "drill_identity:drillRootBound"},
		{"drill_recovery_identity_path", "drill_root.go", "recoveryHeld, err := recovery.Stat(\".\")", "recoveryHeld, err := os.Lstat(c.recoveryDir())", "drill_identity:drillRootBound"},
		{"drill_final_removeall", "drill_root.go", "recovery.Remove(drillID)", "recovery.RemoveAll(drillID)", "drill_root_count:RemoveAll"},
		{"drill_remove_other", "drill_root.go", "drill.Remove(drillRootRecordName)", "drill.Remove(\"pg\")", "drill_root_call:Remove"},
		{"drill_root_write", "drill_root.go", "defer func() { _ = drill.Close() }()", "defer func() { _ = drill.Close() }(); _ = drill.WriteFile(drillRootRecordName, nil, 0o600)", "drill_root_method:WriteFile"},
		{"drill_root_rename", "drill_root.go", "defer func() { _ = recovery.Close() }()", "defer func() { _ = recovery.Close() }(); _ = recovery.Rename(drillID, \"aside\")", "drill_root_method:Rename"},
		{"drill_root_mkdir", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { _ = r.Mkdir(\"late\", 0o700) }\n\n" + drillTail, "drill_root_method:Mkdir"},
		{"drill_record_open_write", "drill_root.go", "drill.OpenFile(drillRootRecordName, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "drill.OpenFile(drillRootRecordName, os.O_RDWR|syscall.O_NONBLOCK, 0)", "root_open_not_read_only:drill_root.go"},
		{"drill_unclosed_root", "drill_root.go", "defer func() { _ = drill.Close() }()", "", "drill_root_count:Close"},
		{"drill_close_not_deferred", "drill_root.go", "defer func() { _ = drill.Close() }()", "_ = drill.Close()", "drill_root_close"},
		{"drill_alias_remove", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { alias := r; _ = alias.RemoveAll(\"pg\") }\n\n" + drillTail, "forbidden_call:drill_root.go:alias.RemoveAll"},
		{"drill_value_remove", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) error { remove := os.Remove; return remove(c.recoveryDir()) }\n\n" + drillTail, "drill_selector:drillRootProbe:os.Remove"},
		{"drill_value_removeall", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) error { removeAll := os.RemoveAll; return removeAll(c.recoveryDir()) }\n\n" + drillTail, "drill_selector:drillRootProbe:os.RemoveAll"},
		{"drill_variable_remove", "drill_root.go", drillTail, "var drillRootProbeRemove = os.Remove\n\nfunc drillRootProbe(c *Cell) error { return drillRootProbeRemove(c.recoveryDir()) }\n\n" + drillTail, "drill_selector:drillRootProbeRemove:os.Remove"},
		{"drill_syscall_remove", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) error { return errors.Join(syscall.Rmdir(c.recoveryDir()), syscall.Unlink(filepath.Join(c.recoveryDir(), \"x\"))) }\n\n" + drillTail, "drill_selector:drillRootProbe:syscall.Rmdir"},
		{"drill_value_openroot", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) error { open := os.OpenRoot; root, _ := open(filepath.Dir(c.recoveryDir())); remove := root.RemoveAll; return remove(\"pg\") }\n\n" + drillTail, "drill_selector:drillRootProbe:os.OpenRoot"},
		{"drill_argument_openroot", "drill_root.go", drillTail, "func drillRootArgumentProbe(c *Cell) { drillRootApply(os.OpenRoot, c.recoveryDir()) }\n\n" + drillTail, "drill_selector:drillRootArgumentProbe:os.OpenRoot"},
		{"drill_value_third_root", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { open := r.OpenRoot; other, _ := open(\"x\"); remove := other.RemoveAll; _ = remove(\"y\") }\n\n" + drillTail, "drill_selector:drillRootProbe:r.OpenRoot"},
		{"drill_value_root_removeall", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { remove := r.RemoveAll; _ = remove(\"pg\") }\n\n" + drillTail, "drill_selector:drillRootProbe:r.RemoveAll"},
		{"drill_alias_write", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { alias := r; _ = alias.WriteFile(\"x\", nil, 0o600); _ = alias.Rename(\"x\", \"y\"); _ = alias.Mkdir(\"z\", 0o700) }\n\n" + drillTail, "drill_selector:drillRootProbe:alias.Rename"},
		{"drill_value_root_write", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { write := r.WriteFile; _ = write(\"x\", nil, 0o600) }\n\n" + drillTail, "drill_selector:drillRootProbe:r.WriteFile"},
		{"drill_syscall_write", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { fd, _ := syscall.Open(c.recoveryDir(), syscall.O_WRONLY|syscall.O_CREAT, 0o600); _ = syscall.Close(fd); _ = syscall.Rename(\"x\", \"y\") }\n\n" + drillTail, "drill_selector:drillRootProbe:syscall.Rename"},
		{"drill_alias_create", "drill_root.go", drillTail, "func drillRootProbe(r *os.Root) { alias := r; _, _ = alias.Create(\"x\") }\n\n" + drillTail, "drill_selector:drillRootProbe:alias.Create"},
		{"drill_final_removeall_value", "drill_root.go", "if recovery.Remove(drillID) == nil {", "if removeAll := recovery.RemoveAll; removeAll(drillID) == nil || recovery.Remove(drillID) == nil {", "drill_selector:deleteDrillLocked:recovery.RemoveAll"},
		{"drill_helper_value", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) error { remove := removePrivateFile; return remove(c.recoveryDir()) }\n\n" + drillTail, "drill_selector:drillRootProbe:removePrivateFile"},
		{"drill_import_renamed", "drill_root.go", "import (\n", "import (\n\tsys \"syscall\"\n", "drill_import:syscall"},
		{"drill_import_unix", "drill_root.go", "import (\n", "import (\n\t\"golang.org/x/sys/unix\"\n", "drill_import:golang.org/x/sys/unix"},
		{"drill_second_hook", "types.go", "type Cell struct {", "var drillRootHookProbe *drillRootOperations\n\n" + "type Cell struct {", "drill_hook_count"},
		{"drill_checkpoint_sync_reference", "drill_root.go", "if drillRootSync(recovery) != nil {", "if syncCheckpointTreeDirectory(c.recoveryDir(), nil) != nil {", "drill_checkpoint_reference:drill_root.go:syncCheckpointTreeDirectory"},
		{"checkpoint_root_open_blocking", "checkpoint_unix.go", "root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "root.OpenFile(name, os.O_RDONLY, 0)", "root_open_not_read_only:checkpoint_unix.go"},
		// (j) no waiting reader on the create path; the PGDATA flag stays in checkpoint_unix.go (d).
		{"waiting_reader_manifest", "checkpoint.go", "openPrivateReader(ctx, c.path(\"manifest.json\"))", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDONLY, 0)", "waiting_reader:checkpoint.go:openPrivateFile"},
		{"waiting_reader_custody", "checkpoint.go", "openPrivateReader(ctx, source)", "openPrivateFile(source, os.O_RDONLY, 0)", "waiting_reader:checkpoint.go:openPrivateFile"},
		{"waiting_reader_inventory_default", "checkpoint.go", "ops.open = " + checkpointDefaultOpen, "ops.open = func(name string) (*os.File, error) { return openPrivateFile(name, os.O_RDONLY, 0) }", "waiting_reader:checkpoint.go:ops.open"},
		{"waiting_reader_inventory_literal", "checkpoint.go", "ops := checkpointOperations{control: c.checkpointControl, free: checkpointFreeSpace}", "ops := checkpointOperations{control: c.checkpointControl, free: checkpointFreeSpace, open: openPlainReader}", "waiting_reader:checkpoint.go:ops.open"},
		{"waiting_reader_pg_version", "checkpoint.go", "readRegular(ctx, openPostgresReader, func(info os.FileInfo) bool { return info.Size() <= 64 }, c.path(\"pg\", \"PG_VERSION\"), errCheckpointSource, errCheckpointSource, errCheckpointSource)", "readPostgresFile(c.path(\"pg\", \"PG_VERSION\"))", "waiting_reader:checkpoint.go:readPostgresFile"},
		{"pg_version_other_opener", "checkpoint.go", "readRegular(ctx, openPostgresReader,", "readRegular(ctx, openPlainReader,", "live_path_not_read:checkpoint.go:pg"},
		{"waiting_reader_value", "checkpoint.go", tail, "func checkpointProbe() { read := os.ReadFile; _ = read }\n\n" + tail, "waiting_reader:checkpoint.go:os.ReadFile"},
		{"waiting_reader_unix", "checkpoint_unix.go", "func checkpointFreeSpace(", "func checkpointProbe(name string) { _, _ = os.Open(name) }\n\nfunc checkpointFreeSpace(", "waiting_reader:checkpoint_unix.go:os.Open"},
		// (j) by declaration (LC-READERS-3R G1): other blocking opens, a renamed or dot import, and any use of the PGDATA root but its two calls.
		{"waiting_reader_open_in_root", "checkpoint.go", tail, "func checkpointProbe(c *Cell) { _, _ = os.OpenInRoot(c.cfg.Root, \"pg\") }\n\n" + tail, "waiting_reader:checkpoint.go:os.OpenInRoot"},
		{"waiting_reader_dirfs", "checkpoint.go", tail, "func checkpointProbe(dir string) { _, _ = os.DirFS(dir).Open(\"x\") }\n\n" + tail, "waiting_reader:checkpoint.go:os.DirFS"},
		{"waiting_reader_renamed_import", "checkpoint.go", "internal/runtimeconfig\"\n)", "internal/runtimeconfig\"\n\tiu \"io/ioutil\"\n)\n\nvar checkpointProbe = iu.ReadFile", "waiting_reader:checkpoint.go:ioutil.ReadFile"},
		{"waiting_reader_dot_import", "checkpoint.go", "internal/runtimeconfig\"\n)", "internal/runtimeconfig\"\n\t. \"io/fs\"\n)\n\nfunc checkpointProbe(fsys FS) { _, _ = ReadFile(fsys, \"x\") }", "waiting_reader:checkpoint.go:fs.ReadFile"},
		{"checkpoint_root_open_method", "checkpoint_unix.go", "return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "if f, err := root.Open(name); err == nil { _ = f.Close() }; return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "pgdata_root_use:checkpoint_unix.go:root.Open"},
		{"checkpoint_root_open_unwaited", "checkpoint_unix.go", "return waitOutLease(ctx, name, root.Lstat, func() (*os.File, error) { return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) })", "return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "pgdata_root_use:checkpoint_unix.go:root.OpenFile"},
		{"checkpoint_root_lstat_called", "checkpoint_unix.go", "name := filepath.FromSlash(rel)", "name := filepath.FromSlash(rel); _, _ = root.Lstat(name)", "pgdata_root_use:checkpoint_unix.go:root.Lstat"},
		{"checkpoint_root_wait_other_lstat", "checkpoint_unix.go", "waitOutLease(ctx, name, root.Lstat,", "waitOutLease(ctx, name, os.Lstat,", "pgdata_root_use:checkpoint_unix.go:root.OpenFile"},
		{"checkpoint_root_read_value", "checkpoint_unix.go", "func() { _ = root.Close() }", "func() { read := root.ReadFile; _ = read; _ = root.Close() }", "pgdata_root_use:checkpoint_unix.go:root.ReadFile"},
		{"checkpoint_root_alias", "checkpoint_unix.go", "func() { _ = root.Close() }", "func() { alias := root; _, _ = alias.Open(\"PG_VERSION\"); _ = root.Close() }", "pgdata_root_use:checkpoint_unix.go:root"},
		{"checkpoint_root_unassigned", "checkpoint_unix.go", "func checkpointFreeSpace(", "func checkpointProbe(c *Cell) (*os.Root, error) { return os.OpenRoot(c.path(\"pg\") + checkpointRootHere) }\n\nfunc checkpointFreeSpace(", "pgdata_root_unassigned:checkpoint_unix.go"},
		// (e) the PGDATA root opens as a directory only, by exactly the named suffix (LC-READERS-5); (b) reads X only before it.
		{"pgdata_root_open_plain", "checkpoint_unix.go", "os.OpenRoot(c.path(\"pg\") + checkpointRootHere)", "os.OpenRoot(c.path(\"pg\"))", "root_open:checkpoint_unix.go"},
		{"pgdata_root_open_join", "checkpoint_unix.go", "os.OpenRoot(c.path(\"pg\") + checkpointRootHere)", "os.OpenRoot(filepath.Join(c.path(\"pg\"), \".\"))", "root_open:checkpoint_unix.go"},
		{"pgdata_root_open_other_suffix", "checkpoint_unix.go", "os.OpenRoot(c.path(\"pg\") + checkpointRootHere)", "os.OpenRoot(c.path(\"pg\") + drillRootHere)", "root_open:checkpoint_unix.go"},
		{"pgdata_root_other_suffix_not_read", "checkpoint_unix.go", "os.OpenRoot(c.path(\"pg\") + checkpointRootHere)", "os.OpenRoot(c.path(\"pg\") + drillRootHere)", "live_path_not_read:checkpoint_unix.go:pg"},
		{"nonblock_selector_elsewhere", "checkpoint_windows.go", "func validateRecoveryObject(", "func checkpointProbe() int { return syscall.O_NONBLOCK }\n\nfunc validateRecoveryObject(", "system_call:checkpoint_windows.go:syscall.O_NONBLOCK"},
		{"checkpoint_openroot", "checkpoint.go", tail, "func checkpointProbe(c *Cell) { _, _ = os.OpenRoot(c.recoveryDir()) }\n\n" + tail, "forbidden_call:checkpoint.go:os.OpenRoot"},
		{"drill_checkpoint_writer", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.checkpointDir(\"\"); _ = syncCheckpointTreeDirectory(dir, nil) }\n\n" + drillTail, "writer_root:drill_root.go:syncCheckpointTreeDirectory"},
		{"drill_third_removeall", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = os.RemoveAll(dir) }\n\n" + drillTail, "removeall_count:drill_root.go"},
		{"checkpoint_drill_writer", "checkpoint.go", tail, "func checkpointProbe(c *Cell) { dir, _ := c.drillDir(\"\"); _ = syncCheckpointTreeDirectory(dir, nil) }\n\n" + tail, "writer_root:checkpoint.go:syncCheckpointTreeDirectory"},
		{"drill_outside_reference", "lifecycle.go", "func (c *Cell) begin(", "func drillRootOutsideProbe() { _ = drillRootRecordName }\n\n" + "func (c *Cell) begin(", "reference:lifecycle.go:drillRootRecordName"},
		{"drill_checkpoint_reference", "checkpoint.go", tail, "func checkpointProbe() { _ = drillRootRecordName }\n\n" + tail, "drill_reference:checkpoint.go:drillRootRecordName"},
		{"drill_calls_checkpoint_delete", "drill_root.go", drillTail, "func drillRootProbe(c *Cell) { _ = c.deleteCheckpointLocked(\"\") }\n\n" + drillTail, "drill_checkpoint_reference:drill_root.go:deleteCheckpointLocked"},
		{"removeall_on_recovery_path", "checkpoint.go", tail, "func checkpointProbe(c *Cell, checkpointID string) error { return os.RemoveAll(c.path(\"recovery\", checkpointID)) }\n\n" + tail, "removeall_target:checkpoint.go"},
		{"third_removeall", "checkpoint.go", tail, "func checkpointSweep(c *Cell) error { dir, _ := c.checkpointDir(\"x\"); return os.RemoveAll(dir) }\n\n" + tail, "removeall_count:checkpoint.go"},
		{"lifecycle_removeall_dropped", "lifecycle.go", "if err := os.RemoveAll(c.path(name)); err != nil {", "if err := removePrivateFile(c.path(name)); err != nil {", "removeall_count:lifecycle.go"},
		{"publish_into_pgdata", "checkpoint.go", "filepath.Join(dir, filepath.FromSlash(entry.path)), false", "filepath.Join(c.path(\"pg\"), filepath.FromSlash(entry.path)), false", "writer_root:checkpoint.go:ops.publish"},
		{"escape_by_literal", "checkpoint.go", tail, "func checkpointProbe(c *Cell, witness checkpointWitness) error { dir, _ := c.checkpointDir(\"\"); return writeAtomic(filepath.Join(dir, \"..\", \"..\", \"manifest.json\"), witness, nil) }\n\n" + tail, "writer_root:checkpoint.go:writeAtomic"},
		{"temporary_in_run", "checkpoint.go", "tree.createTemp(path.Dir(entry.path))", "tree.createTemp(path.Dir(entry.path)); _, _ = createPrivateTemp(c.path(\"run\"))", "live_path_not_read:checkpoint.go:run"},
		{"listing_path_reader", "checkpoint.go", "tree.witness(ctx, name)", "checkpointComplete(ctx, filepath.Join(c.recoveryDir(), name), name)", "checkpoint_tree_bypass:checkpoint.go:checkpointComplete"},
		{"snapshot_path_walker", "checkpoint.go", "recoveryTreeFailure(tree.snapshot())", "recoveryTreeFailure(validatePrivateTreeSnapshot(c.recoveryDir(), filepath.WalkDir, validateRecoveryObject, os.Lstat))", "checkpoint_tree_bypass:checkpoint.go:filepath.WalkDir"},
		{"snapshot_tree_unclosed", "checkpoint.go", "defer tree.close()\n\tif tree.hold() != nil", "if tree.hold() != nil", "checkpoint_reader:checkpoint.go:validateRecovery:tree"},
		{"listing_stage_missing", "checkpoint.go", "c.checkpointStage(\"listed\")", "", "checkpoint_reader:checkpoint.go:listCheckpoints:listed"},
		{"adopt_without_retention", "checkpoint_tree_unix.go", "\t\tif err := t.retain(fd); err != nil {\n\t\t\treturn err\n\t\t}\n", "", "checkpoint_reader:checkpoint_tree_unix.go:adopt:record"},
		{"witness_missing_parent", "checkpoint_tree_unix.go", "return checkpointWitness{}, false, cut\n\t}\n\tif err != nil", "return checkpointWitness{}, false, cut\n\t}\n\tif errors.Is(err, fs.ErrNotExist) { return checkpointWitness{}, false, nil }\n\tif err != nil", "checkpoint_reader:checkpoint_tree_unix.go:witness:cut"},
		// The copy reaches every name below recovery/ through its tree (LC-CKPT-WALK-1B).
		{"copy_pathname_mkdir", "checkpoint.go", "} else if tree.mkdir(entry.path) != nil {", "} else if privateDir(filepath.Join(dir, filepath.FromSlash(entry.path)), true) != nil {", "checkpoint_tree_bypass:checkpoint.go:privateDir"},
		{"tree_pathname_lstat", "checkpoint_tree_unix.go", "\n\treturn privateBeneathLstat(t.recovery, t.name(name), t.want)\n", "\n\treturn os.Lstat(filepath.Join(t.c.recoveryDir(), filepath.FromSlash(t.name(name))))\n", "checkpoint_tree_bypass:checkpoint_tree_unix.go:os.Lstat"},
		{"recovery_joined_sync", "checkpoint.go", tail, "func checkpointProbe(c *Cell, checkpointID string) error { return syncCheckpointTreeDirectory(filepath.Join(c.recoveryDir(), checkpointID), nil) }\n\n" + tail, "writer_root:checkpoint.go:syncCheckpointTreeDirectory"},
		// LC-CKPT-WALK-1B-F1: the bypass rule's missed spellings (P_C5, B23, P_C18), each walk call's identity (B6, B8) and
		// no walk function as a value, each operation's bind (B4), the parent sync in the tree's mkdir (B10), the Windows
		// twin's calls (B24), and the verifier's tree (P_C12).
		{"copy_refusal_stat", "checkpoint.go", "if _, err := tree.lstat(\".\"); !errors.Is(err, fs.ErrNotExist) {", "if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {", "checkpoint_tree_bypass:checkpoint.go:os.Stat"},
		{"copy_refusal_recovery_join", "checkpoint.go", "if _, err := tree.lstat(\".\"); !errors.Is(err, fs.ErrNotExist) {", "if _, err := os.Lstat(filepath.Join(c.recoveryDir(), checkpointID)); !errors.Is(err, fs.ErrNotExist) {", "checkpoint_tree_bypass:checkpoint.go:os.Lstat"},
		{"copy_refusal_recovery_path", "checkpoint.go", "if _, err := tree.lstat(\".\"); !errors.Is(err, fs.ErrNotExist) {", "if _, err := os.Lstat(c.path(\"recovery\", checkpointID)); !errors.Is(err, fs.ErrNotExist) {", "checkpoint_tree_bypass:checkpoint.go:os.Lstat"},
		{"tree_temporary_no_identity", "checkpoint_tree_unix.go", "created, name, err := privateBeneathCreateTemp(t.recovery, t.name(dir), t.want, label)", "created, name, err := privateBeneathCreateTemp(t.recovery, t.name(dir), nil, label)", "checkpoint_tree_want:createTemp:privateBeneathCreateTemp"},
		{"tree_sync_no_identity", "checkpoint_tree_unix.go", "t.name(name), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })", "t.name(name), nil, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })", "checkpoint_tree_want:sync:privateBeneathDirectory"},
		{"tree_walk_value", "checkpoint_tree_unix.go", "created, name, err := privateBeneathCreateTemp(", "temp := privateBeneathCreateTemp\n\tcreated, name, err := temp(", "checkpoint_tree_want_value:createTemp:privateBeneathCreateTemp"},
		{"tree_lstat_unbound", "checkpoint_tree_unix.go", "func (t *checkpointTree) lstat(name string) (fs.FileInfo, error) {\n\tif err := t.bound(); err != nil {\n\t\treturn nil, err\n\t}\n", "func (t *checkpointTree) lstat(name string) (fs.FileInfo, error) {\n", "checkpoint_tree_bind:lstat"},
		{"tree_parent_unsynced", "checkpoint_tree_unix.go", "return syncCheckpointTreeParent(t.name(name), func() error { return privateBeneathSync(fd) })", "return syncCheckpointTreeParent(t.name(name), func() error { return nil })", "checkpoint_tree_parent_sync"},
		{"tree_windows_stat", "checkpoint_tree_windows.go", "return os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))", "return os.Stat(filepath.Join(dir, filepath.FromSlash(name)))", "checkpoint_tree_windows_call:os.Stat"},
		{"tree_windows_second_lstat", "checkpoint_tree_windows.go", "func (t *checkpointTree) close() { closeCheckpointRoots(t.recovery, t.checkpoint) }", "func (t *checkpointTree) close() { closeCheckpointRoots(t.recovery, t.checkpoint); _, _ = os.Lstat(t.id) }", "checkpoint_tree_windows_call:os.Lstat"},
		{"verifier_tree_discard", "checkpoint_verify.go", verifyTail, "func (c *Cell) checkpointProbe(id string) error { return c.checkpointTree(id).discard() }\n\n" + verifyTail, "verifier_remove"},
		{"verifier_tree_sync", "checkpoint_verify.go", verifyTail, "func checkpointProbe(t *checkpointTree) error { return t.sync(\".\") }\n\n" + verifyTail, "verifier_write"},
		// LC-CKPT-WALK-1C1-F1: open, check and record bind by count and in order, record's two syncs, the Windows twin's new
		// calls and its check's decisions, the production read path (O10, O10b), and the verifier's record.
		{"tree_open_unbound", "checkpoint_tree_unix.go", "\tif err := t.bound(); err != nil {\n\t\treturn nil, err\n\t}\n\tlabel := filepath.Join(", "\tlabel := filepath.Join(", "checkpoint_tree_bind:open"},
		{"tree_check_unbound", "checkpoint_tree_unix.go", "\t\tdirectories = directories[1:]\n\t\tif err := t.bound(); err != nil {\n\t\t\treturn 0, err\n\t\t}\n", "\t\tdirectories = directories[1:]\n", "checkpoint_tree_bind:check"},
		{"tree_record_unbound_after_rename", "checkpoint_tree_unix.go", "\tpublished = true\n\tif err := t.bound(); err != nil {\n\t\treturn err\n\t}\n", "\tpublished = true\n", "checkpoint_tree_bind:record"},
		{"tree_open_unwaited", "checkpoint_tree_unix.go", "\treturn waitOutLease(ctx, label, func(string) (fs.FileInfo, error) { return privateBeneathLstat(t.recovery, t.name(name), t.want) },\n\t\tfunc() (*os.File, error) { return privateBeneathOpen(t.recovery, t.name(name), t.want, label) })", "\t_ = ctx\n\treturn privateBeneathOpen(t.recovery, t.name(name), t.want, label)", "checkpoint_tree_order:open"},
		{"tree_check_bound_late", "checkpoint_tree_unix.go", "\t\tif err := t.bound(); err != nil {\n\t\t\treturn 0, err\n\t\t}\n\t\tentries, err := privateBeneathList(t.recovery, t.name(directory), t.want)\n", "\t\tentries, err := privateBeneathList(t.recovery, t.name(directory), t.want)\n\t\tif err := t.bound(); err != nil {\n\t\t\treturn 0, err\n\t\t}\n", "checkpoint_tree_order:check"},
		{"tree_record_bound_after_rename", "checkpoint_tree_unix.go", "\tif err := t.bound(); err != nil {\n\t\treturn err\n\t} else if err := privateBeneathReplace(t.recovery, t.name(path.Dir(name)), path.Base(pending), path.Base(name), t.want); err != nil {\n\t\treturn err\n\t}\n", "\tif err := privateBeneathReplace(t.recovery, t.name(path.Dir(name)), path.Base(pending), path.Base(name), t.want); err != nil {\n\t\treturn err\n\t} else if err := t.bound(); err != nil {\n\t\treturn err\n\t}\n", "checkpoint_tree_order:record"},
		{"tree_record_file_unsynced", "checkpoint_tree_unix.go", "\tif _, err = temporary.Write(data); err == nil {\n\t\terr = syncFile(temporary)\n\t}\n", "\t_, err = temporary.Write(data)\n", "checkpoint_tree_record_sync:file"},
		{"tree_record_sync_elsewhere", "checkpoint_tree_unix.go", "\treturn privateBeneathDirectory(t.recovery, t.name(path.Dir(name)), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })\n}", "\treturn privateBeneathDirectory(t.recovery, t.name(\"pg\"), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })\n}", "checkpoint_tree_record_sync:directory"},
		{"tree_record_sync_noop", "checkpoint_tree_unix.go", "\treturn privateBeneathDirectory(t.recovery, t.name(path.Dir(name)), t.want, func(fd int, _ privateBeneathID) error { return privateBeneathSync(fd) })\n}", "\treturn privateBeneathDirectory(t.recovery, t.name(path.Dir(name)), t.want, func(fd int, _ privateBeneathID) error { return nil })\n}", "checkpoint_tree_record_sync:directory"},
		{"tree_windows_open_plain", "checkpoint_tree_windows.go", "\treturn openPrivateReader(ctx, filepath.Join(dir, filepath.FromSlash(name)))\n", "\treturn openPlainReader(ctx, filepath.Join(dir, filepath.FromSlash(name)))\n", "checkpoint_tree_windows_call:openPrivateReader"},
		{"tree_windows_record_once", "checkpoint_tree_windows.go", "\treturn writeAtomicBytes(filepath.Join(dir, filepath.FromSlash(name)), data, nil)\n", "\treturn writeAtomicBytesOnce(filepath.Join(dir, filepath.FromSlash(name)), data, nil)\n", "checkpoint_tree_windows_call:writeAtomicBytes"},
		{"tree_windows_record_unslashed", "checkpoint_tree_windows.go", "\treturn writeAtomicBytes(filepath.Join(dir, filepath.FromSlash(name)), data, nil)\n", "\treturn writeAtomicBytes(filepath.Join(dir, name), data, nil)\n", "checkpoint_tree_windows_call:filepath.FromSlash"},
		{"tree_windows_check_kind", "checkpoint_tree_windows.go", "if !known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular()) {", "if !known || directory != entry.IsDir() {", "checkpoint_tree_windows_check:if:!known || directory != entry.IsDir() || (!directory && !entry.Type().IsRegular())"},
		{"tree_windows_check_admits", "checkpoint_tree_windows.go", "\t\t\treturn errCheckpointWitness\n", "\t\t\treturn nil\n", "checkpoint_tree_windows_check:return:errCheckpointWitness"},
		{"inventory_pathname_always", "checkpoint.go", "\topened := c.checkpointTest != nil && c.checkpointTest.open != nil\n", "\topened := true\n", "checkpoint_inventory_reader"},
		{"inventory_pathname_in_production", "checkpoint.go", "\topened := c.checkpointTest != nil && c.checkpointTest.open != nil\n", "\topened := c.checkpointTest == nil || c.checkpointTest.open != nil\n", "checkpoint_inventory_reader"},
		{"inventory_arms_swapped", "checkpoint.go", "\t\t\t\tif opened {\n", "\t\t\t\tif !opened {\n", "checkpoint_inventory_reader"},
		{"verifier_tree_record", "checkpoint_verify.go", verifyTail, "func checkpointProbe(t *checkpointTree) error { return t.record(\"probe\", nil) }\n\n" + verifyTail, "verifier_write"},
		{"second_journal_create", "checkpoint.go", "\tif err := c.begin(ctx, \"checkpoint\"); err != nil {", "\t_ = privateDir(c.path(\"journal\"), true)\n\tif err := c.begin(ctx, \"checkpoint\"); err != nil {", "writer_root:checkpoint.go:privateDir"},
		{"manifest_opened_for_write", "checkpoint.go", "openPrivateReader(ctx, c.path(\"manifest.json\"))", "openPrivateFile(c.path(\"manifest.json\"), os.O_RDWR, 0)", "live_path_not_read:checkpoint.go:manifest.json"},
		{"root_open_for_write", "checkpoint_unix.go", "root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "root.OpenFile(name, os.O_RDWR|syscall.O_NONBLOCK, 0)", "root_open_not_read_only:checkpoint_unix.go"},
		{"checkpoint_root_name_made", "checkpoint_unix.go", "name := filepath.FromSlash(rel)", "name := rel", "pgdata_root_name:checkpoint_unix.go"},
		{"checkpoint_root_name_changed", "checkpoint_unix.go", "name := filepath.FromSlash(rel)\n", "name := filepath.FromSlash(rel)\n\t\tname = filepath.Base(name)\n", "pgdata_root_name:checkpoint_unix.go"},
		{"witness_read_elsewhere", "checkpoint.go", tail, "func checkpointPeek(dir string) error { var w checkpointWitness; return readJSON(context.Background(), filepath.Join(dir, checkpointWitnessName), &w) }\n\n" + tail, "witness_read:checkpoint.go:checkpointPeek"},
		{"witness_open_elsewhere", "checkpoint.go", tail, "func checkpointPeek(dir string) { _, _ = openRegular(context.Background(), filepath.Join(dir, checkpointWitnessName), nil, nil, nil) }\n\n" + tail, "witness_read:checkpoint.go:checkpointPeek"},
		{"witness_second_spelling", "checkpoint.go", tail, "var checkpointSeal = \"witness.json\"\n\n" + tail, "witness_name_spelling"},
		{"recovery_dir_elsewhere", "lifecycle.go", "\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "\t_ = c.recoveryDir()\n\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "reference:lifecycle.go:recoveryDir"},
		{"second_inspect_reference", "lifecycle.go", "\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "\t_ = c.checkpointSummaries(ctx, lock, &result)\n\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "inspect_summaries"},
		{"inspect_closes_leftover", "lifecycle.go", "\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "\t_ = c.closeCheckpointLeftover(ctx)\n\tif err := c.checkpointSummaries(ctx, lock, &result); err != nil {", "reference:lifecycle.go:closeCheckpointLeftover"},
		{"summaries_outside_inspect", "lifecycle.go", "func (c *Cell) finishRemoval(", "func (c *Cell) listRecovery() { _ = c.checkpointSummaries(context.Background(), nil, &Inspection{}) }\n\nfunc (c *Cell) finishRemoval(", "reference:lifecycle.go:checkpointSummaries"},
		{"checkpoint_called_elsewhere", "reinstall.go", "func (c *Cell) reinstallBoundary(", "func (c *Cell) reinstallCheckpoint() { _ = c.Checkpoint(nil, CheckpointOptions{}) }\n\nfunc (c *Cell) reinstallBoundary(", "reference:reinstall.go:Checkpoint"},
		{"call_os.OpenRoot_off_unix", "checkpoint_other.go", "func checkpointFreeSpace(", "func checkpointCall() { os.OpenRoot(\"\") }\n\nfunc checkpointFreeSpace(", "forbidden_call:checkpoint_other.go:os.OpenRoot"},
		{"third_witness_reader", "checkpoint_verify.go", verifyTail, "func checkpointVerifyPeek(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointWitnessName)) }\n\n" + verifyTail, "witness_read:checkpoint_verify.go:checkpointVerifyPeek"},
		{"second_inventory_reader", "checkpoint_verify.go", verifyTail, "func checkpointVerifyInventoryPeek(dir string) { p := filepath.Join(dir, checkpointInventoryName); _, _ = os.Open(p) }\n\n" + verifyTail, "inventory_read:checkpoint_verify.go:checkpointVerifyInventoryPeek"},
		{"repeated_witness_owner", "checkpoint.go", tail, "func checkpointComplete(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointWitnessName)) }\n\n" + tail, "witness_read:checkpoint.go:checkpointComplete"},
		{"repeated_inventory_owner", "checkpoint_verify.go", verifyTail, "func verifyCheckpointDocuments(dir string) { _, _ = os.Open(filepath.Join(dir, checkpointInventoryName)) }\n\n" + verifyTail, "inventory_read:checkpoint_verify.go:verifyCheckpointDocuments"},
		{"verifier_writes", "checkpoint_verify.go", verifyTail, "func checkpointVerifyWrite(f *os.File) { _, _ = f.Write(nil) }\n\n" + verifyTail, "verifier_write"},
		{"verifier_syncs", "checkpoint_verify.go", verifyTail, "func checkpointVerifySync(f *os.File) { _ = syncFile(f) }\n\n" + verifyTail, "verifier_write"},
		{"verifier_removes", "checkpoint_verify.go", verifyTail, "func checkpointVerifyRemove() { _ = os.Remove(\"\") }\n\n" + verifyTail, "verifier_remove"},
		{"verifier_journals", "checkpoint_verify.go", verifyTail, "func checkpointVerifyJournal(c *Cell) { _ = c.begin(\"checkpoint\") }\n\n" + verifyTail, "verifier_journal"},
		{"verifier_closes_leftover", "checkpoint_verify.go", verifyTail, "func checkpointVerifyClose(c *Cell) { _ = c.closeCheckpointLeftover(context.Background()) }\n\n" + verifyTail, "verifier_journal"},
		{"verifier_shortcut", "checkpoint_verify.go", verifyTail, "func checkpointVerifyShortcut() { _, _ = checkpointComplete(\"\", \"\") }\n\n" + verifyTail, "verifier_shortcut"},
		{"verifier_called_elsewhere", "checkpoint.go", tail, "func checkpointVerifyCaller(c *Cell) { _, _ = c.verifyCheckpoint(nil, nil, \"\") }\n\n" + tail, "verifier_called:checkpoint.go:checkpointVerifyCaller"},
		{"helper_return", "checkpoint_verify.go", verifyTail, "func reviewPath(dir string) string { return filepath.Join(dir, checkpointWitnessName) }\nfunc reviewRead(dir string) { _, _ = os.Open(reviewPath(dir)) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewPath"},
		{"struct_field", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string) { holder := struct{ path string }{}; holder.path = filepath.Join(dir, checkpointWitnessName); _, _ = os.Open(holder.path) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"method_value", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string, ops checkpointOperations) { reader := ops.open; _, _ = reader(filepath.Join(dir, checkpointWitnessName)) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"unrecognized_reader", "checkpoint_verify.go", verifyTail, "func reviewRead(dir string) { _, _ = os.DirFS(dir).Open(checkpointWitnessName) }\n\n" + verifyTail, "document_reference:checkpointWitnessName:checkpoint_verify.go:reviewRead"},
		{"method_value_write", "checkpoint_verify.go", verifyTail, "func reviewWrite(f *os.File) { writer := f.Write; _, _ = writer([]byte{0x01}) }\n\n" + verifyTail, "verifier_write"},
		{"function_value_write", "checkpoint_verify.go", verifyTail, "func reviewWrite() { publish := writeAtomicBytes; _ = publish }\n\n" + verifyTail, "verifier_write"},
		{"same_file_caller", "checkpoint_verify.go", verifyTail, "func reviewCaller(c *Cell) { _, _ = c.verifyCheckpoint(nil, nil, \"\") }\n\n" + verifyTail, "verifier_called:checkpoint_verify.go:reviewCaller"},
		{"inventory_second_spelling", "checkpoint.go", tail, "var checkpointInventorySeal = \"inventory.json\"\n\n" + tail, "inventory_name_spelling"},
		{"inventory_reference_elsewhere", "checkpoint.go", tail, "func reviewInventory() string { return checkpointInventoryName }\n\n" + tail, "document_reference:checkpointInventoryName:checkpoint.go:reviewInventory"},
		// The held checkpoint roots' rules (b)-(h); drill_root.go keeps its own.
		{"pathname_function_value", "checkpoint.go", tail, "func checkpointProbe() { remove := os.Remove; _ = remove(\"\") }\n\n" + tail, "pathname_removal:checkpoint.go:os.Remove"},
		{"pathname_removeall_value", "checkpoint.go", tail, "func checkpointProbe() { _ = os.RemoveAll }\n\n" + tail, "pathname_removal:checkpoint.go:os.RemoveAll"},
		{"pathname_package_variable", "checkpoint.go", tail, "var checkpointRemove = os.Remove\n\n" + tail, "pathname_removal:checkpoint.go:os.Remove"},
		{"pathname_guarded_value", "checkpoint.go", tail, "func checkpointProbe(c *Cell) error { if runtime.GOOS == \"windows\" { return runtimeconfig.WindowsPrivateRemove(c.recoveryDir()) }; return nil }\n\n" + tail, "pathname_removal:checkpoint.go:runtimeconfig.WindowsPrivateRemove"},
		{"pathname_helper_value", "checkpoint.go", tail, "func checkpointProbe() { remove := removePrivateFile; _ = remove }\n\n" + tail, "pathname_removal:checkpoint.go:removePrivateFile"},
		{"chmod_value", "checkpoint.go", tail, "func checkpointProbe() { _ = os.Chmod }\n\n" + tail, "forbidden_call:checkpoint.go:os.Chmod"},
		{"syscall_unlink", "checkpoint.go", tail, "func checkpointProbe() { _ = syscall.Unlink(\"\") }\n\n" + tail, "system_call:checkpoint.go:syscall.Unlink"},
		{"syscall_rmdir", "checkpoint_unix.go", "func checkpointFreeSpace(", "func checkpointProbe() { _ = syscall.Rmdir(\"\") }\n\nfunc checkpointFreeSpace(", "system_call:checkpoint_unix.go:syscall.Rmdir"},
		{"windows_delete", "checkpoint_windows.go", "func validateRecoveryObject(", "func checkpointProbe() { _ = windows.DeleteFile(nil) }\n\nfunc validateRecoveryObject(", "system_call:checkpoint_windows.go:windows.DeleteFile"},
		{"syscall_alias", "checkpoint_unix.go", "\"syscall\"", "sys \"syscall\"", "system_import:checkpoint_unix.go:syscall"},
		{"openroot_value", "checkpoint.go", tail, "func checkpointProbe() { _ = os.OpenRoot }\n\n" + tail, "root_open:checkpoint.go"},
		{"openroot_value_unix", "checkpoint_unix.go", "func checkpointFreeSpace(", "func checkpointProbe() { open := os.OpenRoot; _ = open }\n\nfunc checkpointFreeSpace(", "root_open:checkpoint_unix.go"},
		{"root_outside", "checkpoint_verify.go", verifyTail, "func checkpointVerifyRoot(checkpoint *os.Root) { _ = checkpoint }\n\n" + verifyTail, "checkpoint_root_uses:checkpoint_verify.go:checkpoint"},
		{"pgdata_root_removal", "checkpoint_unix.go", "}, func() { _ = root.Close() }, nil", "}, func() { _ = root.RemoveAll(\"base\"); _ = root.Close() }, nil", "checkpoint_mutating_method:checkpoint_unix.go:root.RemoveAll"},
		{"root_method_value", "checkpoint_root.go", rootTail, "func checkpointRootProbe(r *os.Root) { remove := r.Remove; _ = remove }\n\n" + rootTail, "checkpoint_root_selector:r.Remove"},
		{"root_alias", "checkpoint_root.go", rootTail, "func checkpointRootProbe(r *os.Root) { alias := r; _ = alias.RemoveAll(\"pg\") }\n\n" + rootTail, "checkpoint_root_call:alias.RemoveAll(\"pg\")"},
		{"root_write", "checkpoint_root.go", rootTail, "func checkpointRootProbe(r *os.Root) { _ = r.WriteFile(\"late\", nil, 0o600) }\n\n" + rootTail, "checkpoint_root_call:r.WriteFile(\"late\", nil, 0o600)"},
		{"final_remove_recursive", "checkpoint_root_windows.go", "if err := recovery.Remove(checkpointID); err != nil {", "if err := recovery.RemoveAll(checkpointID); err != nil {", "checkpoint_root_call:recovery.RemoveAll(checkpointID)"},
		{"recovery_open_plain", "checkpoint_root.go", "os.OpenRoot(c.recoveryDir() + checkpointRootHere)", "os.OpenRoot(c.recoveryDir())", "checkpoint_root_call:os.OpenRoot(c.recoveryDir())"},
		{"checkpoint_open_plain", "checkpoint_root_windows.go", "recovery.OpenRoot(checkpointID + checkpointRootHere)", "recovery.OpenRoot(checkpointID)", "checkpoint_root_call:recovery.OpenRoot(checkpointID)"},
		{"held_identity_from_path", "checkpoint_root.go", "recoveryHeld, err := recovery.Stat(\".\")", "recoveryHeld, err := os.Lstat(c.recoveryDir())", "checkpoint_root_call:os.Lstat(c.recoveryDir())"},
		{"current_identity_follows", "checkpoint_root.go", "checkpointNow, nowErr := recovery.Lstat(checkpointID)", "checkpointNow, nowErr := recovery.Stat(checkpointID)", "checkpoint_root_call:recovery.Stat(checkpointID)"},
		{"third_root", "checkpoint_root.go", rootTail, "func checkpointRootProbe(c *Cell) { r, _ := os.OpenRoot(c.recoveryDir() + checkpointRootHere); _ = r }\n\n" + rootTail, "checkpoint_root_call:os.OpenRoot(c.recoveryDir() + checkpointRootHere)"},
	}
	for _, helper := range checkpointForbiddenHelpers {
		mutations = append(mutations, mutation{"helper_" + helper, "checkpoint.go", tail, "func checkpointCalls(c *Cell) { _ = " + helper + " }\n\n" + tail, "forbidden_helper:checkpoint.go:" + helper})
	}
	for _, call := range checkpointForbiddenOS {
		mutations = append(mutations, mutation{"call_" + call, "checkpoint_windows.go", "func validateRecoveryObject(", "func checkpointCall() { " + call + "() }\n\nfunc validateRecoveryObject(", "forbidden_call:checkpoint_windows.go:" + call})
	}
	for _, method := range drillRootMutators {
		mutations = append(mutations, mutation{"drill_method_value_" + method, "drill_root.go", drillTail, "func drillRootMethodProbe(r *os.Root) { _ = r." + method + " }\n\n" + drillTail, "drill_selector:drillRootMethodProbe:r." + method})
	}
	const pathFunc = "func (c *Cell) path(parts ...string) string {\n"
	mutations = append(mutations, mutation{"journal_hook_assigned", "types.go", pathFunc, pathFunc + "\tc.beforeJournalSettlement = func(string, *os.File) {}\n", "journal_repair_callers"})
	for _, row := range mutations {
		t.Run(row.name, func(t *testing.T) {
			input, source := maps.Clone(sources), strings.ReplaceAll(sources[row.file], "\r\n", "\n")
			if strings.Count(source, row.before) != 1 {
				t.Fatal("checkpoint contract mutation anchor is not unique")
			}
			for _, ending := range []string{"\n", "\r\n"} {
				input[row.file] = strings.ReplaceAll(strings.Replace(source, row.before, row.after, 1), "\n", ending)
				if failures := checkpointSourceContract(input); !slices.Contains(failures, row.want) {
					t.Fatalf("checkpoint contract reported %v, want %s", failures, row.want)
				}
			}
		})
	}
	// A removal declared in files.go and reached from checkpoint.go.
	for _, row := range []struct{ name, declared, call string }{
		{"pathname_variable_elsewhere", "var removeProbe = os.RemoveAll", "func checkpointProbe(c *Cell) { _ = removeProbe(c.recoveryDir()) }"},
		{"pathname_wrapper_elsewhere", "func (c *Cell) removeProbe(name string) error { return os.Remove(name) }", "func checkpointProbe(c *Cell) { _ = c.removeProbe(c.recoveryDir()) }"},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n"} {
				input := maps.Clone(sources)
				for file, edit := range map[string][2]string{"files.go": {"func syncDirectory(", row.declared}, "checkpoint.go": {tail, row.call}} {
					source := strings.ReplaceAll(sources[file], "\r\n", "\n")
					if strings.Count(source, edit[0]) != 1 {
						t.Fatal("checkpoint contract mutation anchor is not unique")
					}
					input[file] = strings.ReplaceAll(strings.Replace(source, edit[0], edit[1]+"\n\n"+edit[0], 1), "\n", ending)
				}
				if failures := checkpointSourceContract(input); !slices.Contains(failures, "pathname_removal:checkpoint.go:removeProbe") {
					t.Fatalf("checkpoint contract reported %v, want pathname_removal:checkpoint.go:removeProbe", failures)
				}
			}
		})
	}
	// The review's fifteen admitted seeds and four more shapes, each planted in
	// checkpoint_root.go, with what a seed needs declared in files.go.
	const seed, removeProbe = "func checkpointRootSeed(", "func (c *Cell) removeProbe(name string) error { return os.Remove(name) }"
	const removeAs, removeBody = "func (c *Cell) ", "(name string) error { return os.Remove(name) }"
	const heldSeed = `recovery, checkpoint, _ := c.holdCheckpoint("ckpt_aaaaaaaaaaaaaaaa"); return c.removeCheckpointHeld(recovery, checkpoint, "ckpt_aaaaaaaaaaaaaaaa")`
	root, files, spool := [2]string{"checkpoint_root.go", rootTail}, [2]string{"files.go", "func syncDirectory("}, "(c *Cell) { root, _ := runtimeconfig.OpenSpoolRoot(c.cfg.Root); closeCheckpointRoots(root, nil) }"
	for _, row := range []struct {
		name, want string
		edits      [][3]string // file, anchor, text put before the anchor
	}{
		{"closed_service_endpoint", "callee:checkpointRootSeed:c.removeServiceControlEndpoint", [][3]string{{root[0], root[1], seed + "c *Cell) error { return c.removeServiceControlEndpoint(nil) }"}}},
		{"closed_owned_endpoint", "callee:checkpointRootSeed:c.removeOwnedServiceControlEndpoint", [][3]string{{root[0], root[1], seed + "c *Cell) error { return c.removeOwnedServiceControlEndpoint(nil, serviceControlEndpointRecord{}) }"}}},
		{"closed_spool_import", "import:github.com/witwave-ai/witself/internal/runtimeconfig", [][3]string{{root[0], "\"errors\"", "\"github.com/witwave-ai/witself/internal/runtimeconfig\""}, {root[0], root[1], seed + spool[1:]}}},
		{"closed_spool_member", "member:checkpointRootSeed:runtimeconfig.OpenSpoolRoot", [][3]string{{root[0], root[1], seed + spool[1:]}}},
		{"closed_alias_wrapper", "callee:checkpointRootSeed:cell.removeProbe", [][3]string{{files[0], files[1], removeProbe}, {root[0], root[1], seed + "c *Cell) error { cell := c; return cell.removeProbe(c.recoveryDir()) }"}}},
		{"closed_alias_endpoint", "callee:checkpointRootSeed:cell.removeServiceControlEndpoint", [][3]string{{root[0], root[1], seed + "c *Cell) error { cell := c; return cell.removeServiceControlEndpoint(nil) }"}}},
		{"closed_expression_wrapper", "callee:checkpointRootSeed:(*Cell).removeProbe", [][3]string{{files[0], files[1], removeProbe}, {root[0], root[1], seed + "c *Cell) error { return (*Cell).removeProbe(c, c.recoveryDir()) }"}}},
		{"closed_expression_endpoint", "callee:checkpointRootSeed:(*Cell).removeServiceControlEndpoint", [][3]string{{root[0], root[1], seed + "c *Cell) error { return (*Cell).removeServiceControlEndpoint(c, nil) }"}}},
		{"closed_two_level_wrapper", "callee:checkpointRootSeed:c.removeProbe", [][3]string{{files[0], files[1], "func (c *Cell) removeProbe(name string) error { return c.removeProbeInner(name) }\n\nfunc (c *Cell) removeProbeInner(name string) error { return os.Remove(name) }"}, {root[0], root[1], seed + "c *Cell) error { return c.removeProbe(c.recoveryDir()) }"}}},
		{"closed_variable_elsewhere", "callee:checkpointRootSeed:removeProbe", [][3]string{{files[0], files[1], "var removeProbe = removePrivateFile"}, {root[0], root[1], seed + "c *Cell) error { return removeProbe(c.recoveryDir()) }"}}},
		{"closed_root_to_helper", "callee:checkpointRootSeed:emptyProbe", [][3]string{{files[0], files[1], "func emptyProbe(root *os.Root) error { return root.RemoveAll(\"pg\") }"}, {root[0], root[1], seed + "recovery *os.Root) error { return emptyProbe(recovery) }"}}},
		{"closed_helper_opens_root", "callee:checkpointRootSeed:c.openProbe", [][3]string{{files[0], files[1], "func (c *Cell) openProbe() error { root, _ := os.OpenRoot(c.cfg.Root); return root.RemoveAll(\"pg\") }"}, {root[0], root[1], seed + "c *Cell) error { return c.openProbe() }"}}},
		{"closed_adapter_method", "callee:checkpointRootSeed:a.removeCreatedUnit", [][3]string{{root[0], root[1], seed + "a *systemdUserAdapter) { a.removeCreatedUnit(ServiceDescriptor{}, nil) }"}}},
		{"closed_journal", "callee:checkpointRootSeed:c.journal", [][3]string{{root[0], root[1], seed + "c *Cell) error { return c.journal(\"checkpoint\", \"copy\", \"done\") }"}}},
		{"closed_begin", "callee:checkpointRootSeed:c.begin", [][3]string{{root[0], root[1], seed + "c *Cell) error { return c.begin(\"checkpoint\") }"}}},
		{"closed_pathname_sync", "callee:checkpointRootSeed:syncCheckpointTreeDirectory", [][3]string{{root[0], root[1], seed + "c *Cell) error { return syncCheckpointTreeDirectory(c.recoveryDir(), nil) }"}}},
		{"closed_method_value", "name:checkpointRootSeed:c.removeServiceControlEndpoint", [][3]string{{root[0], root[1], seed + "c *Cell) { remove := c.removeServiceControlEndpoint; _ = remove }"}}},
		{"closed_function_value", "name:checkpointRootSeed:syncDirectory", [][3]string{{root[0], root[1], seed + ") { sync := syncDirectory; _ = sync }"}}},
		{"closed_import_renamed", "import:errors", [][3]string{{root[0], "\"errors\"", "xerrors \"errors\""}}},
		// The re-review's seven seeds: an allowed callee spelled through an alias, a
		// parameter or a field, or through a c that is not the method's receiver.
		{"closed_alias_sync", "declared:checkpointRootSeed:handle.Sync=c", [][3]string{{files[0], files[1], removeAs + "Sync" + removeBody}, {root[0], root[1], seed + "c *Cell) error { handle := c; return handle.Sync(c.recoveryDir()) }"}}},
		{"closed_alias_readdirnames", "declared:checkpointRootSeed:listing.Readdirnames=c", [][3]string{{files[0], files[1], removeAs + "Readdirnames" + removeBody}, {root[0], root[1], seed + "c *Cell) error { listing := c; return listing.Readdirnames(c.recoveryDir()) }"}}},
		{"closed_alias_mode", "declared:checkpointRootSeed:left.Mode=c", [][3]string{{files[0], files[1], removeAs + "Mode" + removeBody}, {root[0], root[1], seed + "c *Cell) error { left := c; return left.Mode(c.recoveryDir()) }"}}},
		{"closed_parameter_sync", "declared:checkpointRootSeed:handle.Sync=*Cell", [][3]string{{files[0], files[1], removeAs + "Sync" + removeBody}, {root[0], root[1], seed + "c, handle *Cell) error { return handle.Sync(c.recoveryDir()) }"}}},
		{"closed_field_sync", "declared:checkpointRootSeed:handle.Sync=c.seedProbe", [][3]string{{files[0], files[1], "type checkpointSeedProbe struct{}\n\nfunc (checkpointSeedProbe) Sync" + removeBody},
			{"types.go", "\tcheckpointTest           *checkpointOperations", "\tseedProbe                checkpointSeedProbe"}, {root[0], root[1], seed + "c *Cell) error { handle := c.seedProbe; return handle.Sync(c.recoveryDir()) }"}}},
		{"closed_cell_value", "declared:checkpointRootSeed:c.holdCheckpoint=&zero", [][3]string{{root[0], root[1], seed + ") error { var zero Cell; c := &zero; " + heldSeed + " }"}}},
		{"closed_cell_shadowed", "declared:checkpointRootSeed:c.holdCheckpoint=&other", [][3]string{{root[0], root[1], seed + "c *Cell) error { other := *c; other.cfg.Root = string(filepath.Separator) + \"tmp\"; { c := &other; " + heldSeed + " } }"}}},
		// The receiver itself reassigned, or written through.
		{"closed_receiver_assigned", "declared:checkpointRootSeed:c.recoveryDir=*Cell other", [][3]string{{root[0], root[1], "func (c *Cell) checkpointRootSeed(other *Cell) string { c = other; return c.recoveryDir() }"}}},
		{"closed_receiver_written", "assign:checkpointRootSeed:c.cfg.Root", [][3]string{{root[0], root[1], "func (c *Cell) checkpointRootSeed() { c.cfg.Root = \"tmp\" }"}}},
		{"closed_range_written", "assign:checkpointRootSeed:range", [][3]string{{root[0], root[1], "func (c *Cell) checkpointRootSeed() { for _, c.cfg.Root = range []string{\"tmp\"} {} }"}}},
		// A typed root call's receiver that is not a root nor an opened directory.
		{"closed_typed_alias", "receiver:checkpointRootSeed:listing.Close=c", [][3]string{{files[0], files[1], "func (c *Cell) Close() error { _ = os.Remove(c.path(\"seed-absent\")); return nil }"}, {root[0], root[1], "func (c *Cell) checkpointRootSeed() error { listing := c; return listing.Close() }"}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n"} {
				input := maps.Clone(sources)
				for _, edit := range row.edits {
					source := strings.ReplaceAll(input[edit[0]], "\r\n", "\n")
					if strings.Count(source, edit[1]) != 1 {
						t.Fatal("checkpoint contract mutation anchor is not unique")
					}
					input[edit[0]] = strings.ReplaceAll(strings.Replace(source, edit[1], edit[2]+"\n\n"+edit[1], 1), "\n", ending)
				}
				if failures := checkpointSourceContract(input); !slices.Contains(failures, "checkpoint_root_"+row.want) {
					t.Fatalf("checkpoint contract reported %v, want checkpoint_root_%s", failures, row.want)
				}
			}
		})
	}
	t.Run("journal_vocabulary", checkpointJournalVocabulary)
}

// checkpointPredecessorJournalStep is copied literally from the pre-slice validJournalStep.
func checkpointPredecessorJournalStep(operation, step string) bool {
	steps := map[string]map[string]bool{
		"install": {"manifest": true, "password": true, "initdb": true, "configure": true, "postgres": true, "database": true, "stop": true, "principal_credentials": true, "principals": true, "principal_migrations": true, "principal_grants": true, "principal_verified": true},
		"start":   {"ports": true, "postgres": true, "server": true, "server_ready": true, "account": true, "worker": true, "worker_ready": true, "principal_verified": true},
		"stop":    {"stop": true},
		"remove":  {"stop": true, "spool": true, "tmp": true, "logs": true, "purge": true, "runtime_cleanup": true},

		"reinstall": {"postgres": true, "verified": true, "stop": true, "readmitted": true},
	}
	allowed, ok := steps[operation]
	return ok && (step == "operation" || step == "revert_stop" || allowed[step])
}

// checkpointJournalVocabulary (C7(d)): the predecessor refuses every line a
// checkpoint and a deletion write; this reader accepts them.
func checkpointJournalVocabulary(t *testing.T) {
	f, _ := checkpointFixture(t, "legacy", false)
	c := f.cell
	if c.Checkpoint(t.Context(), checkpointOptions(f)) != nil || c.DeleteCheckpoint(t.Context(), DeleteCheckpointOptions{CheckpointID: checkpointIDs(t, c)[0]}) != nil {
		t.Fatal("cannot write checkpoint journal lines")
	}
	lines := bytes.Split(bytes.TrimSuffix(retainedRead(t, c, "journal/operations.jsonl")[len(f.prefix):], []byte("\n")), []byte("\n"))
	for _, line := range lines {
		var entry journalEntry
		if json.Unmarshal(line, &entry) != nil || !validJournalStep(entry.Operation, entry.Step) || checkpointPredecessorJournalStep(entry.Operation, entry.Step) {
			t.Fatal("a checkpoint journal line is accepted by the predecessor or refused by this reader")
		}
	}
	if _, err := c.readJournal(context.Background()); err != nil || len(lines) != len(checkpointRecords)+len(checkpointDeleteRecords) {
		t.Fatal("the current reader refused the checkpoint journal lines")
	}
}

// checkpointAPIWired names every source that reaches the checkpoint API: a
// selector of one of its names in a package directory where any file imports
// the local cell package, or, in any file, a two-argument call of a selector
// named Checkpoint or DeleteCheckpoint.
func checkpointAPIWired(sources map[string]string) []string {
	var wired []string
	files, importing := map[string]*ast.File{}, map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(token.NewFileSet(), name, sources[name], 0)
		if err != nil {
			wired = append(wired, "parse:"+name)
			continue
		}
		files[name] = file
		importing[path.Dir(name)] = importing[path.Dir(name)] || slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
			return checkpointLiteral(spec.Path) == "github.com/witwave-ai/witself/internal/localcell"
		})
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		ast.Inspect(files[name], func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if importing[path.Dir(name)] && slices.Contains([]string{"Checkpoint", "DeleteCheckpoint", "CheckpointOptions", "DeleteCheckpointOptions"}, node.Sel.Name) {
					wired = append(wired, name)
				}
			case *ast.CallExpr:
				if selector, ok := node.Fun.(*ast.SelectorExpr); ok && len(node.Args) == 2 && (selector.Sel.Name == "Checkpoint" || selector.Sel.Name == "DeleteCheckpoint") {
					wired = append(wired, name)
				}
			}
			return true
		})
	}
	return slices.Compact(wired)
}

// checkpointWiringSources reads, by slash path below root, every non-test Go
// file outside internal/localcell that names a checkpoint API or the package;
// dot, node_modules, vendor and testdata directories are not walked.
func checkpointWiringSources(t *testing.T, root string) map[string]string {
	t.Helper()
	sources := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if rel = filepath.ToSlash(rel); err != nil || rel == "." {
			return err
		}
		if entry.IsDir() && (strings.HasPrefix(entry.Name(), ".") || slices.Contains([]string{"node_modules", "vendor", "testdata"}, entry.Name()) || rel == "internal/localcell") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(name)
		if bytes.Contains(data, []byte("Checkpoint")) || bytes.Contains(data, []byte("github.com/witwave-ai/witself/internal/localcell")) {
			sources[rel] = string(data)
		}
		return err
	})
	if err != nil {
		t.Fatal("cannot read the module's sources for the checkpoint wiring contract")
	}
	return sources
}

// checkpointReleasedCaller reports, by label, how sources keyed by slash path
// depart from the released caller contract: the command adapter is the only
// production file that reaches the checkpoint API, and nothing else in the
// module references its two entry points. Sources are read with LF endings.
func checkpointReleasedCaller(sources map[string]string) []string {
	const adapter = "cmd/witself/local_checkpoint_adapter.go"
	failures := map[string]bool{"adapter_missing": true}
	for _, name := range checkpointAPIWired(sources) {
		failures["caller:"+name] = name != adapter
	}
	for name, source := range sources {
		source = strings.ReplaceAll(source, "\r\n", "\n")
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, source, 0)
		if err != nil {
			failures["parse:"+name] = true
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			failures["reference:"+name] = failures["reference:"+name] || name != adapter && ok && (ident.Name == "localCheckpointOpen" || ident.Name == "localCheckpointCallbacksFor")
			return true
		})
		if name == adapter {
			failures["adapter_missing"] = false
			maps.Copy(failures, checkpointAdapterFlow(file, func(node ast.Node) string {
				return source[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset]
			}))
		}
	}
	var result []string
	for label, failed := range failures {
		if failed {
			result = append(result, label)
		}
	}
	slices.Sort(result)
	return result
}

// checkpointAdapterOpen and checkpointAdapterRun are the reviewed adapter
// entry points: localCheckpointOpen returns before constructing a Cell unless
// mode is cold-v1, and localCheckpointRun assigns the mode it passes on only
// from localCheckpointParse's verdict.
const checkpointAdapterOpen = `func localCheckpointOpen(mode localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) {
	if mode != localCheckpointColdV1 || strings.ContainsRune(r.root, 0) || strings.ContainsRune(r.pgbin, 0) {
		return nil, errors.New("local checkpoint construction refused")
	}
	cell, err := localcell.New(localcell.Config{Root: r.root, Binaries: localcell.Binaries{PostgresDir: r.pgbin}, Timeout: r.timeout})
	if err != nil {
		return nil, err
	}
	return cell, nil
}`
const checkpointAdapterRun = `func localCheckpointRun(ctx context.Context, args []string, open func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error), stdout, stderr io.Writer) int {
	var mode localCheckpointMode
	if _, refusal := localCheckpointParse(args); refusal == "" {
		mode = localCheckpointColdV1
	}
	return localCheckpointCommand(ctx, args, localCheckpointCallbacksFor(mode, open), stdout, stderr)
}`

// checkpointAdapterCalls is every reach, by enclosing function and call text,
// of the API methods, localcell.New, open, the callbacks' mode and closures and
// the entry points: one Cell, each method once from ctx and the exact consent,
// the selected mode passed to every open, and no callback that reaches an
// entry point or another callback. A reach outside every function is listed
// under package, so none may occur there.
var checkpointAdapterCalls = []string{
	"localCheckpointCallbacksFor cell.Checkpoint(ctx, localcell.CheckpointOptions{InstallationID: r.installationID})",
	"localCheckpointCallbacksFor cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{CheckpointID: r.checkpointID})",
	"localCheckpointCallbacksFor cell.Inspect(ctx)",
	"localCheckpointCallbacksFor mode", "localCheckpointCallbacksFor mode", "localCheckpointCallbacksFor mode",
	"localCheckpointCallbacksFor open(mode, r)", "localCheckpointCallbacksFor open(mode, r)", "localCheckpointCallbacksFor open(mode, r)",
	"localCheckpointCallbacksFor create", "localCheckpointCallbacksFor create", "localCheckpointCallbacksFor read", "localCheckpointCallbacksFor read", "localCheckpointCallbacksFor remove", "localCheckpointCallbacksFor remove",
	"localCheckpointOpen localcell.New(localcell.Config{Root: r.root, Binaries: localcell.Binaries{PostgresDir: r.pgbin}, Timeout: r.timeout})",
	"localCheckpointRun localCheckpointCallbacksFor(mode, open)",
	"localCheckpointRun localCheckpointCommand(ctx, args, localCheckpointCallbacksFor(mode, open), stdout, stderr)",
}

// checkpointAdapterRelease is the released call inside localCheckpointNamespace.
const checkpointAdapterRelease = "localCheckpointRun(ctx, args[1:], localCheckpointOpen, os.Stdout, os.Stderr)"

// checkpointAdapterMembers closes, over the whole adapter file, the local cell
// package members it may name and the fields, methods and composite keys it
// may select; other packages' members are the command fence's.
var checkpointAdapterMembers = []string{"localcell.Binaries", "localcell.CheckpointOptions", "localcell.ClassifyCheckpointFailure", "localcell.Config", "localcell.DeleteCheckpointOptions", "localcell.Inspection", "localcell.New",
	".Binaries", ".Bytes", ".Checkpoint", ".CheckpointID", ".CheckpointListing", ".CheckpointUnknown", ".Checkpoints", ".Class", ".CleanupIncomplete", ".Complete", ".CreatedAt", ".DeleteCheckpoint", ".ID", ".Inspect",
	".InstallationID", ".Manifest", ".PostgresDir", ".Reason", ".Root", ".Timeout", ".checkpointID", ".entries", ".installationID", ".installationPresent", ".pgbin", ".root", ".status", ".timeout", ".unknown"}

// checkpointAdapterFlow checks every declaration of the adapter, not only
// function bodies, against the reviewed entry points, calls and members. Only
// the release may add, inside localCheckpointNamespace, one localCheckpointOpen
// reference and the planned call. A selector on a local that shadows an import
// fails members.
func checkpointAdapterFlow(file *ast.File, text func(ast.Node) string) map[string]bool {
	failed, calls, got, packages := map[string]bool{"open": true, "run": true}, map[ast.Node]*ast.CallExpr{}, []string{}, map[string]string{}
	for _, spec := range file.Imports {
		packages[path.Base(checkpointLiteral(spec.Path))] = checkpointLiteral(spec.Path)
		failed["members"] = failed["members"] || spec.Name != nil
	}
	ast.Inspect(file, func(node ast.Node) bool {
		member := ""
		switch node := node.(type) {
		case *ast.CallExpr:
			calls[node.Fun] = node
		case *ast.SelectorExpr:
			member = "." + node.Sel.Name
			if x, ok := node.X.(*ast.Ident); ok && x.Obj != nil && packages[x.Name] != "" {
				member = "shadowed " + x.Name
			} else if ok && packages[x.Name] == "github.com/witwave-ai/witself/internal/localcell" {
				member = "localcell" + member
			} else if ok && packages[x.Name] != "" {
				member = ""
			}
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok {
				member = "." + key.Name
			}
		}
		failed["members"] = failed["members"] || member != "" && !slices.Contains(checkpointAdapterMembers, member)
		return true
	})
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		name, body := "package", ast.Node(decl)
		if ok {
			name, body = function.Name.Name, function.Body
			failed["open"] = failed["open"] && (name != "localCheckpointOpen" || text(function) != checkpointAdapterOpen)
			failed["run"] = failed["run"] && (name != "localCheckpointRun" || text(function) != checkpointAdapterRun)
		}
		if ok && function.Body == nil {
			continue
		}
		ast.Inspect(body, func(node ast.Node) bool {
			site := ""
			switch node := node.(type) {
			case *ast.SelectorExpr:
				if x, ok := node.X.(*ast.Ident); slices.Contains([]string{"Inspect", "Checkpoint", "DeleteCheckpoint"}, node.Sel.Name) || ok && x.Name == "localcell" && node.Sel.Name == "New" {
					site = text(node)
				}
			case *ast.Ident:
				if slices.Contains([]string{"localCheckpointOpen", "localCheckpointCallbacksFor", "localCheckpointRun", "localCheckpointCommand", "localCheckpointNamespace"}, node.Name) ||
					node.Name == "open" && (calls[node] != nil || name == "localCheckpointCallbacksFor") || slices.Contains([]string{"mode", "create", "read", "remove"}, node.Name) && name == "localCheckpointCallbacksFor" {
					site = node.Name
				}
			}
			if call := calls[node]; site != "" && call != nil {
				site = text(call)
			}
			if site != "" {
				got = append(got, name+" "+site)
			}
			return true
		})
	}
	if index := slices.Index(got, "localCheckpointNamespace localCheckpointOpen"); index >= 0 {
		got = slices.Delete(got, index, index+1)
	}
	if index := slices.Index(got, "localCheckpointNamespace "+checkpointAdapterRelease); index >= 0 {
		got = slices.Delete(got, index, index+1)
	}
	slices.Sort(got)
	failed["calls"] = !slices.Equal(got, slices.Sorted(slices.Values(checkpointAdapterCalls)))
	return failed
}

// The command adapter is the checkpoint API's only production caller, and only
// its cold-v1 selector reaches the API. Darkness stays for every other caller.
func TestCheckpointReleasedCallerContract(t *testing.T) {
	const adapter = "cmd/witself/local_checkpoint_adapter.go"
	sources := checkpointWiringSources(t, "../..")
	t.Run("allowed-adapter", func(t *testing.T) {
		if _, read := sources["cmd/witself-server/main.go"]; !read {
			t.Fatal("the checkpoint wiring contract did not read a known importer of the package")
		}
		if wired := checkpointAPIWired(sources); !slices.Equal(wired, []string{adapter}) {
			t.Fatalf("the checkpoint API callers are %v, want only the command adapter", wired)
		}
		if failures := checkpointReleasedCaller(sources); len(failures) != 0 {
			t.Fatalf("the released caller contract reported %v", failures)
		}
	})
	const importing = "package main\n\nimport (\n\t\"context\"\n\n\t\"github.com/witwave-ai/witself/internal/localcell\"\n)\n\nvar _ context.Context\n\n"
	const call = importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.Checkpoint(ctx, localcell.CheckpointOptions{}) }\n"
	const fixture = "cmd/witself-fixture/main.go"
	rows := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"checkpoint-call", map[string]string{fixture: call}, []string{fixture}},
		{"delete-call", map[string]string{fixture: importing + "func run(ctx context.Context, cell *localcell.Cell) error { return cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{}) }\n"}, []string{fixture}},
		{"method-value", map[string]string{fixture: importing + "var _ = (*localcell.Cell).Checkpoint\n"}, []string{fixture}},
		{"unrelated-zero-argument-call", map[string]string{"internal/model/model.go": "package model\n\ntype model struct{}\n\nfunc (model) Checkpoint() {}\n\nfunc run() { model{}.Checkpoint() }\n"}, nil},
		{"same-package-non-importing", map[string]string{fixture: importing + "var cell *localcell.Cell\n", "cmd/witself-fixture/run.go": "package main\n\nvar run = cell.Checkpoint\n"}, []string{"cmd/witself-fixture/run.go"}},
		{"two-argument-call-without-import", map[string]string{"tools/other/other.go": "package other\n\nfunc run(cell interface{ Checkpoint(any, any) error }) error { return cell.Checkpoint(nil, nil) }\n"}, []string{"tools/other/other.go"}},
		{"root-level-package", map[string]string{"wiring.go": call}, []string{"wiring.go"}},
		{"outside-the-walk", map[string]string{".tools/main.go": call, "node_modules/x/main.go": call, "vendor/x/main.go": call, "cmd/witself-fixture/testdata/main.go": call,
			"internal/localcell/wiring.go": call, "cmd/witself-fixture/main_test.go": call}, nil},
		{"other-command-file", map[string]string{adapter: sources[adapter], "cmd/witself/other.go": "package main\n\nfunc other(cell localCheckpointCell) error { return cell.Checkpoint(nil, localcell.CheckpointOptions{}) }\n"}, []string{adapter, "cmd/witself/other.go"}},
		{"adapter-name-elsewhere", map[string]string{adapter: sources[adapter], "cmd/other/local_checkpoint_adapter.go": call}, []string{"cmd/other/local_checkpoint_adapter.go", adapter}},
		{"main-method-value", map[string]string{adapter: sources[adapter], "cmd/witself/main.go": importing + "var _ = (*localcell.Cell).Checkpoint\n"}, []string{adapter, "cmd/witself/main.go"}},
	}
	t.Run("other-caller", func(t *testing.T) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				root := t.TempDir()
				for name, source := range row.files {
					if os.MkdirAll(filepath.Join(root, path.Dir(name)), 0o700) != nil || os.WriteFile(filepath.Join(root, name), []byte(source), 0o600) != nil {
						t.Fatal("cannot write a checkpoint wiring fixture")
					}
				}
				fixtures := checkpointWiringSources(t, root)
				if got := checkpointAPIWired(fixtures); !slices.Equal(got, row.want) {
					t.Fatalf("checkpoint wiring fixture %s reported %v", row.name, got)
				}
				released := checkpointReleasedCaller(fixtures)
				for _, name := range row.want {
					if name != adapter && !slices.Contains(released, "caller:"+name) {
						t.Fatalf("checkpoint wiring fixture %s did not report caller %s", row.name, name)
					}
				}
			})
		}
	})
	t.Run("bypass-selector", func(t *testing.T) {
		main, err := os.ReadFile("../../cmd/witself/main.go")
		if err != nil {
			t.Fatal("cannot read the command's main source")
		}
		input := maps.Clone(sources)
		input["cmd/witself/main.go"] = string(main)
		if failures := checkpointReleasedCaller(input); len(failures) != 0 {
			t.Fatalf("the released caller contract reported %v", failures)
		}
		const guard = "\tif mode != localCheckpointColdV1 || strings.ContainsRune(r.root, 0) || strings.ContainsRune(r.pgbin, 0) {\n\t\treturn nil, errors.New(\"local checkpoint construction refused\")\n\t}\n"
		const construct = "\tcell, err := localcell.New(localcell.Config{Root: r.root, Binaries: localcell.Binaries{PostgresDir: r.pgbin}, Timeout: r.timeout})\n"
		const callbacksFor = "func localCheckpointCallbacksFor(mode localCheckpointMode, open func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error)) localCheckpointCallbacks {\n"
		const readOpen = callbacksFor + "\tread := func(ctx context.Context, r localCheckpointRequest) (localCheckpointObservation, localCheckpointFailure) {\n\t\tcell, err := open(mode, r)\n"
		const opener = "func localCheckpointNamespace() func(localCheckpointMode, localCheckpointRequest) (localCheckpointCell, error) {\n\treturn localCheckpointOpen\n}\n\n"
		for _, row := range []struct{ name, file, before, after, want string }{
			{"guard-removed", adapter, guard, "", "open"},
			{"guard-after-construction", adapter, guard + construct, construct + guard, "open"},
			{"second-construction", adapter, "\treturn cell, nil\n}", "\t_, _ = localcell.New(localcell.Config{Root: r.root})\n\treturn cell, nil\n}", "calls"},
			{"mode-forced", adapter, "\tvar mode localCheckpointMode\n", "\tmode := localCheckpointColdV1\n", "run"},
			{"mode-forced-at-open", adapter, "open(mode, r)\n\t\tif err == nil {\n\t\t\terr = cell.Checkpoint", "open(localCheckpointColdV1, r)\n\t\tif err == nil {\n\t\t\terr = cell.Checkpoint", "calls"},
			{"consent-replaced", adapter, "CheckpointID: r.checkpointID", "CheckpointID: \"latest\"", "calls"},
			{"open-referenced", adapter, "localCheckpointCallbacksFor(mode, open)", "localCheckpointCallbacksFor(mode, localCheckpointOpen)", "calls"},
			{"run-opens", "cmd/witself/main.go", "func run(args []string) int {\n", "func run(args []string) int {\n\t_, _ = localCheckpointOpen(localCheckpointColdV1, localCheckpointRequest{})\n", "reference:cmd/witself/main.go"},
			{"run-selects", "cmd/witself/main.go", "func run(args []string) int {\n", "func run(args []string) int {\n\t_ = localCheckpointCallbacksFor(localCheckpointColdV1, nil)\n", "reference:cmd/witself/main.go"},
			{"initializer-deletes", adapter, "func localCheckpointFailed(", "var _ = func() error {\n\tcell, _ := localcell.New(localcell.Config{})\n\treturn cell.DeleteCheckpoint(nil, localcell.DeleteCheckpointOptions{})\n}()\n\nfunc localCheckpointFailed(", "calls"},
			{"initializer-forces-mode", adapter, "func localCheckpointFailed(", "var _ = func() error {\n\tcell, _ := localCheckpointOpen(localCheckpointColdV1, localCheckpointRequest{})\n\treturn cell.Checkpoint(nil, localcell.CheckpointOptions{})\n}()\n\nfunc localCheckpointFailed(", "calls"},
			{"helper-called-from-read", adapter, readOpen, "var localCheckpointCreate = localCheckpointCell.Checkpoint\n\n" + readOpen + "\t\t_ = localCheckpointCreate(cell, ctx, localcell.CheckpointOptions{})\n", "calls"},
			{"unguarded-opener", adapter, callbacksFor, "var localCheckpointUnguarded = func(_ localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) { return localcell.New(localcell.Config{Root: r.root}) }\n\n" + callbacksFor + "\topen = localCheckpointUnguarded\n", "calls"},
			{"package-method-value", adapter, "func localCheckpointFailed(", "var localCheckpointCreate = localCheckpointCell.Checkpoint\n\nfunc localCheckpointFailed(", "calls"},
			{"package-constant", adapter, "func localCheckpointFailed(", "const localCheckpointArity = len([1]func(localCheckpointCell, context.Context) (localcell.Inspection, error){localCheckpointCell.Inspect})\n\nfunc localCheckpointFailed(", "calls"},
			{"package-cell-literal", adapter, "func localCheckpointFailed(", "var localCheckpointZero = &localcell.Cell{}\n\nfunc localCheckpointFailed(", "members"},
			{"open-wrapped", adapter, callbacksFor, callbacksFor + "\tinner := open\n\topen = func(_ localCheckpointMode, r localCheckpointRequest) (localCheckpointCell, error) { return inner(localCheckpointColdV1, r) }\n", "calls"},
			{"open-aliased", adapter, "\t\tinspection, err := cell.Inspect(ctx)\n", "\t\tforced := open\n\t\t_, _ = forced(localCheckpointColdV1, r)\n\t\tinspection, err := cell.Inspect(ctx)\n", "calls"},
			{"stop-asserted", adapter, "err = cell.DeleteCheckpoint(", "_ = cell.(interface{ Stop(context.Context) error }).Stop(ctx)\n\t\t\terr = cell.DeleteCheckpoint(", "members"},
			{"repair-asserted", adapter, "err = cell.DeleteCheckpoint(", "_ = cell.(interface{ Repair(context.Context) error }).Repair(ctx)\n\t\t\terr = cell.DeleteCheckpoint(", "members"},
			{"process-helper", adapter, "func localCheckpointFailed(", "func localCheckpointHelper() { localcell.DispatchProcessHelper() }\n\nfunc localCheckpointFailed(", "members"},
			{"config-key", adapter, "func localCheckpointFailed(", "func localCheckpointJobs() localcell.Config { return localcell.Config{Jobs: nil} }\n\nfunc localCheckpointFailed(", "members"},
			{"dot-import", adapter, "\t\"strings\"\n", "\t\"strings\"\n\t. \"github.com/witwave-ai/witself/internal/localcell\"\n", "members"},
			{"namespace-reentry-from-read", adapter, readOpen, opener + readOpen + "\t\t_ = localCheckpointRun(ctx, []string{\"delete\"}, localCheckpointNamespace(), nil, nil)\n", "calls"},
			{"namespace-opener-in-read", adapter, readOpen, opener + readOpen + "\t\t_, _ = localCheckpointNamespace()(localCheckpointColdV1, r)\n", "calls"},
			{"command-from-read", adapter, readOpen, readOpen + "\t\t_ = localCheckpointCommand(ctx, []string{\"delete\"}, localCheckpointCallbacks{}, nil, nil)\n", "calls"},
			{"read-wrapped-with-remove", adapter, "{create, read, remove}\n", "{create, func(ctx context.Context, r localCheckpointRequest) (localCheckpointObservation, localCheckpointFailure) {\n\t\tif o, f := read(ctx, r); len(o.entries) <= 2 {\n\t\t\treturn o, f\n\t\t}\n\t\treturn localCheckpointObservation{}, remove(ctx, r)\n\t}, remove}\n", "calls"},
			{"release-forces-mode", adapter, checkpointAdapterRelease, strings.Replace(checkpointAdapterRelease, "args[1:]", "append(args[1:], \"--checkpoint-mode=cold-v1\")", 1), "calls"},
			{"release-background", adapter, checkpointAdapterRelease, strings.Replace(checkpointAdapterRelease, "ctx,", "context.Background(),", 1), "calls"},
			{"stop-shadowed", adapter, "err = cell.DeleteCheckpoint(", "if strings, ok := cell.(interface{ Stop(context.Context) error }); ok {\n\t\t\t\t_ = strings.Stop(ctx)\n\t\t\t}\n\t\t\terr = cell.DeleteCheckpoint(", "members"},
			{"localcell-shadowed", adapter, "err = cell.DeleteCheckpoint(", "if localcell, ok := cell.(interface{ Config() error }); ok {\n\t\t\t\t_ = localcell.Config()\n\t\t\t}\n\t\t\terr = cell.DeleteCheckpoint(", "members"},
		} {
			t.Run(row.name, func(t *testing.T) {
				source := strings.ReplaceAll(input[row.file], "\r\n", "\n")
				if strings.Count(source, row.before) != 1 {
					t.Fatal("caller contract mutation anchor is not unique")
				}
				for _, ending := range []string{"\n", "\r\n"} {
					mutated := maps.Clone(input)
					mutated[row.file] = strings.ReplaceAll(strings.Replace(source, row.before, row.after, 1), "\n", ending)
					if failures := checkpointReleasedCaller(mutated); !slices.Contains(failures, row.want) {
						t.Fatalf("caller contract mutation %s (%q endings) reported %v, want %s", row.name, ending, failures, row.want)
					}
				}
			})
		}
	})
}

// Inspect production source across build tags, including method values and
// interface declarations. Only settlement may truncate the held journal.
func TestRepairCheckpointJournalDark(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		status, err := new(Cell).RepairCheckpointJournal(t.Context())
		drillAssert(t, ClassifyCheckpointFailure(err).Class == "local_checkpoint_state_unsupported", "unavailable journal repair changed class")
		drillAssert(t, status == "", "repair reported status on failure")
	}
	for _, source := range checkpointWiringSources(t, filepath.Join("..", "..")) {
		drillAssert(t, !strings.Contains(source, "RepairCheckpointJournal"), "production source reached journal repair")
	}
}
