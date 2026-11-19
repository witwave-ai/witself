package localcell

import "strings"

// packageReferencesGuarded is the closed guarded set, by declaration. Openers
// open or read a name and can wait on or follow what stands there, or return a
// handle no check has seen; escape hatches reach code by value, name or
// address. Each is refused wherever packageReferencesList does not list it. A
// package no file imports needs no name here: its import is refused.
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
net.Dial net.DialTimeout net.DialUnix (*net.Dialer).Dial (*net.Dialer).DialContext net.Listen net.ListenUnix net.ListenUnixgram net.ListenPacket (*net.ListenConfig).Listen
net/http.ListenAndServeTLS net/http.ServeTLS (*net/http.Server).ListenAndServeTLS (*net/http.Server).ServeTLS
golang.org/x/sys/unix.OpenTree golang.org/x/sys/unix.NameToHandleAt golang.org/x/sys/unix.OpenByHandleAt golang.org/x/sys/unix.Syscall9
golang.org/x/sys/windows.NtCreateFile golang.org/x/sys/windows.NtCreateNamedPipeFile golang.org/x/sys/windows.FindFirstFile golang.org/x/sys/windows.CreateNamedPipe golang.org/x/sys/windows.FindFirstChangeNotification
golang.org/x/sys/windows.LoadLibrary golang.org/x/sys/windows.LoadLibraryEx golang.org/x/sys/windows.GetProcAddress (*golang.org/x/sys/windows.DLL).MustFindProc
syscall.FindFirstFile syscall.Syscall18 syscall.AllThreadsSyscall syscall.AllThreadsSyscall6 syscall.NewLazyDLL syscall.LoadDLL syscall.MustLoadDLL syscall.LoadLibrary syscall.GetProcAddress
(*syscall.LazyDLL).NewProc (*syscall.DLL).FindProc (*syscall.DLL).MustFindProc (*syscall.LazyProc).Call (*syscall.Proc).Call
reflect.NewAt reflect.SliceAt (reflect.Value).UnsafePointer (reflect.Value).UnsafeAddr
`)

// packageReferencesList is every non-test file of the package, then its import
// paths on one "import" line, then each of its declarations that references a
// guarded object, with the count.
var packageReferencesList = `
checkpoint.go
	import bytes context crypto/sha256 encoding/hex encoding/json errors github.com/witwave-ai/witself/internal/id github.com/witwave-ai/witself/internal/runtimeconfig io io/fs math os os/exec path path/filepath regexp runtime slices sort strings sync time unicode/utf16 unicode/utf8
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
	checkpointComplete openRegular
	syncCheckpointDirectory syncDirectory
checkpoint_other.go
	import os
checkpoint_root.go
	import errors io/fs os path/filepath runtime
	(*Cell).discardOwnCheckpoint (*Cell).holdCheckpoint
	(*Cell).holdCheckpoint (*os.Root).OpenRoot os.OpenRoot
	(*Cell).removeCheckpointHeld (*os.Root).Open
	syncRootDirectory (*os.Root).Open
checkpoint_unix.go
	import errors io/fs os path/filepath reflect syscall
	(*Cell).openCheckpointSource (*os.Root).OpenFile os.OpenRoot
checkpoint_verify.go
	import bytes context crypto/sha256 encoding/hex encoding/json io io/fs math os path/filepath reflect strings time unicode/utf8
	checkpointVerifyRead openPrivateReader
	checkpointVerifyTree path/filepath.WalkDir
checkpoint_windows.go
	import github.com/witwave-ai/witself/internal/runtimeconfig golang.org/x/sys/windows os path/filepath
	(*Cell).openCheckpointSource github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
commands.go
	import bytes context crypto/rand encoding/base64 errors fmt io net net/http net/url os os/exec path/filepath regexp runtime strconv strings time
	(*Cell).logFile openRegularWriter
	(*Cell).password openPrivateReader readRegular
doc.go
drill_record.go
	import bytes encoding/json errors io strings
drill_reserve.go
	import errors io/fs
drill_reserve_linux.go
	import errors golang.org/x/sys/unix os strings unsafe
	drillPlatformAt (*os.Root).Open golang.org/x/sys/unix.Openat
	drillPlatformCreate golang.org/x/sys/unix.Openat os.NewFile
	drillPlatformVerify golang.org/x/sys/unix.Syscall unsafe.Pointer
drill_reserve_other.go
	import os
