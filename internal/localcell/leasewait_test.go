package localcell

import (
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/constant"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// leaseWaitSites reach the system only through waitOutLease (R3, R4); the dark
// drill record open is excluded (OQ9).
var leaseWaitSites = []string{"files_other.go:openPrivateReader", "files_other.go:openPrivateWriter", "checkpoint_unix.go:openCheckpointSource", "lock_unix.go:openLockFile"}

// leaseWaitExcluded: the dark drill record open (OQ9); leaseWaitPlain: the one
// plain open in a file holding a site, pinned to pass its caller's flags only.
const leaseWaitExcluded, leaseWaitPlain = "drill_root.go:drillRootHeld", "files_other.go:openPrivateFile"

// leaseWaitBody and leaseWaitValues are waitOutLease and leaseWait's value as
// gofmt prints them, comments aside: calls time.Now(), open(), lstat(path),
// time.Now(), time.NewTimer(delay), ctx.Done(), pause.Stop(), ctx.Err()
// (errors.Is, Add, After, Mode, IsRegular aside), EWOULDBLOCK only inside
// errors.Is, no other open or Stat, ctx only to end a pause with the cut.
const leaseWaitBody = `func waitOutLease[T any](ctx context.Context, path string, lstat func(string) (os.FileInfo, error), open func() (T, error)) (T, error) {
	deadline, delay := time.Now().Add(leaseWait.ceiling), leaseWait.first
	for {
		result, err := open()
		if runtime.GOOS != "linux" || !errors.Is(err, syscall.EWOULDBLOCK) {
			return result, err
		}
		if info, statErr := lstat(path); statErr != nil || !info.Mode().IsRegular() || time.Now().Add(delay).After(deadline) {
			return result, err
		}
		pause := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			pause.Stop()
			return result, &leaseWaitCut{cause: ctx.Err()}
		case <-pause.C:
		}
		if delay *= 2; delay > leaseWait.longest {
			delay = leaseWait.longest
		}
	}
}`
const leaseWaitValues = "struct{ ceiling, first, longest time.Duration }{60 * time.Second, time.Millisecond, 100 * time.Millisecond}"

// leaseWaitLock is lock_unix.go's openLockFile as gofmt prints it (G1): its
// never-waiting flags and the one open, through waitOutLease, nothing else.
const leaseWaitLock = `func openLockFile(ctx context.Context, path string, create bool) (*os.File, error) {
	flags := syscall.O_RDWR | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if create {
		flags |= syscall.O_CREAT
	}
	fd, err := waitOutLease(ctx, path, os.Lstat, func() (int, error) { return syscall.Open(path, flags, 0o600) })
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}`

func leaseWaitPrint(fset *token.FileSet, node any) string {
	var printed strings.Builder
	_ = format.Node(&printed, fset, node)
	return printed.String()
}

// leaseWaitLiteral prints an opener, a function literal as its type and its one
// return (types.ExprString prints only a literal's type).
func leaseWaitLiteral(opener ast.Expr) string {
	if literal, ok := opener.(*ast.FuncLit); ok {
		return types.ExprString(literal.Type) + " { " + writerOneReturn(literal.Body) + " }"
	}
	return types.ExprString(opener)
}

// leaseWaitThrough: open is the one return of the function literal passed to a
// waitOutLease call among calls, with function's own context (nothing else),
// open's path and its namespace's Lstat.
func leaseWaitThrough(function *ast.FuncDecl, calls []*ast.CallExpr, open *ast.CallExpr) bool {
	lstat, selector := "os.Lstat", open.Fun.(*ast.SelectorExpr)
	if selector.Sel.Name == "OpenFile" && types.ExprString(selector.X) != "os" {
		lstat = types.ExprString(selector.X) + ".Lstat"
	}
	return len(open.Args) > 0 && slices.ContainsFunc(calls, func(wait *ast.CallExpr) bool {
		if len(wait.Args) != 4 || types.ExprString(wait.Fun) != "waitOutLease" || !leaseWaitOwnContext(function, wait.Args[0]) ||
			types.ExprString(wait.Args[1]) != types.ExprString(open.Args[0]) || types.ExprString(wait.Args[2]) != lstat {
			return false
		}
		literal, ok := wait.Args[3].(*ast.FuncLit)
		return ok && literal.Pos() < open.Pos() && open.End() < literal.End() && writerOneReturn(literal.Body) == "return "+types.ExprString(open)
	})
}

// leaseWaitOwnContext: argument is the identifier the parser resolved to a
// context.Context parameter of function itself.
func leaseWaitOwnContext(function *ast.FuncDecl, argument ast.Expr) bool {
	identifier, ok := argument.(*ast.Ident)
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			if ok && identifier.Obj != nil && identifier.Obj == name.Obj && types.ExprString(field.Type) == "context.Context" {
				return true
			}
		}
	}
	return false
}

