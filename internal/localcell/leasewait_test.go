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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// leaseWaitSites reach the system only through waitOutLease (R3, R4); the dark
// drill record open is excluded (OQ9). The checkpoint tree's open reaches it
// through the private beneath walk (LC-CKPT-WALK-1C1).
var leaseWaitSites = []string{"files_other.go:openPrivateReader", "files_other.go:openPrivateWriter", "checkpoint_unix.go:openCheckpointSource", "lock_unix.go:openLockFile",
	"checkpoint_tree_unix.go:open"}

// leaseWaitExcluded: the dark drill record open (OQ9); leaseWaitPlain: the one
// plain open in a file holding a site, pinned to pass its caller's flags only.
const leaseWaitExcluded, leaseWaitPlain = "drill_root.go:drillRootHeld", "files_other.go:openPrivateFile"

// leaseWaitBody and leaseWaitValues are waitOutLease and leaseWait's value as
// gofmt prints them, comments aside: calls time.Now(), open(), lstat(path),
// time.Now(), time.NewTimer(delay), leaseWaitDone(ctx), pause.Stop(), ctx.Err()
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
		case <-leaseWaitDone(ctx):
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

// leaseWaitBeneath: open, privateBeneathOpen(t.recovery, X, t.want, label), is
// the one return of the second function literal passed to a waitOutLease call
// among calls, with function's own context and label, whose first function
// literal's one return is privateBeneathLstat(t.recovery, X, t.want): the
// walk's no-follow stat of the same name and identity.
func leaseWaitBeneath(function *ast.FuncDecl, calls []*ast.CallExpr, open *ast.CallExpr) bool {
	if len(open.Args) != 4 || types.ExprString(open.Args[0]) != "t.recovery" || types.ExprString(open.Args[2]) != "t.want" {
		return false
	}
	return slices.ContainsFunc(calls, func(wait *ast.CallExpr) bool {
		if len(wait.Args) != 4 || types.ExprString(wait.Fun) != "waitOutLease" || !leaseWaitOwnContext(function, wait.Args[0]) ||
			types.ExprString(wait.Args[1]) != types.ExprString(open.Args[3]) {
			return false
		}
		lstat, isLstat := wait.Args[2].(*ast.FuncLit)
		opener, isOpener := wait.Args[3].(*ast.FuncLit)
		return isLstat && isOpener && opener.Pos() < open.Pos() && open.End() < opener.End() && writerOneReturn(opener.Body) == "return "+types.ExprString(open) &&
			writerOneReturn(lstat.Body) == "return privateBeneathLstat(t.recovery, "+types.ExprString(open.Args[1])+", t.want)"
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
// (LC-LEASE-2A, 2B, 2C1, 2C2, 2D R5, 2D2 and 2D3; 2D's seam), in the package reference contract's notation.
var leaseWaitOpeners = []string{"waitOutLease", "openPrivateReader", "openPrivateWriter", "openPostgresReader", "openPlainReader", "openLockFile",
	"(*Cell).openCheckpointSource", "openRegularBy", "readRegular", "openRegular", "openRegularWriter", "openServiceRecord", "checkpointComplete",
	"readMaintenanceFenceJSON", "(*Cell).readMaintenanceFence", "acquireLock", "(*Cell).readJournal", "(*Cell).journal", "(*Cell).journalCause",
	"(*Cell).step", "(*Cell).begin", "(*Cell).finish", "(*Cell).discardJournalTail", "(*Cell).finishRemoval", "(*Cell).finishRetainedRemoval",
	"(*Cell).postmaster", "(*Cell).maintenanceFenceAllows", "(*Cell).refuseBeforeLockNames", "(*Cell).password", "(*Cell).logFile", "(*Cell).childEnvironment",
	"(*Cell).prepareComponentPorts", "(*Cell).prepareComponentPortsFor", "(*Cell).credentialPassword", "(*Cell).validatePrincipalPasswords", "(*Cell).makePrincipalPasswords",
	"(*Cell).principalDSN", "(*Cell).maintenanceDSN", "(*Cell).lastValueFreeDiagnostic", "(*Cell).admitReinstall", "(*Cell).reinstallAvailable", "(*Cell).finishReinstall",
	"(*Cell).checkpointOps", "(*Cell).checkpointSummaries", "(*Cell).closeCheckpointFailure", "(*Cell).closeCheckpointLeftover", "(*Cell).listCheckpoints",
	"(*Cell).openCheckpointEntry", "(*Cell).admitDeleteCheckpoint", "openPrincipalCredentialFile", "(*checkpointTree).open", "(*checkpointTree).witness",
	"readJSON", "readManifest", "readManifestBy", "readServiceRecordJSON", "readManifestJSON", "openAdmissionRecord", "(*Cell).readServiceControlEndpointRecordBy", "(*Cell).serviceControlSecretBy", "(*Cell).cleanupInterruption", "(*Cell).processState", "(*Cell).processStateWithRecord", "serviceAdapterRemovalHostLock", "(*Cell).checkpointCold",
	"(*Cell).own", "(*Cell).ownExisting", "(*Cell).verifyAll", "(*Cell).recoverPostgresRecord"}

// leaseWaitPlaceholders are the non-test calls into leaseWaitOpeners that pass
// context.Background() instead of their declaration's own context, by file and
// declaration with their counts (R6); it grows until the host's slices (OQ-H).
var leaseWaitPlaceholders = map[string]int{"checkpoint_verify.go:(*Cell).verifyCheckpoint": 1, "commands.go:(*Cell).databaseCommand": 1,
	"commands.go:(*Cell).pgCommand": 1, "commands.go:(*Cell).startChildPrepared": 3, "commands.go:(*Cell).waitReady": 2, "drill_reserve.go:(*Cell).reserveDrillLocked": 2,
	"drill_root.go:(*Cell).deleteDrillLocked": 2, "drill_root.go:(*Cell).drillRootList": 1, "drill_storage.go:(*Cell).admitDrillStorageLocked": 3,
	"files.go:(*Cell).readUpdateJournal":                3,
	"installer_principals.go:(*Cell).installPrincipals": 1, "installer_principals.go:(*Cell).installerAdmin": 4,
	"installer_principals.go:(*Cell).observePrivilegedSessions": 1, "installer_principals.go:(*Cell).openMaintenance": 1,
	"installer_principals.go:(*Cell).principalOperations": 1, "installer_principals.go:(*Cell).provisionInstallerPrincipals": 2,
	"installer_principals.go:(*Cell).runPrincipalMigration": 1, "installer_principals.go:(*Cell).verifyCredentialPurposes": 1,
	"installer_principals.go:(*Cell).verifyInstallerStartupCatalog": 1, "installer_principals.go:(*Cell).verifyRuntimePrincipalStart": 1,
	"launch_unix.go:(*Cell).launchChild": 1, "lifecycle.go:(*Cell).Install": 6, "lifecycle.go:(*Cell).Remove": 11, "lifecycle.go:(*Cell).Repair": 11,
	"lifecycle.go:(*Cell).Start": 15, "lifecycle.go:(*Cell).Status": 1,
	"lifecycle.go:(*Cell).stopSelected": 4, "lifecycle.go:(*Cell).stopSelectedLegacy": 5, "lifecycle.go:(*Cell).stopWithServiceFence": 6,
	"lock_contention.go:classifyOwnershipFailure":        1,
	"port_recovery.go:(*Cell).waitReadyWithPortRecovery": 2, "postgres_unix.go:(*Cell).startPostgres": 3, "postgres_windows.go:(*Cell).pendingPostgresWithoutPID": 1,
	"postgres_windows.go:(*Cell).pendingPostmasterPID": 1, "postgres_windows.go:(*Cell).startPostgres": 3,
	"readiness.go:(*Cell).childReadinessExit": 1, "reinstall.go:(*Cell).Reinstall": 2, "reinstall.go:(*Cell).reinstallLocked": 6,
	"service_adapter_admission.go:serviceAdapterAdmissionForStep": 1, "service_boot.go:(*Cell).bindServiceProcessBootWithReader": 1, "service_boot.go:(*Cell).reconcileOldBoot": 2, "service_boot.go:(*Cell).reconcileServiceBoot": 3,
	"service_control_endpoint.go:(*Cell).publishServiceControlEndpointWithBoot": 1, "service_control_endpoint.go:(*Cell).readServiceControlEndpointRecord": 1,
	"service_control.go:(*Cell).readServiceControlSecret": 1, "service_control_endpoint.go:(*Cell).readServiceControlEndpointWithBoot": 2, "service_control_exit.go:(*Cell).exitServiceHostForRemoval": 3,
	"service_control_exit.go:(*Cell).serviceHostExitIdentity": 1, "service_control_exit.go:(*Cell).serviceHostLeaseHeld": 1,
	"service_control_exit.go:(*Cell).waitServiceHostExit": 2, "service_control_exit.go:(*supervisor).finishHostExit": 1,
	"service_descriptor.go:(*Cell).PrepareService": 3, "service_descriptor.go:(*Cell).RepairService": 2, "service_descriptor.go:(*Cell).readServiceDescriptorForOwner": 2,
	"service_descriptor.go:(*Cell).SetServiceDesiredState": 1, "service_descriptor.go:(*Cell).serviceControlSecret": 1,
	"service_descriptor.go:(*Cell).setServiceFaulted": 1, "service_descriptor.go:(*Cell).setServiceDesiredStateLocked": 1, "service_foreground.go:(*Cell).acquireServiceLeaseWithBoot": 1,
	"service_health.go:(*Cell).ServiceHealth": 3, "service_health.go:(*Cell).healthPrincipal": 1,
	"service_health.go:(*Cell).serviceAPICredential": 1, "service_health.go:(*Cell).serviceLiveTree": 2, "service_launchd.go:(*launchdAgentAdapter).Apply": 1,
	"service_launchd.go:(*launchdAgentAdapter).Remove":     2,
	"service_launchd.go:(*launchdAgentAdapter).ownedPlist": 1, "service_maintenance.go:(*Cell).serviceMaintenanceObservation": 2,
	"service_shutdown_other.go:(*Cell).stopChildOutcome": 1, "service_shutdown_windows.go:(*Cell).stopChildOutcomeWithBudget": 1,
	"service_shutdown_windows.go:waitShutdownReady": 1, "service_supervisor.go:(*Cell).Run": 1, "service_supervisor.go:(*supervisor).load": 1,
	"service_supervisor.go:(*supervisor).rememberProcesses": 2, "service_systemd_adapter.go:(*systemdUserAdapter).Apply": 1,
	"service_systemd_adapter.go:(*systemdUserAdapter).Remove":    2,
	"service_systemd_adapter.go:(*systemdUserAdapter).ownedUnit": 1, "service_windows_task.go:(*windowsLogonTaskAdapter).Apply": 1,
	"service_windows_task.go:(*windowsLogonTaskAdapter).Remove": 2, "service_windows_task.go:(*windowsLogonTaskAdapter).Start": 1,
	"service_windows_task.go:(*windowsLogonTaskAdapter).mutationLock": 1, "stop_windows.go:(*Cell).stopPostgres": 1, "stop_windows.go:(*Cell).windowsStopRecord": 1, "update_record.go:(*Cell).authenticateUpdateInstallation": 1,
	"update_record.go:(*Cell).readDerivedUpdateRecord": 1, "update_record.go:(*Cell).readUpdateIntent": 1}

// leaseWaitCutText: per file, each text exactly once. files.go's cut, then
// openRegularBy's and openRegularWriter's opens with the cut returned before
// any Lstat classifies it (R4); update_fence.go's fence reads return the cut,
// never the record's refusal or an active fence (LC-LEASE-2B R3); files.go's
// step returns a cut wrapped, never its bare failure (LC-LEASE-2C1 R3);
// refuseBeforeLockNames returns a cut, never defers it (LC-LEASE-2C2 R6); so do 2D's R3 readers.
var leaseWaitCutText = map[string][]string{"files.go": {"type leaseWaitCut struct{ cause error }\n\nfunc (e *leaseWaitCut) Error() string { return \"local cell lease wait ended: \" + " +
	"e.cause.Error() }\nfunc (e *leaseWaitCut) Unwrap() error { return e.cause }\n", "open error) (*os.File, error) {\n\tf, err := openReader(ctx, path)\n" +
	"\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n\tif err != nil {\n\t\tinfo, err := os.Lstat(path)\n",
	"\tf, err := openPrivateWriter(ctx, path, flags, mode)\n\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn nil, cut\n\t}\n\tif err != nil {\n\t\tif info, err := os.Lstat(path);",
	"\t\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\t\t// A wait that ctx ended crosses as the value-free cut only.\n\t\t\treturn fmt.Errorf(\"local cell %s: %s failed: %w\", operation, step, cut)\n\t\t}\n" +
		"\t\treturn fmt.Errorf(\"local cell %s: %s failed\", operation, step)\n"},
	"update_fence.go": {"\tf, err := openRegular(ctx, path, refused, refused, refused)\n\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn cut\n\t} else if err != nil {\n\t\treturn refused\n\t}\n",
		"\tif err := read(path, &fence); errors.As(err, new(*leaseWaitCut)) {\n\t\treturn nil, err\n\t} else if err != nil || !fence.valid() {\n\t\treturn nil, errMaintenanceFenceActive\n\t}\n"},
	"checkpoint.go": {"\theld, err := acquireLock(ctx, c.cfg.Root, false)\n\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn err\n\t} else if err != nil {\n\t\treturn nil\n\t}\n"},
	"installer_principal_credentials.go": {"\tf, err := openPrincipalCredentialFile(ctx, path, os.O_RDONLY, 0)\n\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn \"\", cut\n" +
		"\t} else if err != nil {\n\t\treturn \"\", ErrLocalPrincipalCredentialsRefused\n\t}\n", "\t\tvalue, err := c.credentialPassword(ctx, m, purpose)\n" +
		"\t\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\t\treturn cut\n\t\t} else if err != nil || seen[value] {\n\t\t\treturn ErrLocalPrincipalCredentialsRefused\n\t\t}\n"},
	"lifecycle.go": {"data, err := readRegular(ctx, openPostgresReader, postmasterBounded, c.path(\"pg\", \"postmaster.pid\"), unreadable, unreadable, unreadable)\n" +
		"\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn cut\n\t}\n\tif err != nil {\n\t\treturn unreadable\n\t}\n"},
	"reinstall.go": {"c.path(\"pg\", \"PG_VERSION\"),\n\t\terrReinstallState, errReinstallState, errReinstallState)\n\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n" +
		"\t\treturn cut\n\t} else if err != nil {\n\t\treturn errReinstallState\n\t}\n"}}

// LC-LEASE-2C2's lease_wait_detached: R3's mark (files.go) and the wait's reader
// (files_other.go), each exactly once; leaseWaitDetached and its key named only
// there and once in each released verb, its first statement; no declaration
// that holds a context and is reached only from the released verbs (R4) holds
// a placeholder: leaseWaitReleased, which the census also holds to its callers'
// own context (G2, G5), and with 2D leaseWaitDetachedHeld.
var leaseWaitDetachedTexts = map[string][]string{"files.go": {"type leaseWaitDetachedKey struct{}\n",
	"func leaseWaitDetached(ctx context.Context) context.Context {\n\treturn context.WithValue(ctx, leaseWaitDetachedKey{}, true)\n}\n"},
	"files_other.go": {"func leaseWaitDone(ctx context.Context) <-chan struct{} {\n\tif ctx.Value(leaseWaitDetachedKey{}) != nil {\n\t\treturn nil\n\t}\n\treturn ctx.Done()\n}\n"}}
var leaseWaitDetachedNames = map[string]int{"files.go:leaseWaitDetachedKey": 1, "files.go:leaseWaitDetached": 2, "files_other.go:leaseWaitDone": 1,
	"checkpoint.go:(*Cell).Checkpoint": 1, "checkpoint.go:(*Cell).DeleteCheckpoint": 1, "lifecycle.go:(*Cell).Inspect": 1}
var leaseWaitReleased = []string{"checkpoint.go:(*Cell).Checkpoint", "checkpoint.go:(*Cell).DeleteCheckpoint", "lifecycle.go:(*Cell).Inspect",
	"checkpoint.go:(*Cell).admitCheckpoint", "checkpoint.go:(*Cell).admitCheckpointDry", "checkpoint.go:(*Cell).checkpointLocked", "checkpoint.go:(*Cell).copyCheckpoint",
	"checkpoint.go:(*Cell).deleteCheckpointLocked", "checkpoint.go:(*Cell).inventoryCheckpoint", "checkpoint.go:(*Cell).witnessCheckpoint"}

// leaseWaitDetachedHeld are the rest of 2C2's R1 twenty and checkpointOps, whose
// ops.open binds its own context (2D): openers the opener census decides (a
// placeholder handed to one is counted, an own context is live); checkpointOps is
// shared with the dark drill, whose calls pass the placeholder (2C2's report).
var leaseWaitDetachedHeld = []string{"checkpoint_unix.go:(*Cell).openCheckpointSource", "checkpoint_windows.go:(*Cell).openCheckpointSource",
	"checkpoint_other.go:(*Cell).openCheckpointSource", "checkpoint.go:checkpointComplete", "checkpoint.go:(*Cell).refuseBeforeLockNames",
	"checkpoint.go:(*Cell).admitDeleteCheckpoint", "checkpoint.go:(*Cell).checkpointCold", "checkpoint.go:(*Cell).checkpointSummaries",
	"checkpoint.go:(*Cell).closeCheckpointFailure", "checkpoint.go:(*Cell).closeCheckpointLeftover", "checkpoint.go:(*Cell).listCheckpoints",
	"checkpoint.go:(*Cell).openCheckpointEntry", "checkpoint.go:(*Cell).checkpointOps"}

// leaseWaitValueUses are the non-test uses of an opener other than as a callee
// (G5), including openers handed to an admission reader or readRegular, by
// file and declaration with their counts; leaseWaitReleasedCallers are the
// non-test calls into leaseWaitReleased from outside it (G2, G5).
var leaseWaitValueUses = map[string]int{
	"files.go:readManifest": 1, "service_control.go:(*Cell).readServiceControlSecret": 1,
	"service_control_endpoint.go:(*Cell).readServiceControlEndpointRecord": 1, "service_control_endpoint.go:(*Cell).readServiceControlEndpointWithBoot": 1,
	"service_descriptor.go:(*Cell).readServiceDescriptorForOwner": 1, "service_descriptor.go:(*Cell).serviceControlSecret": 1,
	"checkpoint.go:(*Cell).admitCheckpoint": 1, "commands.go:(*Cell).password": 1, "files.go:openRegular": 1,
	"lifecycle.go:(*Cell).postmaster": 1, "lifecycle.go:(*Cell).recoverPostgresRecord": 1, "reinstall.go:(*Cell).reinstallAvailable": 1, "service_descriptor.go:openServiceRecord": 1}
var leaseWaitReleasedCallers = map[string]int{"lifecycle.go:(*Cell).Status": 1}

// leaseWaitCall is one use of an opener or a released declaration: its site
// and file offset.
type leaseWaitCall struct {
	site   string
	offset int
}

// leaseWaitTyped type-checks the sources system builds through the package
// reference contract's world and returns by kind each call into
// leaseWaitOpeners, a callee resolved by type (a method by its receiver type,
// an interface method of a listed method's name and signature, or 2D's seam by
// its variable): "live" with its declaration's own context parameter first (a
// package-level seam literal's own included), "placeholder" with a call of
// package context's Background (by object), else "other"; each other use of an
// opener ("value"); each call into leaseWaitReleased ("released" with its own
// context, else "released_other"); the openers it resolves; and whether the
// world or the check failed.
func leaseWaitTyped(world *referencesWorld, system string, sources map[string]string) (calls map[leaseWaitCall]string, resolved map[string]bool, failed bool) {
	world.Lock()
	defer world.Unlock()
	if world.listed == nil {
		world.load(system)
	}
	if calls, resolved = map[leaseWaitCall]string{}, map[string]bool{}; world.failed {
		return calls, resolved, true
	}
	matcher, fset, files := world.context, token.NewFileSet(), []*ast.File(nil)
	matcher.OpenFile = func(path string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(sources[path])), nil }
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		if match, err := matcher.MatchFile(".", name); err == nil && match {
			file, err := parser.ParseFile(fset, name, sources[name], parser.SkipObjectResolution)
			if failed = failed || err != nil; err == nil {
				files = append(files, file)
			}
		}
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	config := types.Config{Importer: world.importer(world.listed[world.self].ImportMap, world.listed[world.self].Dir), Sizes: types.SizesFor("gc", "amd64"),
		Error: func(error) { failed = true }}
	self, _ := config.Check(world.self, fset, files, info)
	targets, released, methods := map[types.Object]bool{}, map[types.Object]bool{}, map[string]types.Type{}
	for _, name := range leaseWaitOpeners {
		if object := world.resolve(self, name); object != nil {
			targets[object], resolved[name], methods[object.Name()] = true, true, object.Type()
		}
	}
	for _, site := range leaseWaitReleased {
		if _, name, _ := strings.Cut(site, ":"); world.resolve(self, name) != nil {
			released[world.resolve(self, name)] = true
		}
	}
	for _, file := range files {
		name := fset.PositionFor(file.Package, false).Filename
		for _, declaration := range file.Decls {
			function, _ := declaration.(*ast.FuncDecl)
			referencesDeclarations(declaration, func(owner string, node ast.Node) {
				called, holder := map[*ast.Ident]bool{}, leaseWaitHolder(function, node)
				ast.Inspect(node, func(node ast.Node) bool {
					if identifier, ok := node.(*ast.Ident); ok && !called[identifier] {
						if object := leaseWaitOrigin(info.Uses[identifier]); object != nil && targets[object] {
							calls[leaseWaitCall{name + ":" + owner, fset.PositionFor(identifier.Pos(), false).Offset}] = "value"
						}
					}
					call, _ := node.(*ast.CallExpr)
					callee, seam := (*types.Func)(nil), false
					if call != nil {
						called[referencesIdentifier(call.Fun)] = true
						callee, _ = referencesNamed(info, call.Fun).(*types.Func)
						seam = callee == nil && targets[leaseWaitOrigin(referencesNamed(info, call.Fun))] // LC-LEASE-2D: the seam by its variable
					}
					if callee == nil && !seam {
						return true
					}
					use, own, receiver := leaseWaitCall{name + ":" + owner, fset.PositionFor(call.Pos(), false).Offset}, leaseWaitOwnTyped(info, holder, call.Args), (*types.Var)(nil)
					if !seam {
						receiver = callee.Signature().Recv()
					}
					switch background := (*types.Func)(nil); {
					case !seam && released[callee.Origin()]:
						calls[use] = map[bool]string{true: "released", false: "released_other"}[own]
					case !seam && !targets[callee.Origin()] && (receiver == nil || !types.IsInterface(receiver.Type()) || !types.Identical(callee.Type(), methods[callee.Name()])):
					case own:
						calls[use] = "live"
					default: // a placeholder by its text and by its object (G2)
						if argument, ok := slices.Concat(call.Args, []ast.Expr{nil})[0].(*ast.CallExpr); ok && types.ExprString(argument) == "context.Background()" {
							background, _ = referencesNamed(info, argument.Fun).(*types.Func)
						}
						calls[use] = map[bool]string{true: "placeholder", false: "other"}[background != nil && background.FullName() == "context.Background"]
					}
					return true
				})
			})
		}
	}
	return calls, resolved, failed
}

// leaseWaitHolder is the declaration whose context parameters are a call's
// own: function, or for a package-level seam (a value specification of one
// function literal, LC-LEASE-2D) the literal's type.
func leaseWaitHolder(function *ast.FuncDecl, node ast.Node) *ast.FuncDecl {
	if value, ok := node.(*ast.ValueSpec); ok && len(value.Values) == 1 {
		if literal, ok := value.Values[0].(*ast.FuncLit); ok {
			return &ast.FuncDecl{Type: literal.Type}
		}
	}
	return function
}

// leaseWaitOrigin is the generic origin of a function or a variable (the
// seam), or the object itself.
func leaseWaitOrigin(object types.Object) types.Object {
	switch named := object.(type) {
	case *types.Func:
		return named.Origin()
	case *types.Var:
		return named.Origin()
	}
	return object
}

// leaseWaitOwnTyped: the first argument is, by its object, a context.Context
// parameter of function itself.
func leaseWaitOwnTyped(info *types.Info, function *ast.FuncDecl, arguments []ast.Expr) bool {
	if function == nil || len(arguments) == 0 {
		return false
	}
	identifier, ok := arguments[0].(*ast.Ident)
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			if ok && info.Uses[identifier] != nil && info.Uses[identifier] == info.Defs[name] && types.ExprString(field.Type) == "context.Context" {
				return true
			}
		}
	}
	return false
}