drill_root.go
	import errors io os path/filepath runtime strings syscall
	(*Cell).deleteDrillLocked (*os.Root).Open x2 (*os.Root).OpenRoot os.OpenRoot
	(*Cell).drillRootList os.ReadDir
	drillRootClassify openPrivateFile os.ReadDir
	drillRootHeld (*os.Root).OpenFile
	drillRootNames (*os.Root).Open
	drillRootSync (*os.Root).Open
drill_storage.go
	import context errors math os strings
	(*Cell).admitDrillStorageLocked (*os.Root).Open os.OpenRoot
drill_storage_darwin.go
	import golang.org/x/sys/unix os
drill_storage_linux.go
	import golang.org/x/sys/unix os
drill_storage_other.go
	import os
drill_storage_windows.go
	import golang.org/x/sys/windows os unsafe
	drillPlatformVolume (*golang.org/x/sys/windows.LazyDLL).NewProc (*golang.org/x/sys/windows.LazyProc).Call x2 golang.org/x/sys/windows.NewLazySystemDLL unsafe.Pointer x4
environment_unix.go
environment_windows.go
	import errors golang.org/x/sys/windows path/filepath strings
files.go
	import bufio bytes encoding/json errors fmt github.com/witwave-ai/witself/internal/runtimeconfig io os path/filepath runtime strings time
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
	import errors os runtime syscall time
	createPrivateTemp os.CreateTemp
	openPlainReader openPrivateReader
	openPostgresReader openPrivateReader
	openPrivateFile os.OpenFile
	openPrivateReader os.OpenFile
	openPrivateWriter os.OpenFile
	readPostgresFile readPrivateFile
	readPrivateFile os.ReadFile
files_windows.go
	import errors github.com/witwave-ai/witself/internal/runtimeconfig io os path/filepath
	createPrivateTemp github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
	openPlainReader os.Open
	openPrivateFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateOpenFile
	openPrivateReader openPrivateFile
	openPrivateWriter openPrivateFile
	readPrivateFile openPrivateFile
	validatePrivateTree path/filepath.WalkDir
fixed_cause.go
installation_identity.go
	import github.com/witwave-ai/witself/internal/id strings
installer_principal_credentials.go
	import crypto/rand encoding/base64 errors github.com/witwave-ai/witself/internal/store io net/url os reflect runtime strings
	(*Cell).credentialPassword openPrivateReader
	(*Cell).makePrincipalPasswords os.ReadDir
	(*Cell).validatePrincipalPasswords os.ReadDir
	openPrincipalCredentialFile openPrivateReader
installer_principals.go
	import context github.com/jackc/pgx/v5 github.com/jackc/pgx/v5/pgconn github.com/witwave-ai/witself/internal/runtimeconfig github.com/witwave-ai/witself/internal/store io slices strings time
launch_unix.go
	import context errors io os os/exec syscall time
launch_windows.go
	import context errors golang.org/x/sys/windows os path/filepath runtime slices strings unicode/utf16 unsafe
	(*Cell).launchWindows os.Open unsafe.Pointer x4
lifecycle.go
	import context errors os path/filepath runtime strconv time
	(*Cell).Install os.ReadDir x2
	(*Cell).finishRemoval syncDirectory
	(*Cell).postmaster openPostgresReader readRegular
	(*Cell).processStateWithRecord readJSON
	(*Cell).recoverPostgresRecord openPostgresReader readRegular
lock.go
	import errors fmt os path/filepath
	acquireLock openLockFile x2
lock_contention.go
	import errors os path/filepath runtime
	classifyOwnershipFailure openLockFile
lock_unix.go
	import errors os syscall
	openLockFile os.NewFile syscall.Open
lock_unsupported.go
	import errors os
lock_windows.go
	import errors golang.org/x/sys/windows os
	openLockFile openPrivateFile
port_recovery.go
	import context errors time
ports.go
	import net
	allocatePorts net.Listen
	probePorts net.Listen
postgres_files_windows.go
	import errors github.com/witwave-ai/witself/internal/runtimeconfig os path/filepath
	(*Cell).writePostgresConfig github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
	openPostgresReader github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
	readPostgresFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRead
postgres_pending.go
	import os path/filepath
	(*Cell).sweepPostgresPending os.ReadDir x3
postgres_token_other.go
	import os/exec
postgres_token_windows.go
	import errors golang.org/x/sys/windows os/exec path/filepath runtime strings syscall unsafe
	createRestrictedTokenProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsPostgresToken (*golang.org/x/sys/windows.LazyProc).Call unsafe.Pointer x4