// leaseWaitOpeners are the declarations that take the operation's context first
// (LC-LEASE-2A R5); openCheckpointSource is a method.
var leaseWaitOpeners = []string{"waitOutLease", "openPrivateReader", "openPrivateWriter", "openPostgresReader", "openPlainReader", "openLockFile",
	"openCheckpointSource", "openRegularBy", "readRegular"}

// leaseWaitPlaceholders are the non-test calls into leaseWaitOpeners that pass
// context.Background() instead of their declaration's own context, by file and
// declaration with their counts (R6); each later slice shrinks it.
var leaseWaitPlaceholders = map[string]int{"checkpoint.go:(*Cell).admitCheckpoint": 1, "checkpoint.go:(*Cell).checkpointOps": 1,
	"checkpoint.go:(*Cell).copyCheckpoint": 1, "checkpoint.go:(*Cell).openCheckpointEntry": 2, "checkpoint.go:(*Cell).witnessCheckpoint": 1,
	"commands.go:(*Cell).password": 1, "files.go:openRegular": 1, "files.go:openRegularWriter": 1, "installer_principal_credentials.go:openPrincipalCredentialFile": 1,
	"lifecycle.go:(*Cell).postmaster": 1, "lifecycle.go:(*Cell).recoverPostgresRecord": 1, "lock.go:acquireLock": 2, "lock_contention.go:classifyOwnershipFailure": 1,
	"readiness.go:(*Cell).lastValueFreeDiagnostic": 1, "reinstall.go:(*Cell).reinstallAvailable": 1, "service_adapter_admission.go:serviceAdapterRemovalHostLock": 1,
	"service_control_exit.go:(*Cell).serviceHostLeaseHeld": 1, "service_descriptor.go:openServiceRecord": 1, "service_foreground.go:(*Cell).acquireServiceLeaseWithBoot": 1,
	"service_launchd.go:(*launchdAgentAdapter).ownedPlist": 1, "service_systemd_adapter.go:(*systemdUserAdapter).ownedUnit": 1}

// leaseWaitCutText is files.go's cut, then openRegularBy's open with the cut
// returned before any Lstat classifies it (R4), each exactly once.
var leaseWaitCutText = []string{"type leaseWaitCut struct{ cause error }\n\nfunc (e *leaseWaitCut) Error() string { return \"local cell lease wait ended: \" + " +
	"e.cause.Error() }\nfunc (e *leaseWaitCut) Unwrap() error { return e.cause }\n", "open error) (*os.File, error) {\n\tf, err := openReader(ctx, path)\n" +
	"\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n\tif err != nil {\n\t\tinfo, err := os.Lstat(path)\n"}