// leaseWaitContract returns every failed key of R6 over the non-test sources,
// read without build constraints and with CRLF normalised; LC-LEASE-2A adds
// lease_wait_placeholder and lease_wait_cut, LC-LEASE-2B resolves the first by
// type on every references system (a call counted once) and reports the
// second per file too.
func leaseWaitContract(sources map[string]string) (result []string) {
	failures, fset, counts, found := map[string]bool{}, token.NewFileSet(), map[string]int{}, map[string]bool{}
	placeholders, lf, typed, group := map[string]int{}, map[string]string{}, [len(referencesSystems)]map[leaseWaitCall]string{}, sync.WaitGroup{}
	resolved, failed := [len(referencesSystems)]map[string]bool{}, [len(referencesSystems)]bool{}
	detached, entries := map[string]int{}, map[string]bool{} // LC-LEASE-2C2's lease_wait_detached
	values, released, reassigned := map[string]int{}, map[string]int{}, map[string]int{}
	for name, source := range sources {
		lf[name] = strings.ReplaceAll(source, "\r\n", "\n")
	}
	for index, system := range referencesSystems {
		group.Go(func() {
			typed[index], resolved[index], failed[index] = leaseWaitTyped(&referencesWorlds[index], system, lf)
		})
	}
	group.Wait()
	calls := map[leaseWaitCall]string{}
	for index, system := range referencesSystems {
		failures["lease_wait_types:"+system] = failed[index]
		for call, kind := range typed[index] {
			if seen, ok := calls[call]; ok && seen != kind {
				kind = "other" // systems that disagree on a use
			}
			calls[call] = kind
		}
	}
	for call, kind := range calls {
		switch _, owner, _ := strings.Cut(call.site, ":"); kind {
		case "live": // an own context enters the layer only from the layer or a released declaration
			failures["lease_wait_live:"+call.site] = failures["lease_wait_live:"+call.site] || !slices.Contains(leaseWaitOpeners, owner) && !slices.Contains(leaseWaitReleased, call.site)
		case "value":
			values[call.site]++
		case "released", "released_other":
			released[call.site]++
			failures["lease_wait_released:"+call.site] = failures["lease_wait_released:"+call.site] || kind != "released"
		default:
			placeholders[call.site]++
			failures["lease_wait_placeholder:"+call.site] = failures["lease_wait_placeholder:"+call.site] || kind != "placeholder"
		}
	}
	for _, site := range slices.Concat(slices.Collect(maps.Keys(values)), slices.Collect(maps.Keys(leaseWaitValueUses))) {
		failures["lease_wait_value:"+site] = values[site] != leaseWaitValueUses[site]
	}
	for _, site := range slices.Concat(slices.Collect(maps.Keys(released)), slices.Collect(maps.Keys(leaseWaitReleasedCallers))) {
		failures["lease_wait_released:"+site] = failures["lease_wait_released:"+site] || !slices.Contains(leaseWaitReleased, site) && released[site] != leaseWaitReleasedCallers[site]
	}
	for _, name := range leaseWaitOpeners {
		failures["lease_wait_unknown:"+name] = !slices.ContainsFunc(resolved[:], func(names map[string]bool) bool { return names[name] })
	}
	for _, name := range slices.Sorted(maps.Keys(sources)) {
		file, err := parser.ParseFile(fset, name, lf[name], parser.ParseComments)
		if failures["parse:"+name] = err != nil; err != nil {
			continue
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
			referencesDeclarations(declaration, func(owner string, node ast.Node) {
				function, _ := node.(*ast.FuncDecl) // G1: a layer declaration that assigns or exposes its own context
				function = leaseWaitHolder(function, node)
				layer := function != nil && (slices.Contains(leaseWaitOpeners, owner) || slices.Contains(leaseWaitReleased, name+":"+owner))
				ast.Inspect(node, func(node ast.Node) bool {
					var targets []ast.Expr
					switch node := node.(type) {
					case *ast.Ident:
						if node.Name == "leaseWaitDetached" || node.Name == "leaseWaitDetachedKey" {
							detached[name+":"+owner]++
						}
					case *ast.AssignStmt:
						targets = node.Lhs
					case *ast.UnaryExpr:
						targets = []ast.Expr{map[bool]ast.Expr{true: node.X}[node.Op == token.AND]}
					case *ast.RangeStmt:
						targets = []ast.Expr{node.Key, node.Value}
					}
					for _, target := range targets {
						reassigned[name+":"+owner] += map[bool]int{true: 1}[layer && leaseWaitOwnContext(function, target)]
					}
					return true
				})
				if function != nil && function.Body != nil && len(function.Body.List) > 0 && strings.HasPrefix(owner, "(*Cell).") {
					first, _ := function.Body.List[0].(*ast.AssignStmt) // ctx = leaseWaitDetached(ctx), ctx its own
					entries[name+":"+owner] = first != nil && leaseWaitPrint(fset, first) == "ctx = leaseWaitDetached(ctx)" && leaseWaitOwnContext(function, first.Rhs[0].(*ast.CallExpr).Args[0])
				}
			})
		}
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
				} else if types.ExprString(call.Fun) == "privateBeneathOpen" {
					failures["lease_wait_caller:"+site] = failures["lease_wait_caller:"+site] || !leaseWaitBeneath(function, calls, call)
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
	for file, texts := range leaseWaitDetachedTexts {
		for _, text := range texts {
			failures["lease_wait_detached"] = failures["lease_wait_detached"] || strings.Count(lf[file], text) != 1
		}
	}
	for _, site := range slices.Concat(slices.Collect(maps.Keys(detached)), slices.Collect(maps.Keys(leaseWaitDetachedNames))) {
		failures["lease_wait_detached"] = failures["lease_wait_detached"] || detached[site] != leaseWaitDetachedNames[site] ||
			strings.Contains(site, ":(*Cell).") && !entries[site]
	}
	for _, site := range slices.Concat(leaseWaitReleased, leaseWaitDetachedHeld) {
		failures["lease_wait_detached"] = failures["lease_wait_detached"] || placeholders[site] != 0
	}
	for _, site := range slices.Concat(slices.Collect(maps.Keys(reassigned)), leaseWaitReleased[:3]) { // only the three entries' detach
		failures["lease_wait_reassigned:"+site] = reassigned[site] != map[bool]int{true: 1}[slices.Contains(leaseWaitReleased[:3], site)]
	}
	for file, texts := range leaseWaitCutText {
		for _, text := range texts {
			cut := strings.Count(lf[file], text) != 1
			failures["lease_wait_cut:"+file] = failures["lease_wait_cut:"+file] || cut
			failures["lease_wait_cut"] = failures["lease_wait_cut"] || cut // 2A's key, kept
		}
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
	const tree = "\treturn waitOutLease(ctx, label, func(string) (fs.FileInfo, error) { return privateBeneathLstat(t.recovery, t.name(name), t.want) },\n" +
		"\t\tfunc() (*os.File, error) { return privateBeneathOpen(t.recovery, t.name(name), t.want, label) })"
	waited := func(open string) string {
		return "return waitOutLease(ctx, path, os.Lstat, func() (*os.File, error) {\n\t\treturn " + open + "\n\t})"
	}
	const checkpoint, start = "func (c *Cell) Checkpoint(ctx context.Context, options CheckpointOptions) error {\n", "func (c *Cell) Start(ctx context.Context) error {\n"
	const source, ops = "\topen, release, err := c.openCheckpointSource(", "\t\tops.open = func(path string) (*os.File, error) { return openPrivateReader(ctx, path) }\n"
	const begin, inspect, witness = "\tif err := c.begin(context.Background(), \"start\"); err != nil {\n", "\tinspection, err := c.Inspect(ctx)\n", source + "ctx)\n\tif err != nil {\n\t\treturn errCheckpointWitness\n"
	const journal, removal = "\tprogress, err := c.readJournal(ctx)\n\tif err != nil {\n\t\treturn result, err\n\t}\n", "func (c *Cell) finishRemoval(ctx context.Context, m *Manifest, purge bool) error {\n"
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
		{"tree_open_direct", "checkpoint_tree_unix.go", tree, "\t_ = ctx\n\treturn privateBeneathOpen(t.recovery, t.name(name), t.want, label)", "lease_wait_caller:checkpoint_tree_unix.go:open"},
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
		// LC-LEASE-2C2: the detach, the released placeholder and the lockless cut.
		{"detach_dropped", "checkpoint.go", checkpoint + "\tctx = leaseWaitDetached(ctx)\n", checkpoint, "lease_wait_detached"},
		{"detach_elsewhere", "lifecycle.go", start, start + "\tctx = leaseWaitDetached(ctx)\n", "lease_wait_detached"},
		{"released_placeholder", "checkpoint.go", source + "ctx)\n\tif err != nil {\n\t\treturn fail(", source + "context.Background())\n\tif err != nil {\n\t\treturn fail(", "lease_wait_detached"},
		{"released_placeholder_site", "checkpoint.go", source + "ctx)\n\tif err != nil {\n\t\treturn fail(", source + "context.Background())\n\tif err != nil {\n\t\treturn fail(",
			"lease_wait_placeholder:checkpoint.go:(*Cell).copyCheckpoint"},
		{"wait_ignores_detach", "files_other.go", "\t\tcase <-leaseWaitDone(ctx):\n", "\t\tcase <-ctx.Done():\n", "lease_wait_body"},
		{"refuse_cut_deferred", "checkpoint.go", "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn err\n\t} else if err != nil {\n", "\tif err != nil {\n", "lease_wait_cut:checkpoint.go"},
		// LC-LEASE-2C2-F1: the independent review's survivors behind G1, G2 and G5, each by its own text.
		{"inspect_context_reassigned", "lifecycle.go", journal, "\tctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)\n\tdefer cancel()\n" + journal, "lease_wait_reassigned:lifecycle.go:(*Cell).Inspect"},
		{"witness_context_reassigned", "checkpoint.go", witness, "\tctx, sourceCancel := context.WithTimeout(context.Background(), c.cfg.Timeout)\n\tdefer sourceCancel()\n" + witness,
			"lease_wait_reassigned:checkpoint.go:(*Cell).witnessCheckpoint"},
		{"removal_context_reassigned", "lifecycle.go", removal, removal + "\tctx = context.WithoutCancel(ctx)\n", "lease_wait_reassigned:lifecycle.go:(*Cell).finishRemoval"},
		{"live_call_added", "lifecycle.go", begin, "\tif _, err := c.readJournal(ctx); err != nil {\n\t\treturn err\n\t}\n" + begin, "lease_wait_live:lifecycle.go:(*Cell).Start"},
		{"live_closure_added", "lifecycle.go", begin, "\tprobe := func() error { _, err := c.readJournal(ctx); return err }\n\tif err := probe(); err != nil {\n\t\treturn err\n\t}\n" + begin,
			"lease_wait_live:lifecycle.go:(*Cell).Start"},
		{"live_status_added", "lifecycle.go", inspect, inspect + "\t_, _ = c.postmaster(ctx)\n", "lease_wait_live:lifecycle.go:(*Cell).Status"},
		{"placeholder_spoofed", "lifecycle.go", begin + "\t\treturn err\n\t}\n", "\tif err := func() error {\n\t\tcontext := struct{ Background func() context.Context }{func() context.Context { return ctx }}\n" +
			"\t\treturn c.begin(context.Background(), \"start\")\n\t}(); err != nil {\n\t\treturn err\n\t}\n", "lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"released_live_added", "lifecycle.go", "\treturn c.finish(context.Background(), m, \"start\")\n}\n", "\t_, _ = c.Inspect(ctx)\n\treturn c.finish(context.Background(), m, \"start\")\n}\n",
			"lease_wait_released:lifecycle.go:(*Cell).Start"},
		{"value_in_field", "lifecycle.go", begin, "\treader := struct{ read func(context.Context) (journalProgress, error) }{c.readJournal}\n\tif _, err := reader.read(context.Background()); err != nil {\n\t\treturn err\n\t}\n" + begin,
			"lease_wait_value:lifecycle.go:(*Cell).Start"},
		{"interface_method_call", "lifecycle.go", begin, "\tif _, err := (interface {\n\t\treadJournal(context.Context) (journalProgress, error)\n\t})(c).readJournal(context.Background()); err != nil {\n\t\treturn err\n\t}\n" + begin,
			"lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"function_value", "lifecycle.go", begin, "\tread := c.readJournal\n\tif _, err := read(context.Background()); err != nil {\n\t\treturn err\n\t}\n" + begin, "lease_wait_value:lifecycle.go:(*Cell).Start"},
		{"released_placeholder_caller", "lifecycle.go", inspect, "\tinspection, err := c.Inspect(context.Background())\n", "lease_wait_released:lifecycle.go:(*Cell).Status"},
		{"released_callee_added", "lifecycle.go", begin, "\tif _, err := c.Inspect(context.Background()); err != nil {\n\t\treturn err\n\t}\n" + begin, "lease_wait_released:lifecycle.go:(*Cell).Start"},
		// LC-LEASE-2D: the readers' cuts, the released ops.open and the seam's context; the seam by its variable,
		// called with a placeholder or its own context and used as a value, and its literal's context reassigned.
		{"credential_cut_refused", "installer_principal_credentials.go", "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn \"\", cut\n\t} else if err != nil {\n", "\tif err != nil {\n",
			"lease_wait_cut:installer_principal_credentials.go"},
		{"reinstall_cut_unsupported", "reinstall.go", "\tif cut := (*leaseWaitCut)(nil); errors.As(err, &cut) {\n\t\treturn cut\n\t} else if err != nil {\n", "\tif err != nil {\n", "lease_wait_cut:reinstall.go"},
		{"released_ops_placeholder", "checkpoint.go", ops, strings.Replace(ops, "(ctx,", "(context.Background(),", 1), "lease_wait_detached"},
		{"released_ops_placeholder_site", "checkpoint.go", ops, strings.Replace(ops, "(ctx,", "(context.Background(),", 1), "lease_wait_placeholder:checkpoint.go:(*Cell).checkpointOps"},
		{"seam_context_dropped", "installer_principal_credentials.go", "\treturn openPrivateReader(ctx, path)\n}\n", "\treturn openPrivateReader(context.Background(), path)\n}\n",
			"lease_wait_placeholder:installer_principal_credentials.go:openPrincipalCredentialFile"},
		{"seam_placeholder_added", "lifecycle.go", begin, "\tif f, err := openPrincipalCredentialFile(context.Background(), c.cfg.Root, os.O_RDONLY, 0); err == nil {\n\t\t_ = f.Close()\n\t}\n" + begin,
			"lease_wait_placeholder:lifecycle.go:(*Cell).Start"},
		{"seam_live_added", "lifecycle.go", begin, "\tif f, err := openPrincipalCredentialFile(ctx, c.cfg.Root, os.O_RDONLY, 0); err == nil {\n\t\t_ = f.Close()\n\t}\n" + begin,
			"lease_wait_live:lifecycle.go:(*Cell).Start"},
		{"seam_value", "lifecycle.go", begin, "\topen := openPrincipalCredentialFile\n\tif f, err := open(context.Background(), c.cfg.Root, os.O_RDONLY, 0); err == nil {\n\t\t_ = f.Close()\n\t}\n" + begin,
			"lease_wait_value:lifecycle.go:(*Cell).Start"},
		{"seam_context_reassigned", "installer_principal_credentials.go", "\treturn openPrivateReader(ctx, path)\n}\n", "\tctx = context.WithoutCancel(ctx)\n\treturn openPrivateReader(ctx, path)\n}\n",
			"lease_wait_reassigned:installer_principal_credentials.go:openPrincipalCredentialFile"},
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

// leaseHolderRules are readers6_linux_test.go's holder as gofmt prints it, comments aside
// (FLAKE-64): take hands hold the kind, the opener's first pause and the time the lease was taken; a
// retaking holder polls by nanosleep about every first/50 with a 1 µs timer slack on a thread locked
// for good, and lets go only within first/2 of last seeing its lease unbroken, then takes it again by
// raw fcntl calls, a refused re-take retried for at most first/4.
var leaseHolderRules = map[string]string{"take": "\tgo l.hold(f, l.retake, leaseWait.first, time.Now())\n", "hold": `func (l *writerLease) hold(f *os.File, retaking bool, first time.Duration, fresh time.Time) {
	nap, lost := func() { time.Sleep(100 * time.Microsecond) }, false
	if retaking {
		runtime.LockOSThread()
		_ = unix.Prctl(unix.PR_SET_TIMERSLACK, uintptr(time.Microsecond), 0, 0, 0)
		pause := unix.NsecToTimespec(int64(first / 50))
		nap = func() { _ = unix.Nanosleep(&pause, nil) }
	}
	for open := true; open; l.mu.Unlock() {
		nap()
		l.mu.Lock()
		polled := time.Now()
		if held, err := unix.FcntlInt(f.Fd(), unix.F_GETLEASE, 0); l.file == f && err == nil && held != l.lease {
			l.saw = true
			if l.letGo {
				_, _ = unix.FcntlInt(f.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
			}
			if l.retake {
				fresh, lost = l.retaken(f.Fd(), fresh, lost, first)
			}
		} else if err == nil {
			fresh = polled
		}
		open = l.file == f
	}
}`, "retaken": `func (l *writerLease) retaken(fd uintptr, fresh time.Time, lost bool, first time.Duration) (time.Time, bool) {
	if !lost && time.Since(fresh) > first/2 {
		return fresh, false
	}
	_, _, _ = unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, unix.F_UNLCK)
	_, _, errno := unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, uintptr(l.lease))
	for end := time.Now().Add(first / 4); errno == unix.EAGAIN && time.Now().Before(end); {
		_, _, errno = unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, uintptr(l.lease))
	}
	return time.Now(), errno != 0
}`}

// leaseHolderOld is the holder before FLAKE-64 from take's go statement to do: its time.Sleep lasted
// about the opener's first pause, and it let go and took the lease again through the scheduler.
const leaseHolderOld = `go l.hold(f)
	} else {
		_ = f.Close()
	}
}

func (l *writerLease) hold(f *os.File) {
	for open := true; open; l.mu.Unlock() {
		time.Sleep(100 * time.Microsecond)
		l.mu.Lock()
		if held, err := unix.FcntlInt(f.Fd(), unix.F_GETLEASE, 0); l.file == f && err == nil && held != l.lease {
			l.saw = true
			if l.letGo || l.retake {
				_, _ = unix.FcntlInt(f.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
			}
			if l.retake {
				_, _ = unix.FcntlInt(f.Fd(), unix.F_SETLEASE, l.lease)
			}
		}
		open = l.file == f
	}
}

`

// leaseHolderContract returns the key of each holder rule source breaks.
func leaseHolderContract(source string) (failures []string) {
	fset, printed := token.NewFileSet(), map[string]string{}
	file, err := parser.ParseFile(fset, "readers6_linux_test.go", strings.ReplaceAll(source, "\r\n", "\n"), parser.SkipObjectResolution)
	if err != nil {
		return []string{"parse:readers6_linux_test.go"}
	}
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Recv != nil {
			printed[function.Name.Name] = leaseWaitPrint(fset, function)
		}
	}
	for name, rule := range leaseHolderRules {
		if !strings.Contains(printed[name], rule) {
			failures = append(failures, "lease_holder:"+name)
		}
	}
	slices.Sort(failures)
	return failures
}

// The lease holder's rules (FLAKE-64), with LF and CRLF endings: the source keeps them, and each
// mutation, the old holder among them, reports its rule's key.
func TestLeaseHolderSourceContract(t *testing.T) {
	data, err := os.ReadFile("readers6_linux_test.go")
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	from, to := strings.Index(source, "go l.hold(f, l.retake"), strings.Index(source, "func (l *writerLease) do(")
	if err != nil || from < 0 || to < from {
		t.Fatal("cannot read the lease holder from its source")
	}
	once := func(before, after string) string {
		if strings.Count(source, before) != 1 {
			return ""
		}
		return strings.Replace(source, before, after, 1)
	}
	for _, row := range []struct{ name, mutated, want string }{
		{"result", source, ""},
		{"old_holder", once(source[from:to], leaseHolderOld), "lease_holder:hold"},
		{"old_poll", once("nap = func() { _ = unix.Nanosleep(&pause, nil) }", "nap = func() { _ = pause; time.Sleep(100 * time.Microsecond) }"), "lease_holder:hold"},
		{"scheduler_retake", once("_, _, _ = unix.RawSyscall(unix.SYS_FCNTL, fd, unix.F_SETLEASE, unix.F_UNLCK)", "_, _ = unix.FcntlInt(fd, unix.F_SETLEASE, unix.F_UNLCK)"), "lease_holder:retaken"},
		{"retry_removed", once("errno == unix.EAGAIN && time.Now().Before(end)", "errno == unix.EAGAIN && end.IsZero()"), "lease_holder:retaken"},
		{"late_release", once("!lost && time.Since(fresh) > first/2", "false"), "lease_holder:retaken"},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, ending := range []string{"\n", "\r\n"} {
				failures := leaseHolderContract(strings.ReplaceAll(row.mutated, "\n", ending))
				if row.mutated == "" || row.want == "" && len(failures) != 0 || row.want != "" && !slices.Contains(failures, row.want) {
					t.Fatalf("lease holder source contract reported keys %v, the row wants %q", failures, row.want)
				}
			}
		})
	}
}
