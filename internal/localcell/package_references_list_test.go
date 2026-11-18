package localcell

import "strings"

// packageReferencesGuarded is the closed guarded set, by declaration. Openers
// open or read a name and can wait on or follow what stands there, or return a
// handle no check has seen; escape hatches reach code by value, name or
// address. Each is refused wherever packageReferencesList does not list it.
var packageReferencesGuarded = strings.Fields(`
os.Open os.OpenFile os.Create os.CreateTemp os.ReadFile os.WriteFile os.ReadDir os.DirFS os.OpenRoot os.OpenInRoot os.NewFile os.MkdirTemp
(*os.Root).Open (*os.Root).OpenFile (*os.Root).Create (*os.Root).ReadFile (*os.Root).WriteFile (*os.Root).OpenRoot (*os.Root).FS
io/ioutil.ReadFile io/ioutil.WriteFile io/ioutil.ReadDir io/ioutil.TempFile io/ioutil.TempDir
io/fs.ReadFile io/fs.ReadDir io/fs.WalkDir io/fs.Glob io/fs.Sub io/fs.Stat io/fs.Lstat io/fs.ReadLink
path/filepath.Walk path/filepath.WalkDir path/filepath.Glob path/filepath.EvalSymlinks
syscall.Open syscall.Openat syscall.Creat syscall.CreateFile
golang.org/x/sys/unix.Open golang.org/x/sys/unix.Openat golang.org/x/sys/unix.Openat2 golang.org/x/sys/unix.Creat
golang.org/x/sys/windows.CreateFile golang.org/x/sys/windows.Open
net/http.Dir net/http.ServeFile net/http.FileServer net/http.FS net/http.ServeFileFS net/http.FileServerFS
github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateOpenFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRead
github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateOpenRoot github.com/witwave-ai/witself/internal/runtimeconfig.OpenSpoolRoot
openPrivateFile openPrivateReader openPrivateWriter openPlainReader openPostgresReader readPrivateFile readPostgresFile createPrivateTemp
openLockFile openRegular openRegularBy openRegularWriter readRegular openServiceRecord readWindowsTaskRecord readJSON readMaintenanceFenceJSON
syncDirectory (*Cell).openCheckpointSource (*Cell).holdCheckpoint
(reflect.Value).Call (reflect.Value).CallSlice (reflect.Value).Method (reflect.Value).MethodByName (reflect.Type).Method (reflect.Type).MethodByName
unsafe.Pointer unsafe.Add unsafe.Slice unsafe.String unsafe.StringData unsafe.SliceData
golang.org/x/sys/windows.NewLazySystemDLL golang.org/x/sys/windows.NewLazyDLL golang.org/x/sys/windows.LoadDLL golang.org/x/sys/windows.MustLoadDLL
(*golang.org/x/sys/windows.LazyDLL).NewProc (*golang.org/x/sys/windows.DLL).FindProc (*golang.org/x/sys/windows.LazyProc).Call (*golang.org/x/sys/windows.Proc).Call
syscall.Syscall syscall.Syscall6 syscall.Syscall9 syscall.Syscall12 syscall.Syscall15 syscall.SyscallN syscall.RawSyscall syscall.RawSyscall6
golang.org/x/sys/unix.Syscall golang.org/x/sys/unix.Syscall6 golang.org/x/sys/unix.RawSyscall golang.org/x/sys/unix.RawSyscall6 golang.org/x/sys/unix.SyscallNoError golang.org/x/sys/unix.RawSyscallNoError
`)