postgres_unix.go
	import context errors strconv strings
postgres_windows.go
	import bytes context errors os path/filepath strconv strings time
	(*Cell).pendingPostgresWithoutPID readJSON
	(*Cell).pendingPostmasterPID readJSON
	(*Cell).windowsPostgresReady readPostgresFile
	(*Cell).windowsStalePostmaster readPostgresFile
private_tree_boundary.go
	import io/fs path/filepath strings
private_tree_snapshot.go
	import errors io/fs os path/filepath
process.go
process_darwin.go
	import errors fmt golang.org/x/sys/unix time
process_linux.go
	import encoding/binary errors os strconv strings syscall time
	linuxProcessStartTicks os.ReadFile
	processIdentity os.ReadFile
	processStartedAt os.ReadFile x2
process_unix.go
	import errors os os/exec syscall
process_unsupported.go
	import errors os os/exec time
process_windows.go
	import errors fmt golang.org/x/sys/windows os os/exec path/filepath syscall time
	windowsValidateProcessExecutable golang.org/x/sys/windows.CreateFile
readiness.go
	import context errors fmt io os strings time
	(*Cell).lastValueFreeDiagnostic openPrivateReader
reinstall.go
	import context errors github.com/witwave-ai/witself/internal/store os path/filepath strconv strings
	(*Cell).reinstallAvailable openPostgresReader readRegular
removal_retention.go
	import errors os time
	(*Cell).finishRetainedRemoval openRegularWriter syncDirectory
rollback_policy.go
service.go
	import context errors
service_adapter_admission.go
	import errors os path/filepath reflect runtime strings time
	serviceAdapterCanonicalPath path/filepath.EvalSymlinks
	serviceAdapterRemovalHostLock openLockFile
service_boot.go
	import context errors os strings
	(*Cell).bindServiceProcessBootWithReader readJSON
	(*Cell).reconcileOldBoot readJSON syncDirectory x2
	(*Cell).reconcileServiceBoot readJSON
service_boot_darwin.go
	import errors fmt golang.org/x/sys/unix
service_boot_linux.go
	import errors os strings
	serviceBootID os.ReadFile