// leaseWaitContract returns every failed key of R6 over the non-test sources,
// read without build constraints and with CRLF normalised; LC-LEASE-2A adds
// lease_wait_placeholder and lease_wait_cut.
func leaseWaitContract(sources map[string]string) (result []string) {
	failures, fset, counts, found := map[string]bool{}, token.NewFileSet(), map[string]int{}, map[string]bool{}
	placeholders := map[string]int{}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, name, strings.ReplaceAll(sources[name], "\r\n", "\n"), parser.ParseComments)
		if failures["parse:"+name] = err != nil; err != nil {
			continue
		}
		// lease_wait_placeholder: a call into leaseWaitOpeners whose first argument
		// is not its declaration's own context is a site, by file and declaration.
		for _, declaration := range file.Decls {
			function, _ := declaration.(*ast.FuncDecl)
			referencesDeclarations(declaration, func(owner string, node ast.Node) {
				ast.Inspect(node, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					callee, fun := "", ast.Unparen(call.Fun)
					if generic, ok := fun.(*ast.IndexExpr); ok {
						fun = generic.X
					}
					switch fun := fun.(type) {
					case *ast.Ident:
						if fun.Obj == nil || fun.Obj.Kind == ast.Fun {
							callee = fun.Name
						}
					case *ast.SelectorExpr:
						if fun.Sel.Name == "openCheckpointSource" {
							callee = fun.Sel.Name
						}
					}
					if site := name + ":" + owner; slices.Contains(leaseWaitOpeners, callee) && (function == nil || len(call.Args) == 0 || !leaseWaitOwnContext(function, call.Args[0])) {
						placeholders[site]++
						failures["lease_wait_placeholder:"+site] = failures["lease_wait_placeholder:"+site] || len(call.Args) == 0 || types.ExprString(call.Args[0]) != "context.Background()"
					}
					return true
				})
			})
		}
		windows := strings.HasSuffix(name, "_windows.go") // or a build line only Windows meets
		for _, group := range file.Comments {
			for _, comment := range group.List {
				if expression, err := constraint.Parse(comment.Text); err == nil && comment.Pos() < file.Package {
					windows = windows || expression.Eval(func(tag string) bool { return tag == "windows" }) && !expression.Eval(func(tag string) bool { return tag == "linux" || tag == "unix" })
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			var targets []ast.Expr // what node assigns or exposes to assignment
			switch node := node.(type) {
			case *ast.Ident:
				failures["lease_wait_windows"] = failures["lease_wait_windows"] || windows && (node.Name == "waitOutLease" || node.Name == "leaseWait")
				// A local or non-function declaration of the name is a shadow (G1).
				failures["lease_wait_shadow:"+name] = failures["lease_wait_shadow:"+name] || node.Name == "waitOutLease" && node.Obj != nil && node.Obj.Kind != ast.Fun
			case *ast.AssignStmt:
				targets = node.Lhs
			case *ast.IncDecStmt:
				targets = []ast.Expr{node.X}
			case *ast.UnaryExpr:
				targets = []ast.Expr{map[bool]ast.Expr{true: node.X}[node.Op == token.AND]}
			case *ast.RangeStmt:
				targets = []ast.Expr{node.Key, node.Value}
			case *ast.ValueSpec:
				for index, identifier := range node.Names {
					if identifier.Name == "leaseWait" {
						counts["leaseWait"]++
						failures["lease_wait_values"] = failures["lease_wait_values"] || name != "files_other.go" || len(node.Values) != len(node.Names) || node.Type != nil ||
							leaseWaitPrint(fset, node.Values[index]) != leaseWaitValues
					}
				}
			}
			for _, target := range slices.DeleteFunc(targets, func(target ast.Expr) bool { return target == nil }) {
				text := strings.TrimLeft(types.ExprString(target), "(")
				failures["lease_wait_values"] = failures["lease_wait_values"] || text == "leaseWait" || strings.HasPrefix(text, "leaseWait.") || strings.HasPrefix(text, "leaseWait)")
			}
			return true
		})
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if bare := *function; function.Name.Name == "waitOutLease" {
				bare.Doc, counts["waitOutLease"] = nil, counts["waitOutLease"]+1
				failures["lease_wait_body"] = failures["lease_wait_body"] || name != "files_other.go" || leaseWaitPrint(fset, &bare) != leaseWaitBody
			} else if function.Name.Name == "openLockFile" && name == "lock_unix.go" {
				bare.Doc, counts["openLockFile"] = nil, counts["openLockFile"]+1
				failures["lease_wait_lock"] = failures["lease_wait_lock"] || leaseWaitPrint(fset, &bare) != leaseWaitLock
			}
			site, nonblocking, calls := name+":"+function.Name.Name, false, []*ast.CallExpr(nil)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					calls = append(calls, call)
				} else if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "O_NONBLOCK" {
					nonblocking = true
				}
				return true
			})
			// Every open in a file that holds a site is examined, not only where
			// O_NONBLOCK is named (G1); the plain open stays exactly a pass-through.
			holds := slices.ContainsFunc(leaseWaitSites, func(held string) bool { return strings.HasPrefix(held, name+":") })
			if site == leaseWaitPlain {
				failures["lease_wait_caller:"+site] = writerOneReturn(function.Body) != "return os.OpenFile(path, flags, mode)"
				continue
			} else if found[site] = !windows && site != leaseWaitExcluded && (nonblocking || holds || slices.Contains(leaseWaitSites, site)); !found[site] {
				continue
			}
			for _, call := range calls {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && (slices.Contains([]string{"os.Open", "os.OpenFile", "syscall.Open", "unix.Open"}, types.ExprString(selector)) || selector.Sel.Name == "OpenFile") {
					failures["lease_wait_caller:"+site] = failures["lease_wait_caller:"+site] || !leaseWaitThrough(function, calls, call)
					found[site+":open"] = true
				}
			}
		}
	}
	for _, site := range leaseWaitSites {
		failures["lease_wait_caller:"+site] = failures["lease_wait_caller:"+site] || !found[site+":open"]
	}
	failures["lease_wait_body"] = failures["lease_wait_body"] || counts["waitOutLease"] != 1
	failures["lease_wait_values"] = failures["lease_wait_values"] || counts["leaseWait"] != 1
	failures["lease_wait_lock"] = failures["lease_wait_lock"] || counts["openLockFile"] != 1
	for _, site := range slices.Concat(slices.Collect(maps.Keys(placeholders)), slices.Collect(maps.Keys(leaseWaitPlaceholders))) {
		failures["lease_wait_placeholder:"+site] = failures["lease_wait_placeholder:"+site] || placeholders[site] != leaseWaitPlaceholders[site]
	}
	for _, text := range leaseWaitCutText {
		failures["lease_wait_cut"] = failures["lease_wait_cut"] || strings.Count(strings.ReplaceAll(sources["files.go"], "\r\n", "\n"), text) != 1
	}
	for key, failed := range failures {
		if failed {
			result = append(result, key)
		}
	}
	slices.Sort(result)
	return result
}