// packageReferencesList is every non-test file of the package, then each of
// its declarations that references a guarded object, with the count.
var packageReferencesList = `
checkpoint.go
	(*Cell).admitCheckpoint openPostgresReader readRegular
	(*Cell).admitDeleteCheckpoint os.ReadDir
	(*Cell).checkpointOps openPrivateReader x3
	(*Cell).copyCheckpoint (*Cell).openCheckpointSource createPrivateTemp syncDirectory x2
	(*Cell).deleteCheckpointLocked (*Cell).holdCheckpoint
	(*Cell).inventoryCheckpoint openPrivateReader path/filepath.WalkDir
	(*Cell).listCheckpoints os.ReadDir
	(*Cell).openCheckpointEntry openPrivateReader x2
	(*Cell).planCheckpoint path/filepath.WalkDir
	(*Cell).validateRecovery os.ReadDir path/filepath.WalkDir
	(*Cell).witnessCheckpoint (*Cell).openCheckpointSource
	checkpointComplete readJSON
	syncCheckpointDirectory syncDirectory
checkpoint_other.go
checkpoint_root.go
	(*Cell).discardOwnCheckpoint (*Cell).holdCheckpoint
	(*Cell).holdCheckpoint (*os.Root).OpenRoot os.OpenRoot
	(*Cell).removeCheckpointHeld (*os.Root).Open
	syncRootDirectory (*os.Root).Open
checkpoint_unix.go
	(*Cell).openCheckpointSource (*os.Root).OpenFile os.OpenRoot
checkpoint_verify.go
	checkpointVerifyRead openPrivateReader
	checkpointVerifyTree path/filepath.WalkDir
checkpoint_windows.go
	(*Cell).openCheckpointSource github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
commands.go
	(*Cell).logFile openRegularWriter
	(*Cell).password openPrivateReader readRegular
doc.go
drill_record.go
drill_root.go
	(*Cell).deleteDrillLocked (*os.Root).OpenRoot os.OpenRoot
	(*Cell).drillRootList os.ReadDir
	drillRootClassify openPrivateFile os.ReadDir
	drillRootHeld (*os.Root).OpenFile
	drillRootNames (*os.Root).Open
	drillRootSync (*os.Root).Open
environment_unix.go
environment_windows.go
files.go
	(*Cell).discardJournalTail openRegularWriter
	(*Cell).journalCause openRegularWriter syncDirectory
	(*Cell).readJournal openRegular
	(*Cell).readUpdateJournal openRegular
	(*Cell).validateRoot path/filepath.EvalSymlinks
	openRegular openPrivateReader openRegularBy
	openRegularWriter openPrivateWriter
	privateDir syncDirectory
	readJSON openRegular
	readManifest readJSON
	readRegular openRegularBy
	syncDirectory os.Open
	syncJournalDirectory syncDirectory
	syncPrivateDirParent syncDirectory
	writeAtomicBytesOnce createPrivateTemp syncDirectory
files_other.go
	createPrivateTemp os.CreateTemp
	openPlainReader openPrivateReader
	openPostgresReader openPrivateReader
	openPrivateFile os.OpenFile
	openPrivateReader os.OpenFile
	openPrivateWriter os.OpenFile
	readPostgresFile readPrivateFile
	readPrivateFile os.ReadFile
files_windows.go
	createPrivateTemp github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
	openPlainReader os.Open
	openPrivateFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateOpenFile
	openPrivateReader openPrivateFile
	openPrivateWriter openPrivateFile
	readPrivateFile openPrivateFile
	validatePrivateTree path/filepath.WalkDir
fixed_cause.go
installation_identity.go
installer_principal_credentials.go
	(*Cell).makePrincipalPasswords os.ReadDir
	(*Cell).validatePrincipalPasswords os.ReadDir
	openPrincipalCredentialFile openPrivateReader
installer_principals.go
launch_unix.go
launch_windows.go
	(*Cell).launchWindows os.Open unsafe.Pointer x4
lifecycle.go
	(*Cell).Install os.ReadDir x2
	(*Cell).finishRemoval syncDirectory
	(*Cell).postmaster openPostgresReader readRegular
	(*Cell).processStateWithRecord readJSON
	(*Cell).recoverPostgresRecord openPostgresReader readRegular
lock.go
	acquireLock openLockFile x2
lock_contention.go
	classifyOwnershipFailure openLockFile
lock_unix.go
	openLockFile os.NewFile syscall.Open
lock_unsupported.go
lock_windows.go
	openLockFile openPrivateFile
port_recovery.go
ports.go
postgres_files_windows.go
	(*Cell).writePostgresConfig github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
	openPostgresReader github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
	readPostgresFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRead
postgres_pending.go
	(*Cell).sweepPostgresPending os.ReadDir x3
postgres_token_other.go
postgres_token_windows.go
	createRestrictedTokenProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsPostgresToken (*golang.org/x/sys/windows.LazyProc).Call unsafe.Pointer x4
postgres_unix.go
postgres_windows.go
	(*Cell).pendingPostgresWithoutPID readJSON
	(*Cell).pendingPostmasterPID readJSON
	(*Cell).windowsPostgresReady readPostgresFile
	(*Cell).windowsStalePostmaster readPostgresFile
private_tree_boundary.go
private_tree_snapshot.go
process.go
process_darwin.go
process_linux.go
	linuxProcessStartTicks os.ReadFile
	processIdentity os.ReadFile
	processStartedAt os.ReadFile x2
process_unix.go
process_unsupported.go
process_windows.go
	windowsValidateProcessExecutable golang.org/x/sys/windows.CreateFile
readiness.go
	(*Cell).lastValueFreeDiagnostic openPrivateReader
reinstall.go
	(*Cell).reinstallAvailable openPostgresReader readRegular
removal_retention.go
	(*Cell).finishRetainedRemoval openRegularWriter syncDirectory
rollback_policy.go
service.go
service_adapter_admission.go
	serviceAdapterCanonicalPath path/filepath.EvalSymlinks
	serviceAdapterRemovalHostLock openLockFile
service_boot.go
	(*Cell).bindServiceProcessBootWithReader readJSON
	(*Cell).reconcileOldBoot readJSON syncDirectory x2
	(*Cell).reconcileServiceBoot readJSON
service_boot_darwin.go
service_boot_linux.go
	serviceBootID os.ReadFile
service_boot_unsupported.go
service_boot_windows.go
	queryWindowsBootEnvironment (*golang.org/x/sys/windows.LazyProc).Call unsafe.Pointer x2
	windowsNtQuerySystemInformation (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
service_control.go
service_control_endpoint.go
	(*Cell).publishServiceControlEndpointWithBoot readJSON
	(*Cell).readServiceControlEndpointRecord openServiceRecord
	(*Cell).readServiceControlEndpointWithBoot readJSON
service_control_exit.go
	(*Cell).exitServiceHostForRemoval readJSON
	(*Cell).serviceHostExitIdentity readJSON
	(*Cell).serviceHostLeaseHeld openLockFile
	(*Cell).waitServiceHostExit readJSON x2
service_control_exit_budget.go
service_control_exit_unix.go
service_control_exit_windows.go
service_control_unix.go
service_control_unsupported.go
service_control_windows.go
	windowsDialPipe golang.org/x/sys/windows.CreateFile
	windowsPipeValidateDescriptor unsafe.Pointer
service_descriptor.go
	(*Cell).readServiceDescriptorForOwner readJSON
	(*Cell).serviceControlSecret openServiceRecord
	(*Cell).writeServiceDescriptor syncDirectory
	openServiceRecord openPrivateReader openRegularBy
	validateServiceHost path/filepath.EvalSymlinks x2
service_failure_site.go
service_foreground.go
	(*Cell).acquireServiceLeaseWithBoot openLockFile
service_health.go
	(*Cell).serviceAPICredential readJSON
service_launchd.go
	(*launchdAgentAdapter).Apply os.OpenFile
	(*launchdAgentAdapter).ownedPlist openPlainReader
	(*launchdAgentAdapter).sync syncDirectory
service_launchd_darwin.go
service_lease_publish.go
service_maintenance.go
service_shutdown.go
service_shutdown_descriptor.go
service_shutdown_other.go
service_shutdown_policy.go
service_shutdown_protocol.go
service_shutdown_supervisor.go
service_shutdown_windows.go
	(*Cell).stopChildOutcomeWithBudget readJSON
	readShutdownDescriptor openPrivateFile
	waitShutdownReady readJSON
service_supervisor.go
	(*supervisor).load readJSON
	(*supervisor).rememberProcesses readJSON x2
service_systemd.go
service_systemd_adapter.go
	(*systemdUserAdapter).Apply os.OpenFile syncDirectory
	(*systemdUserAdapter).Remove syncDirectory
	(*systemdUserAdapter).ownedUnit openPlainReader
	(*systemdUserAdapter).removeCreatedUnit syncDirectory
	systemdUserDirectories path/filepath.EvalSymlinks
service_systemd_errors.go
service_systemd_linux.go
service_task_host.go
service_unsupported.go
service_windows_task.go
	(*windowsLogonTaskAdapter).Remove syncDirectory
	(*windowsLogonTaskAdapter).records readWindowsTaskRecord x2
	createWindowsTaskRecord openPrivateFile syncDirectory
	readWindowsTaskRecord openPrivateFile
service_windows_task_capture.go
service_windows_task_identity.go
service_windows_task_native_other.go
service_windows_task_native_windows.go
	windowsTaskTokenUint32 unsafe.Pointer
service_windows_task_xml.go
stop_unix.go
stop_windows.go
	(*Cell).windowsStopRecord readJSON
	attachConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	freeConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsConsoleBreak (*golang.org/x/sys/windows.LazyProc).Call x2
types.go
	CanonicalRoot path/filepath.EvalSymlinks
update_fence.go
	(*Cell).readMaintenanceFence readMaintenanceFenceJSON
	readMaintenanceFenceJSON openRegular
update_record.go
	(*Cell).appendUpdateJournalEntry openPrivateFile syncDirectory
	(*Cell).readDerivedUpdateRecord openServiceRecord
	(*Cell).readUpdateIntent readJSON
	openUpdateJournalFile openPrivateFile
	syncUpdateJournalDirectory syncDirectory
`