service_boot_unsupported.go
service_boot_windows.go
	import encoding/binary errors fmt golang.org/x/sys/windows runtime unsafe
	queryWindowsBootEnvironment (*golang.org/x/sys/windows.LazyProc).Call unsafe.Pointer x2
	windowsNtQuerySystemInformation (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
service_control.go
	import context crypto/subtle encoding/json errors fmt io net net/http os runtime strconv sync time
service_control_endpoint.go
	import crypto/rand encoding/base32 encoding/json io os strings
	(*Cell).publishServiceControlEndpointWithBoot readJSON
	(*Cell).readServiceControlEndpointRecord openServiceRecord
	(*Cell).readServiceControlEndpointWithBoot readJSON
service_control_exit.go
	import bytes context crypto/rand encoding/base32 encoding/json errors fmt io net net/http os runtime strconv strings sync syscall time
	(*Cell).exitServiceHostForRemoval readJSON
	(*Cell).serviceHostExitIdentity readJSON
	(*Cell).serviceHostLeaseHeld openLockFile
	(*Cell).waitServiceHostExit readJSON x2
service_control_exit_budget.go
	import context time
service_control_exit_unix.go
service_control_exit_windows.go
	import golang.org/x/sys/windows
service_control_unix.go
	import context errors net os syscall
	(*Cell).dialServiceControl (*net.Dialer).DialContext
	(*Cell).listenServiceControl net.ListenUnix
service_control_unsupported.go
	import context net
service_control_windows.go
	import context errors fmt golang.org/x/sys/windows io net os runtime sync time unsafe
	windowsCreateNamedPipe golang.org/x/sys/windows.CreateNamedPipe
	windowsDialPipe golang.org/x/sys/windows.CreateFile
	windowsListenPipe golang.org/x/sys/windows.CreateNamedPipe
	windowsPipeValidateDescriptor unsafe.Pointer
service_descriptor.go
	import context crypto/rand encoding/base64 errors io os os/user path/filepath reflect strconv strings
	(*Cell).readServiceDescriptorForOwner readJSON
	(*Cell).serviceControlSecret openServiceRecord
	(*Cell).writeServiceDescriptor syncDirectory
	openServiceRecord openPrivateReader openRegularBy
	validateServiceHost path/filepath.EvalSymlinks x2
service_failure_site.go
	import errors fmt io sync/atomic
service_foreground.go
	import errors os
	(*Cell).acquireServiceLeaseWithBoot openLockFile
service_health.go
	import context crypto/rand encoding/base64 encoding/json errors github.com/witwave-ai/witself/internal/runtimeconfig github.com/witwave-ai/witself/internal/store io net/http net/url os time
	(*Cell).serviceAPICredential readJSON
service_launchd.go
	import bytes context encoding/xml errors fmt io os os/exec path/filepath reflect slices strings time unicode unicode/utf8
	(*launchdAgentAdapter).Apply os.OpenFile
	(*launchdAgentAdapter).ownedPlist openPlainReader
	(*launchdAgentAdapter).sync syncDirectory
service_launchd_darwin.go
	import context golang.org/x/sys/unix os os/exec os/user path/filepath strconv syscall time
service_lease_publish.go
	import fmt github.com/witwave-ai/witself/internal/runtimeconfig io os time
service_maintenance.go
	import context errors slices
service_shutdown.go
	import context errors fmt runtime time
service_shutdown_descriptor.go
	import context crypto/rand crypto/subtle encoding/base64 encoding/json errors io path/filepath strings
service_shutdown_other.go
	import context github.com/witwave-ai/witself/internal/runtimeconfig time
service_shutdown_policy.go
	import context errors time
service_shutdown_protocol.go
	import bufio bytes context encoding/json io sync
service_shutdown_supervisor.go
	import context errors
service_shutdown_windows.go
	import context encoding/json errors fmt github.com/witwave-ai/witself/internal/runtimeconfig golang.org/x/sys/windows os path/filepath strings sync time
	(*Cell).stopChildOutcomeWithBudget readJSON
	readShutdownDescriptor openPrivateFile
	waitShutdownReady readJSON
service_supervisor.go
	import context errors fmt os runtime slices time
	(*supervisor).load readJSON
	(*supervisor).rememberProcesses readJSON x2
service_systemd.go
	import context path/filepath strconv strings unicode
service_systemd_adapter.go
	import bytes context encoding/json errors fmt io os path/filepath reflect strings time
	(*systemdUserAdapter).Apply os.OpenFile syncDirectory
	(*systemdUserAdapter).Remove syncDirectory
	(*systemdUserAdapter).ownedUnit openPlainReader
	(*systemdUserAdapter).removeCreatedUnit syncDirectory
	systemdUserDirectories path/filepath.EvalSymlinks
service_systemd_errors.go
	import errors fmt os os/exec regexp strings unicode unicode/utf8
service_systemd_linux.go
	import context golang.org/x/sys/unix os os/exec os/user path/filepath strconv syscall time
service_task_host.go
service_unsupported.go
service_windows_task.go
	import bytes context encoding/json errors fmt io os path/filepath reflect time
	(*windowsLogonTaskAdapter).Remove syncDirectory
	(*windowsLogonTaskAdapter).records readWindowsTaskRecord x2
	createWindowsTaskRecord openPrivateFile syncDirectory
	readWindowsTaskRecord openPrivateFile
service_windows_task_capture.go
	import sync
service_windows_task_identity.go
	import strings
service_windows_task_native_other.go
	import context
service_windows_task_native_windows.go
	import context errors golang.org/x/sys/windows os os/exec path/filepath syscall time unsafe
	windowsTaskTokenUint32 unsafe.Pointer
service_windows_task_xml.go
	import bytes encoding/binary encoding/xml errors io reflect regexp strings unicode unicode/utf16 unicode/utf8
stop_unix.go
	import context errors strconv
stop_windows.go
	import context errors golang.org/x/sys/windows os os/exec strconv syscall time
	(*Cell).windowsStopRecord readJSON
	attachConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	freeConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsConsoleBreak (*golang.org/x/sys/windows.LazyProc).Call x2
types.go
	import context errors github.com/witwave-ai/witself/internal/runtimeconfig net net/http os path/filepath sync time
	CanonicalRoot path/filepath.EvalSymlinks
update_fence.go
	import bytes encoding/json errors io os time
	(*Cell).readMaintenanceFence readMaintenanceFenceJSON
	readMaintenanceFenceJSON openRegular
update_record.go
	import bytes encoding/hex encoding/json errors io os strings time
	(*Cell).appendUpdateJournalEntry openPrivateFile syncDirectory
	(*Cell).readDerivedUpdateRecord openServiceRecord
	(*Cell).readUpdateIntent readJSON
	openUpdateJournalFile openPrivateFile
	syncUpdateJournalDirectory syncDirectory
`