// leaseWaitCutStub declares the cut files.go declares, so that files_other.go
// type-checks alone.
const leaseWaitCutStub = "\ntype leaseWaitCut struct{ cause error }\n\nfunc (e *leaseWaitCut) Error() string { return e.cause.Error() }\n"

// leaseWaitGate type-checks files_other.go alone as goos builds it, standard
// packages from source, and returns the constant value of the gate that comes
// before the EWOULDBLOCK test: true returns the first open's result (P4).
func leaseWaitGate(goos, source string) (first, ok bool) {
	target, fset, packages := build.Default, token.NewFileSet(), map[string]*types.Package{"unsafe": types.Unsafe}
	target.GOOS, target.GOARCH, target.CgoEnabled = goos, "arm64", false
	var importer readers4Importer
	importer = func(path string) (*types.Package, error) {
		if packages[path] != nil {
			return packages[path], nil
		}
		found, err := target.Import(path, "", 0)
		files := []*ast.File(nil)
		for index := 0; err == nil && index < len(found.GoFiles); index++ {
			var file *ast.File
			file, err = parser.ParseFile(fset, filepath.Join(found.Dir, found.GoFiles[index]), nil, parser.SkipObjectResolution)
			files = append(files, file)
		}
		if err != nil {
			return nil, err
		}
		packages[path], _ = (&types.Config{Importer: importer, Error: func(error) {}}).Check(path, fset, files, nil)
		return packages[path], nil
	}
	matcher := target
	matcher.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(source)), nil }
	match, err := matcher.MatchFile(".", "files_other.go")
	file, parseErr := parser.ParseFile(fset, "files_other.go", source, 0)
	var wait *ast.FuncDecl
	for index := 0; err == nil && parseErr == nil && index < len(file.Decls); index++ {
		if function, isFunction := file.Decls[index].(*ast.FuncDecl); isFunction && function.Name.Name == "waitOutLease" {
			wait = function
		}
	}
	if !match || wait == nil {
		return false, false
	}
	info, value, seen := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}, constant.Value(nil), false
	ok = true
	_, _ = (&types.Config{Importer: importer, Error: func(err error) {
		ok = ok && (err.(types.Error).Pos < wait.Pos() || err.(types.Error).Pos >= wait.End())
	}}).Check("localcell", fset, []*ast.File{file}, info)
	ast.Inspect(wait.Body, func(node ast.Node) bool {
		if branch, isIf := node.(*ast.IfStmt); isIf && !seen {
			if or, isOr := branch.Cond.(*ast.BinaryExpr); isOr && or.Op == token.LOR {
				value = info.Types[or.X].Value
			}
			seen = true
		}
		return !seen
	})
	ok = ok && value != nil && value.Kind() == constant.Bool
	return ok && constant.BoolVal(value), ok
}

// The every-platform contract of the lease wait (R6), with LF and CRLF endings;
// each row's mutation reports its key with both.
func TestLeaseWaitSourceContract(t *testing.T) {
	sources, ended := admissionReadSources(t), func(input map[string]string, ending string) map[string]string {
		output := map[string]string{}
		for name, source := range input {
			output[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", ending)
		}
		return output
	}
	for _, ending := range []string{"\n", "\r\n"} {
		if failures := leaseWaitContract(ended(sources, ending)); len(failures) != 0 {
			t.Fatalf("lease wait source contract failed with keys %v", failures)
		}
	}
	sources = ended(sources, "\n")
	const reader, writer = "os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)", "os.OpenFile(path, flags|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, mode)"
	waited := func(open string) string {
		return "return waitOutLease(ctx, path, os.Lstat, func() (*os.File, error) {\n\t\treturn " + open + "\n\t})"
	}
	for _, row := range []struct{ name, file, before, after, want string }{
		{"wait_any_error", "files_other.go", "!errors.Is(err, syscall.EWOULDBLOCK)", "false", "lease_wait_body"},
		{"wait_no_lstat", "files_other.go", "info, statErr := lstat(path); statErr != nil || !info.Mode().IsRegular() || ", "", "lease_wait_body"},
		{"wait_blocking_retry", "files_other.go", "result, err := open()", "result, err := open(); _, _ = os.Open(path)", "lease_wait_body"},
		{"wait_ceiling", "files_other.go", "60 * time.Second", "600 * time.Second", "lease_wait_values"},
		{"reader_direct", "files_other.go", waited(reader), "return " + reader, "lease_wait_caller:files_other.go:openPrivateReader"},
		{"writer_direct", "files_other.go", waited(writer), "return " + writer, "lease_wait_caller:files_other.go:openPrivateWriter"},
		{"checkpoint_direct", "checkpoint_unix.go", "waitOutLease(ctx, name, root.Lstat, func() (*os.File, error) { return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) })",
			"root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)", "lease_wait_caller:checkpoint_unix.go:openCheckpointSource"},
		{"checkpoint_other_lstat", "checkpoint_unix.go", "waitOutLease(ctx, name, root.Lstat,", "waitOutLease(ctx, name, os.Lstat,", "lease_wait_caller:checkpoint_unix.go:openCheckpointSource"},
		{"lock_direct", "lock_unix.go", "waitOutLease(ctx, path, os.Lstat, func() (int, error) { return syscall.Open(path, flags, 0o600) })", "syscall.Open(path, flags, 0o600)",
			"lease_wait_caller:lock_unix.go:openLockFile"},
		{"new_never_waiting_open", "files.go", "func regularFile(", "func readLeased(path string) (*os.File, error) { return os.OpenFile(path, syscall.O_NONBLOCK, 0) }\nfunc regularFile(",
			"lease_wait_caller:files.go:readLeased"},
		{"windows_wait", "files_windows.go", "\treturn openPrivateFile(path, os.O_RDONLY, 0)\n",
			"\treturn waitOutLease(context.Background(), path, os.Lstat, func() (*os.File, error) { return openPrivateFile(path, os.O_RDONLY, 0) })\n", "lease_wait_windows"},
		{"values_assigned", "files.go", "func regularFile(", "func leaseLonger() { leaseWait.ceiling *= 10 }\nfunc regularFile(", "lease_wait_values"},
		{"parse_error", "files_other.go", "func waitOutLease[T any](", "func waitOutLease[T any]((", "parse:files_other.go"},
		{"wait_not_gated", "files_other.go", `runtime.GOOS != "linux" || `, "", "lease_wait_body"},
		{"wait_gate_inverted", "files_other.go", `runtime.GOOS != "linux"`, `runtime.GOOS == "linux"`, "lease_wait_body"},
		{"lock_flags_blocking", "lock_unix.go", "syscall.O_NOFOLLOW | syscall.O_NONBLOCK\n", "syscall.O_NOFOLLOW\n", "lease_wait_lock"},
		{"lock_flags_cleared", "lock_unix.go", "syscall.Open(path, flags, 0o600)", "syscall.Open(path, lockFlagsOnce(&flags), 0o600)", "lease_wait_lock"},
		{"lock_local_shadow", "lock_unix.go", "\tfd, err := waitOutLease(", "\twaitOutLease := func(_ string, _ func(string) (os.FileInfo, error), open func() (int, error)) (int, error) { return open() }\n\tfd, err := waitOutLease(", "lease_wait_shadow:lock_unix.go"},
		{"lock_probe_helper", "lock_unix.go", "func unlockFile(", "func lockProbe(path string) {\n\tif fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0); err == nil {\n\t\t_ = syscall.Close(fd)\n\t}\n}\n\nfunc unlockFile(", "lease_wait_caller:lock_unix.go:lockProbe"},
		{"plain_open_changed", "files_other.go", "return os.OpenFile(path, flags, mode)", "return os.OpenFile(path, flags|syscall.O_NONBLOCK, mode)", "lease_wait_caller:files_other.go:openPrivateFile"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if strings.Count(sources[row.file], row.before) != 1 {
				t.Fatal("lease wait contract mutation anchor is not unique")
			}
			mutated := maps.Clone(sources)
			mutated[row.file] = strings.Replace(sources[row.file], row.before, row.after, 1)
			for _, ending := range []string{"\n", "\r\n"} {
				if failures := leaseWaitContract(ended(mutated, ending)); !slices.Contains(failures, row.want) {
					t.Fatalf("lease wait source contract reported keys %v without the row key %s", failures, row.want)
				}
			}
		})
	}
	// P4 by type, on every platform: as darwin builds it the gate is the constant
	// true, so the first open's result returns; as linux builds it, false.
	for goos, first := range map[string]bool{"darwin": true, "linux": false} {
		t.Run("gate_"+goos, func(t *testing.T) {
			if got, ok := leaseWaitGate(goos, sources["files_other.go"]+leaseWaitCutStub); !ok || got != first {
				t.Fatal("waitOutLease does not return its first result exactly where the target is not Linux")
			}
		})
	}
}
