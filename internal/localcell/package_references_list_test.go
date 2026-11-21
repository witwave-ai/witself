package localcell

import "strings"

// packageReferencesGuarded is the closed guarded set, by declaration. Openers
// open or read a name and can wait on or follow what stands there, or return a
// handle no check has seen; escape hatches reach code by value, name or
// address; writers and removers create, write, truncate, rename, link, change
// the mode, owner, times or attributes of, or remove a name or what a handle
// refers to, then the package's own that do so, or pass on or build from what
// they are given the name they write, create or remove. Each is refused
// wherever packageReferencesList does not list it, and a guarded method also
// where a value converted to an interface supplies it. A
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
syncDirectory (*Cell).openCheckpointSource (*Cell).holdCheckpoint (*Cell).holdRecovery
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
os.Chmod os.Chown os.Chtimes os.CopyFS os.Lchown os.Link os.Mkdir os.MkdirAll os.Remove os.RemoveAll os.Rename os.Symlink os.Truncate
(*os.File).Chmod (*os.File).Chown (*os.File).ReadFrom (*os.File).Truncate (*os.File).Write (*os.File).WriteAt (*os.File).WriteString (*net.ListenConfig).ListenPacket (*net.UnixListener).SetUnlinkOnClose
(*net.Dialer).DialUnix (*net.UnixListener).Close (*net/http.Request).FormFile (*net/http.Request).FormValue (*net/http.Request).ParseMultipartForm
(*net/http.Request).PostFormValue (*mime/multipart.Form).RemoveAll (*mime/multipart.Reader).ReadForm
(*os.Root).Chmod (*os.Root).Chown (*os.Root).Chtimes (*os.Root).Lchown (*os.Root).Link (*os.Root).Mkdir (*os.Root).MkdirAll (*os.Root).Remove (*os.Root).RemoveAll (*os.Root).Rename (*os.Root).Symlink
syscall.Bind syscall.Chflags syscall.Chmod syscall.Chown syscall.CreateDirectory syscall.CreateFileMapping syscall.CreateHardLink syscall.CreateSymbolicLink syscall.DeleteFile syscall.DeviceIoControl syscall.Exchangedata syscall.Fallocate syscall.Fchflags syscall.Fchmod
syscall.Fchmodat syscall.Fchown syscall.Fchownat syscall.Ftruncate syscall.Futimes syscall.Futimesat syscall.Lchown syscall.Link syscall.MapViewOfFile syscall.Mkdir syscall.Mkdirat syscall.Mkfifo syscall.Mknod syscall.Mknodat syscall.Mmap syscall.Mount syscall.MoveFile
syscall.PivotRoot syscall.Pwrite syscall.RemoveDirectory syscall.Removexattr syscall.Rename syscall.Renameat syscall.Rmdir syscall.Sendfile syscall.SetEndOfFile syscall.SetFileAttributes syscall.SetFileTime syscall.Setxattr syscall.Splice syscall.Symlink syscall.Tee
syscall.Truncate syscall.Undelete syscall.Unlink syscall.Unlinkat syscall.Unmount syscall.Utime syscall.Utimes syscall.UtimesNano syscall.Write syscall.WriteFile
syscall.Acct syscall.CertOpenStore syscall.Madvise
golang.org/x/sys/unix.Bind golang.org/x/sys/unix.Chflags golang.org/x/sys/unix.Chmod golang.org/x/sys/unix.Chown golang.org/x/sys/unix.Clonefile golang.org/x/sys/unix.Clonefileat golang.org/x/sys/unix.CopyFileRange golang.org/x/sys/unix.Exchangedata
golang.org/x/sys/unix.Fallocate golang.org/x/sys/unix.Fchflags golang.org/x/sys/unix.Fchmod golang.org/x/sys/unix.Fchmodat golang.org/x/sys/unix.Fchown golang.org/x/sys/unix.Fchownat golang.org/x/sys/unix.Fclonefileat golang.org/x/sys/unix.Fremovexattr
golang.org/x/sys/unix.Fsetxattr golang.org/x/sys/unix.Ftruncate golang.org/x/sys/unix.Futimes golang.org/x/sys/unix.Futimesat golang.org/x/sys/unix.IoctlFileClone golang.org/x/sys/unix.IoctlFileCloneRange golang.org/x/sys/unix.Lchown golang.org/x/sys/unix.Link
golang.org/x/sys/unix.Linkat golang.org/x/sys/unix.Lremovexattr golang.org/x/sys/unix.Lsetxattr golang.org/x/sys/unix.Lutimes golang.org/x/sys/unix.Mkdir golang.org/x/sys/unix.Mkdirat golang.org/x/sys/unix.Mkfifo golang.org/x/sys/unix.Mkfifoat
golang.org/x/sys/unix.Mknod golang.org/x/sys/unix.Mknodat golang.org/x/sys/unix.Mmap golang.org/x/sys/unix.MmapPtr golang.org/x/sys/unix.Mount golang.org/x/sys/unix.MountSetattr golang.org/x/sys/unix.MoveMount golang.org/x/sys/unix.PivotRoot golang.org/x/sys/unix.Pwrite
golang.org/x/sys/unix.Pwritev golang.org/x/sys/unix.Pwritev2 golang.org/x/sys/unix.Removexattr golang.org/x/sys/unix.Rename golang.org/x/sys/unix.Renameat golang.org/x/sys/unix.Renameat2 golang.org/x/sys/unix.RenameatxNp golang.org/x/sys/unix.RenamexNp
golang.org/x/sys/unix.Rmdir golang.org/x/sys/unix.Sendfile golang.org/x/sys/unix.Setattrlist golang.org/x/sys/unix.Setxattr golang.org/x/sys/unix.Splice golang.org/x/sys/unix.Symlink golang.org/x/sys/unix.Symlinkat golang.org/x/sys/unix.Tee
golang.org/x/sys/unix.Truncate golang.org/x/sys/unix.Undelete golang.org/x/sys/unix.Unlink golang.org/x/sys/unix.Unlinkat golang.org/x/sys/unix.Unmount golang.org/x/sys/unix.Utime golang.org/x/sys/unix.Utimes golang.org/x/sys/unix.UtimesNano
golang.org/x/sys/unix.UtimesNanoAt golang.org/x/sys/unix.Vmsplice golang.org/x/sys/unix.Write golang.org/x/sys/unix.Writev
golang.org/x/sys/unix.Acct golang.org/x/sys/unix.FcntlFstore golang.org/x/sys/unix.FcntlInt golang.org/x/sys/unix.FsconfigCreate golang.org/x/sys/unix.FsconfigReconfigure golang.org/x/sys/unix.FsconfigSetBinary golang.org/x/sys/unix.FsconfigSetFd golang.org/x/sys/unix.FsconfigSetFlag
golang.org/x/sys/unix.FsconfigSetPath golang.org/x/sys/unix.FsconfigSetPathEmpty golang.org/x/sys/unix.FsconfigSetString golang.org/x/sys/unix.Fsmount golang.org/x/sys/unix.Fsopen golang.org/x/sys/unix.Fspick golang.org/x/sys/unix.IoctlGetInt golang.org/x/sys/unix.IoctlGetTermios
golang.org/x/sys/unix.IoctlGetUint32 golang.org/x/sys/unix.IoctlGetUint64 golang.org/x/sys/unix.IoctlGetWinsize golang.org/x/sys/unix.IoctlIfreq golang.org/x/sys/unix.IoctlLoopConfigure golang.org/x/sys/unix.IoctlLoopSetStatus64 golang.org/x/sys/unix.IoctlRetInt golang.org/x/sys/unix.IoctlSetInt
golang.org/x/sys/unix.IoctlSetPointerInt golang.org/x/sys/unix.IoctlSetString golang.org/x/sys/unix.IoctlSetTermios golang.org/x/sys/unix.IoctlSetWinsize golang.org/x/sys/unix.Madvise
golang.org/x/sys/windows.Bind golang.org/x/sys/windows.Chmod golang.org/x/sys/windows.Chown golang.org/x/sys/windows.CreateDirectory golang.org/x/sys/windows.CreateFileMapping golang.org/x/sys/windows.CreateHardLink golang.org/x/sys/windows.CreateSymbolicLink
golang.org/x/sys/windows.DefineDosDevice golang.org/x/sys/windows.DeleteFile golang.org/x/sys/windows.DeleteVolumeMountPoint golang.org/x/sys/windows.DeviceIoControl golang.org/x/sys/windows.Fchmod golang.org/x/sys/windows.Fchown golang.org/x/sys/windows.Ftruncate
golang.org/x/sys/windows.Lchown golang.org/x/sys/windows.Link golang.org/x/sys/windows.MapViewOfFile golang.org/x/sys/windows.Mkdir golang.org/x/sys/windows.MoveFile golang.org/x/sys/windows.MoveFileEx golang.org/x/sys/windows.NtSetEaFile
golang.org/x/sys/windows.NtSetInformationFile golang.org/x/sys/windows.RemoveDirectory golang.org/x/sys/windows.Rename golang.org/x/sys/windows.Rmdir golang.org/x/sys/windows.SetEndOfFile golang.org/x/sys/windows.SetFileAttributes
golang.org/x/sys/windows.SetFileInformationByHandle golang.org/x/sys/windows.SetFileTime golang.org/x/sys/windows.SetFileValidData golang.org/x/sys/windows.SetKernelObjectSecurity golang.org/x/sys/windows.SetNamedSecurityInfo golang.org/x/sys/windows.SetSecurityInfo
golang.org/x/sys/windows.SetVolumeLabel golang.org/x/sys/windows.SetVolumeMountPoint golang.org/x/sys/windows.Symlink golang.org/x/sys/windows.Unlink golang.org/x/sys/windows.Utimes golang.org/x/sys/windows.UtimesNano golang.org/x/sys/windows.Write
golang.org/x/sys/windows.WriteFile
golang.org/x/sys/windows.CertOpenStore
github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateMkdir github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresMkdir github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRename
github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateRemove github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateRename
(*Cell).appendUpdateJournalEntry (*Cell).copyCheckpoint (*Cell).deleteDrillLocked (*Cell).discardJournalTail (*Cell).discardOwnCheckpoint (*Cell).finishRemoval (*Cell).journal (*Cell).journalCause (*Cell).listenServiceControl (*Cell).preparePostgresDirectory
(*Cell).publishServiceLease (*Cell).reconcileOldBoot (*Cell).removeCheckpointHeld (*Cell).removeOwnedServiceControlEndpoint (*Cell).removeServiceControlEndpoint (*Cell).stopChildOutcomeWithBudget (*Cell).sweepPostgresPending (*Cell).unsealCheckpointHeld
(*Cell).writePostgresConfig (*Cell).writeServiceDescriptor (*launchdAgentAdapter).inspectDirectories (*launchdAgentAdapter).removePlistIdentity (*systemdUserAdapter).inspectDirectories (*systemdUserAdapter).removeCreatedUnit acquireLock createWindowsTaskRecord
drillRootSweep makePrivateDir privateDir publishPrivateFile removePrivateFile writeAtomic writeAtomicBytes writeAtomicBytesOnce writeAtomicOnce
(*Cell).bindServiceProcessBoot (*Cell).bindServiceProcessBootWithReader (*Cell).deleteCheckpointLocked (*Cell).inventoryCheckpoint (*Cell).launchChild (*Cell).launchWindows
(*Cell).logFile (*Cell).startChild (*Cell).startChildContext (*Cell).startChildPrepared (*Cell).stopChildOutcome (*Cell).terminateChild (*Cell).updateStep
(*Cell).waitReadyWithPortRecovery (*Cell).witnessCheckpoint (*Cell).writeUpdateRecord (*launchdAgentAdapter).cleanupPartial (*systemdUserAdapter).cleanupPartial InstallLocalShutdown
`)

// packageReferencesList is every non-test file of the package, then its import
// paths on one "import" line, then each of its declarations that references a
// guarded object, with the count.
var packageReferencesList = `
checkpoint.go
	import bytes context crypto/sha256 encoding/hex encoding/json errors github.com/witwave-ai/witself/internal/id github.com/witwave-ai/witself/internal/runtimeconfig io io/fs math os os/exec path path/filepath regexp runtime slices sort strings sync time unicode/utf16 unicode/utf8
	(*Cell).DeleteCheckpoint (*Cell).deleteCheckpointLocked
	(*Cell).admitCheckpoint openPostgresReader readRegular
	(*Cell).admitDeleteCheckpoint os.ReadDir privateDir
	(*Cell).checkpointLocked (*Cell).copyCheckpoint (*Cell).inventoryCheckpoint (*Cell).journal (*Cell).witnessCheckpoint
	(*Cell).checkpointOps openPrivateReader x3
	(*Cell).closeCheckpointFailure (*Cell).journal
	(*Cell).closeCheckpointLeftover (*Cell).journal
	(*Cell).copyCheckpoint (*Cell).openCheckpointSource (*os.File).Write privateDir
	(*Cell).deleteCheckpointLocked (*Cell).holdCheckpoint (*Cell).journal (*Cell).removeCheckpointHeld (*Cell).unsealCheckpointHeld privateDir
	(*Cell).inventoryCheckpoint openPrivateReader path/filepath.WalkDir writeAtomicBytes
	(*Cell).listCheckpoints os.ReadDir
	(*Cell).openCheckpointEntry openPrivateReader x2
	(*Cell).planCheckpoint path/filepath.WalkDir
	(*Cell).refuseBeforeLockNames acquireLock
	(*Cell).validateRecovery os.ReadDir path/filepath.WalkDir privateDir
	(*Cell).witnessCheckpoint (*Cell).openCheckpointSource writeAtomic
	checkpointComplete openRegular
checkpoint_other.go
	import context os
checkpoint_root.go
	import errors io/fs os path/filepath runtime
	(*Cell).holdCheckpoint (*Cell).holdRecovery (*os.Root).OpenRoot
	(*Cell).holdRecovery os.OpenRoot
	(*Cell).removeCheckpointHeld (*os.Root).Open (*os.Root).Remove (*os.Root).RemoveAll
	(*Cell).unsealCheckpointHeld (*os.Root).Remove
	syncRootDirectory (*os.Root).Open
checkpoint_root_windows.go
	import errors io/fs
	(*Cell).discardOwnCheckpoint (*Cell).holdCheckpoint (*Cell).removeCheckpointHeld (*os.Root).Remove
checkpoint_tree_other.go
	import io/fs os
checkpoint_tree_unix.go
	import errors golang.org/x/sys/unix io/fs os path path/filepath
	(*checkpointTree).hold (*Cell).holdRecovery
	checkpointTreeRemoveDirectory golang.org/x/sys/unix.Unlinkat
checkpoint_tree_windows.go
	import io/fs os path path/filepath
	(*checkpointTree).createTemp createPrivateTemp
	(*checkpointTree).discard (*Cell).discardOwnCheckpoint
	(*checkpointTree).mkdir privateDir
	(*checkpointTree).publish publishPrivateFile
	(*checkpointTree).sync syncDirectory
checkpoint_unix.go
	import context errors io/fs os path/filepath reflect syscall
	(*Cell).openCheckpointSource (*os.Root).OpenFile os.OpenRoot
checkpoint_verify.go
	import bytes context crypto/sha256 encoding/hex encoding/json io io/fs math os path/filepath reflect strings time unicode/utf8
	checkpointVerifyRead openPrivateReader
	checkpointVerifyTree path/filepath.WalkDir
checkpoint_windows.go
	import context github.com/witwave-ai/witself/internal/runtimeconfig golang.org/x/sys/windows os path/filepath
	(*Cell).openCheckpointSource github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
commands.go
	import bytes context crypto/rand encoding/base64 errors fmt io net net/http net/url os os/exec path/filepath regexp runtime strconv strings time
	(*Cell).configurePostgres (*Cell).sweepPostgresPending (*Cell).writePostgresConfig
	(*Cell).logFile openRegularWriter
	(*Cell).makePassword writeAtomicBytes
	(*Cell).password openPrivateReader readRegular
	(*Cell).pgCommand (*Cell).logFile (*os.File).Write x2
	(*Cell).startChild (*Cell).startChildContext
	(*Cell).startChildContext (*Cell).startChildPrepared
	(*Cell).startChildPrepared (*Cell).bindServiceProcessBoot (*Cell).launchChild (*Cell).logFile
	ready (*net.UnixListener).Close
doc.go
drill_record.go
	import bytes encoding/json errors io strings
drill_reserve.go
	import context encoding/json errors github.com/witwave-ai/witself/internal/id io/fs os path path/filepath runtime strings
	(*Cell).drillReserveRecord (*os.File).Write (*os.Root).Remove (*os.Root).Rename
	(*Cell).reserveDrillLocked (*Cell).deleteDrillLocked (*os.Root).Open x5 os.OpenRoot
	drillReserveRoot (*os.Root).OpenRoot
drill_reserve_linux.go
	import errors golang.org/x/sys/unix math os strings unsafe
	drillPlatformAllocate golang.org/x/sys/unix.Fallocate
	drillPlatformAt (*os.Root).Open golang.org/x/sys/unix.Openat
	drillPlatformCreate golang.org/x/sys/unix.Openat os.NewFile
	drillPlatformMkdir golang.org/x/sys/unix.Mkdirat
	drillPlatformOpen golang.org/x/sys/unix.Openat os.NewFile
	drillPlatformVerify golang.org/x/sys/unix.Syscall unsafe.Pointer
drill_reserve_other.go
	import os
drill_reserve_windows.go
	import errors github.com/witwave-ai/witself/internal/runtimeconfig golang.org/x/sys/windows math os strings unsafe
	drillPlatformAllocate golang.org/x/sys/windows.Ftruncate
	drillPlatformVerify unsafe.Pointer
drill_root.go
	import context errors io os path/filepath runtime strings syscall
	(*Cell).deleteDrillLocked (*os.Root).Open x2 (*os.Root).OpenRoot (*os.Root).Remove x2 drillRootSweep x2 os.OpenRoot
	(*Cell).drillRootList os.ReadDir
	drillRootClassify openPrivateFile os.ReadDir privateDir
	drillRootHeld (*os.Root).OpenFile
	drillRootNames (*os.Root).Open
	drillRootSweep (*os.Root).RemoveAll
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
	import bufio bytes context encoding/json errors fmt github.com/witwave-ai/witself/internal/runtimeconfig io os path/filepath runtime strings time
	(*Cell).discardJournalTail (*os.File).Truncate openRegularWriter
	(*Cell).journal (*Cell).journalCause
	(*Cell).journalCause (*os.File).Write openRegularWriter syncDirectory
	(*Cell).layout privateDir
	(*Cell).readJournal openRegular
	(*Cell).readUpdateJournal openRegular
	(*Cell).step (*Cell).journal x3 (*Cell).journalCause x2
	(*Cell).validateRoot path/filepath.EvalSymlinks privateDir x2
	openRegular openPrivateReader openRegularBy
	openRegularWriter openPrivateWriter
	privateDir makePrivateDir syncDirectory
	readJSON openRegular
	readManifest readJSON
	readRegular openRegularBy
	syncDirectory os.Open
	syncJournalDirectory syncDirectory
	syncPrivateDirParent syncDirectory
	writeAtomic writeAtomicOnce
	writeAtomicBytes writeAtomicBytesOnce
	writeAtomicBytesOnce (*os.File).Write createPrivateTemp publishPrivateFile removePrivateFile syncDirectory
	writeAtomicOnce writeAtomicBytesOnce
files_other.go
	import context errors os runtime syscall time
	(*Cell).validatePostgresDirectory privateDir
	(*Cell).writePostgresConfig writeAtomicBytes
	createPrivateTemp os.CreateTemp
	makePrivateDir os.Mkdir
	openPlainReader openPrivateReader
	openPostgresReader openPrivateReader
	openPrivateFile os.OpenFile
	openPrivateReader os.OpenFile
	openPrivateWriter os.OpenFile
	publishPrivateFile os.Rename
	readPostgresFile readPrivateFile
	readPrivateFile os.ReadFile
	removePrivateFile os.Remove
files_windows.go
	import context errors github.com/witwave-ai/witself/internal/runtimeconfig io os path/filepath
	createPrivateTemp github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp
	makePrivateDir github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateMkdir
	openPlainReader os.Open
	openPrivateFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateOpenFile
	openPrivateReader openPrivateFile
	openPrivateWriter openPrivateFile
	publishPrivateFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateRename
	readPrivateFile openPrivateFile
	removePrivateFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateRemove
	validatePrivateTree path/filepath.WalkDir
fixed_cause.go
installation_identity.go
	import github.com/witwave-ai/witself/internal/id strings
installer_principal_credentials.go
	import context crypto/rand encoding/base64 errors github.com/witwave-ai/witself/internal/store io net/url os reflect runtime strings
	(*Cell).credentialPassword openPrivateReader privateDir
	(*Cell).makePrincipalPasswords os.ReadDir privateDir x3 writeAtomicBytes
	(*Cell).validatePrincipalPasswords os.ReadDir privateDir
	openPrincipalCredentialFile openPrivateReader
installer_principals.go
	import context github.com/jackc/pgx/v5 github.com/jackc/pgx/v5/pgconn github.com/witwave-ai/witself/internal/runtimeconfig github.com/witwave-ai/witself/internal/store io slices strings time
	(*Cell).prepareSelectedHealthCredential privateDir
launch_unix.go
	import context errors io os os/exec syscall time
	(*Cell).launchChild (*net.UnixListener).Close (*os.File).Write x3
	DispatchProcessHelper writeAtomic
launch_windows.go
	import context errors golang.org/x/sys/windows os path/filepath runtime slices strings unicode/utf16 unsafe
	(*Cell).launchChild (*Cell).launchWindows
	(*Cell).launchWindows os.Open unsafe.Pointer x4 writeAtomic x3
lifecycle.go
	import context errors os path/filepath runtime strconv time
	(*Cell).Inspect acquireLock
	(*Cell).Install (*Cell).preparePostgresDirectory os.ReadDir x2 writeAtomic
	(*Cell).Remove (*Cell).finishRemoval x2 (*Cell).journal os.RemoveAll x2 writeAtomic x2
	(*Cell).Repair (*Cell).discardJournalTail (*Cell).finishRemoval (*Cell).journal writeAtomic
	(*Cell).Start (*Cell).bindServiceProcessBoot (*Cell).startChildPrepared x3 (*Cell).waitReadyWithPortRecovery x2
	(*Cell).begin (*Cell).journal
	(*Cell).finish (*Cell).journal writeAtomic
	(*Cell).finishRemoval os.Remove os.RemoveAll syncDirectory
	(*Cell).own acquireLock
	(*Cell).ownExisting acquireLock
	(*Cell).postmaster openPostgresReader readRegular
	(*Cell).principalStartOperations (*Cell).startChildPrepared x2 (*Cell).waitReadyWithPortRecovery x2
	(*Cell).processStateWithRecord readJSON
	(*Cell).recoverPostgresRecord openPostgresReader readRegular writeAtomic
	(*Cell).stopSelected (*Cell).stopChildOutcome
	(*Cell).stopSelectedLegacy (*Cell).terminateChild
	(*Cell).verifyStartAccount writeAtomic
lock.go
	import context errors fmt os path/filepath
	acquireLock makePrivateDir openLockFile x2
lock_contention.go
	import context errors os path/filepath runtime
	classifyOwnershipFailure openLockFile
lock_unix.go
	import context errors os syscall
	openLockFile os.NewFile syscall.Open
lock_unsupported.go
	import context errors os
lock_windows.go
	import context errors golang.org/x/sys/windows os
	openLockFile openPrivateFile
port_recovery.go
	import context errors time
	(*Cell).waitReadyWithPortRecovery (*Cell).journalCause (*Cell).startChildContext
ports.go
	import context net
	(*Cell).prepareComponentPortsFor (*Cell).journalCause writeAtomic
	allocatePorts (*net.UnixListener).Close net.Listen
	probePorts (*net.UnixListener).Close net.Listen
postgres_files_windows.go
	import context errors github.com/witwave-ai/witself/internal/runtimeconfig os path/filepath
	(*Cell).preparePostgresDirectory github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresMkdir
	(*Cell).writePostgresConfig (*os.File).Write github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivateCreateTemp github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRename removePrivateFile
	openPostgresReader github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresOpen
	readPostgresFile github.com/witwave-ai/witself/internal/runtimeconfig.WindowsPrivatePostgresRead
postgres_pending.go
	import os path/filepath
	(*Cell).sweepPostgresPending os.ReadDir x3 removePrivateFile x3
postgres_token_other.go
	import os/exec
postgres_token_windows.go
	import errors golang.org/x/sys/windows os/exec path/filepath runtime strings syscall unsafe
	createRestrictedTokenProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsPostgresToken (*golang.org/x/sys/windows.LazyProc).Call unsafe.Pointer x4
postgres_unix.go
	import context errors strconv strings
	(*Cell).startPostgres writeAtomic
postgres_windows.go
	import bytes context errors os path/filepath strconv strings time
	(*Cell).pendingPostgresWithoutPID readJSON
	(*Cell).pendingPostmasterPID readJSON
	(*Cell).startPostgres (*Cell).launchWindows (*Cell).logFile
	(*Cell).windowsPostgresReady readPostgresFile
	(*Cell).windowsStalePostmaster readPostgresFile
private_beneath_unix.go
	import errors golang.org/x/sys/unix io/fs math/rand/v2 os strconv strings time
	privateBeneathAt (*os.Root).Open golang.org/x/sys/unix.Openat
	privateBeneathCreateTemp golang.org/x/sys/unix.Openat os.NewFile
	privateBeneathDup golang.org/x/sys/unix.FcntlInt os.NewFile
	privateBeneathMkdir golang.org/x/sys/unix.Mkdirat
	privateBeneathOpen golang.org/x/sys/unix.Openat os.NewFile
	privateBeneathOpened golang.org/x/sys/unix.Openat
	privateBeneathPublish golang.org/x/sys/unix.Renameat
	privateBeneathRemove golang.org/x/sys/unix.Unlinkat x2
	privateBeneathRemoveBelow golang.org/x/sys/unix.Unlinkat x2
	privateBeneathReplace golang.org/x/sys/unix.Renameat
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
	(*Cell).finishReinstall (*Cell).journal writeAtomic
	(*Cell).reinstallAvailable openPostgresReader readRegular
	(*Cell).reinstallLocked writeAtomic
	(*Cell).startRetainedPostgres (*Cell).bindServiceProcessBoot
removal_retention.go
	import context errors os time
	(*Cell).finishRetainedRemoval (*Cell).journal x3 openRegularWriter syncDirectory writeAtomic x2
	(*Cell).retainRemoval writeAtomic
rollback_policy.go
service.go
	import context errors
service_adapter_admission.go
	import context errors os path/filepath reflect runtime strings time
	serviceAdapterCanonicalPath path/filepath.EvalSymlinks
	serviceAdapterRemovalHostLock openLockFile
service_boot.go
	import context errors os strings
	(*Cell).bindServiceProcessBoot (*Cell).bindServiceProcessBootWithReader
	(*Cell).bindServiceProcessBootWithReader readJSON writeAtomic
	(*Cell).reconcileOldBoot readJSON removePrivateFile x2 syncDirectory x2
	(*Cell).reconcileServiceBoot (*Cell).reconcileOldBoot readJSON writeAtomic
	(*Cell).startServicePostgres (*Cell).bindServiceProcessBoot
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
	(*Cell).Control (*net.UnixListener).Close
	(*Cell).serveServiceControlForPlatform (*Cell).listenServiceControl (*Cell).removeServiceControlEndpoint (*os.File).Write x3
	startServiceControlServer (*os.File).Write
	writeServiceControlResponse (*os.File).Write
service_control_endpoint.go
	import context crypto/rand encoding/base32 encoding/json io os strings
	(*Cell).publishServiceControlEndpointWithBoot readJSON writeAtomic
	(*Cell).readServiceControlEndpointRecord openServiceRecord privateDir
	(*Cell).readServiceControlEndpointWithBoot readJSON
	(*Cell).refreshOwnedServiceControlEndpoint writeAtomic
	(*Cell).removeOwnedServiceControlEndpoint removePrivateFile
service_control_exit.go
	import bytes context crypto/rand encoding/base32 encoding/json errors fmt io net net/http os runtime strconv strings sync syscall time
	(*Cell).exitServiceHostForRemoval readJSON
	(*Cell).serveHostExit (*os.File).Write x2
	(*Cell).serviceHostExitIdentity readJSON
	(*Cell).serviceHostLeaseHeld openLockFile
	(*Cell).startHostExitRequest (*net.UnixListener).Close
	(*Cell).waitServiceHostExit readJSON x2
	(*serviceHostExitSession).finish (*net.UnixListener).Close
	(*supervisor).finishHostExit (*os.File).Write
service_control_exit_budget.go
	import context time
service_control_exit_unix.go
service_control_exit_windows.go
	import golang.org/x/sys/windows
service_control_unix.go
	import context errors net os syscall
	(*Cell).dialServiceControl (*net.Dialer).DialContext
	(*Cell).listenServiceControl (*net.UnixListener).Close x2 (*net.UnixListener).SetUnlinkOnClose net.ListenUnix os.Chmod removePrivateFile x2
	(*Cell).removeServiceControlEndpoint removePrivateFile
service_control_unsupported.go
	import context net
service_control_windows.go
	import context errors fmt golang.org/x/sys/windows io net os runtime sync time unsafe
	(*Cell).removeServiceControlEndpoint (*Cell).removeOwnedServiceControlEndpoint (*net.UnixListener).Close
	(*windowsPipeConn).io golang.org/x/sys/windows.WriteFile
	(*windowsPipeListener).Accept (*net.UnixListener).Close
	(*windowsPipeListener).acceptInstance (*os.File).Write x3
	windowsCreateNamedPipe golang.org/x/sys/windows.CreateNamedPipe
	windowsDialPipe golang.org/x/sys/windows.CreateFile
	windowsListenPipe golang.org/x/sys/windows.CreateNamedPipe
	windowsPipeValidateDescriptor unsafe.Pointer
service_descriptor.go
	import context crypto/rand encoding/base64 errors io os os/user path/filepath reflect strconv strings
	(*Cell).PrepareService (*Cell).writeServiceDescriptor privateDir writeAtomicBytes
	(*Cell).RepairService (*Cell).writeServiceDescriptor
	(*Cell).readServiceDescriptorForOwner privateDir readJSON
	(*Cell).serviceControlSecret openServiceRecord privateDir
	(*Cell).setServiceDesiredStateLocked (*Cell).writeServiceDescriptor
	(*Cell).setServiceFaulted (*Cell).writeServiceDescriptor
	(*Cell).writeServiceDescriptor removePrivateFile syncDirectory writeAtomic x2
	openServiceRecord openPrivateReader openRegularBy
	validateServiceHost path/filepath.EvalSymlinks x2
service_failure_site.go
	import errors fmt io sync/atomic
service_foreground.go
	import context errors os
	(*Cell).acquireServiceLeaseWithBoot (*Cell).publishServiceLease openLockFile privateDir
service_health.go
	import context crypto/rand encoding/base64 encoding/json errors github.com/witwave-ai/witself/internal/runtimeconfig github.com/witwave-ai/witself/internal/store io net/http net/url os time
	(*Cell).serviceAPICredential readJSON writeAtomic
	serviceAuthenticatedProbe (*net.UnixListener).Close
service_launchd.go
	import bytes context encoding/xml errors fmt io os os/exec path/filepath reflect slices strings time unicode unicode/utf8
	(*launchdAgentAdapter).Apply (*launchdAgentAdapter).cleanupPartial (*launchdAgentAdapter).inspectDirectories (*launchdAgentAdapter).removePlistIdentity (*os.File).Chmod (*os.File).Write os.OpenFile
	(*launchdAgentAdapter).Remove (*launchdAgentAdapter).removePlistIdentity
	(*launchdAgentAdapter).capability (*launchdAgentAdapter).inspectDirectories
	(*launchdAgentAdapter).cleanupPartial (*launchdAgentAdapter).removePlistIdentity
	(*launchdAgentAdapter).inspectDirectories os.Mkdir
	(*launchdAgentAdapter).ownedPlist (*launchdAgentAdapter).inspectDirectories openPlainReader
	(*launchdAgentAdapter).removePlistIdentity (*launchdAgentAdapter).inspectDirectories os.Remove
	(*launchdAgentAdapter).sync syncDirectory
service_launchd_darwin.go
	import context golang.org/x/sys/unix os os/exec os/user path/filepath strconv syscall time
service_lease_publish.go
	import fmt github.com/witwave-ai/witself/internal/runtimeconfig io os time
	(*Cell).publishServiceLease (*os.File).Write writeAtomicOnce
	(leasePublishHooks).defaults writeAtomicOnce x3
service_maintenance.go
	import context errors slices
service_shutdown.go
	import context errors fmt runtime time
service_shutdown_descriptor.go
	import context crypto/rand crypto/subtle encoding/base64 encoding/json errors io path/filepath strings
service_shutdown_other.go
	import context github.com/witwave-ai/witself/internal/runtimeconfig time
	(*Cell).stopChildOutcome (*Cell).terminateChild
service_shutdown_policy.go
	import context errors time
service_shutdown_protocol.go
	import bufio bytes context encoding/json io sync
service_shutdown_supervisor.go
	import context errors
service_shutdown_windows.go
	import context encoding/json errors fmt github.com/witwave-ai/witself/internal/runtimeconfig golang.org/x/sys/windows os path/filepath strings sync time
	(*Cell).prepareShutdownLaunch privateDir
	(*Cell).stopChildOutcome (*Cell).stopChildOutcomeWithBudget
	(*Cell).stopChildOutcomeWithBudget readJSON removePrivateFile
	(*windowsShutdownChannel).serve (*net.UnixListener).Close
	InstallLocalShutdown writeAtomic
	readShutdownDescriptor openPrivateFile privateDir x3
	waitShutdownReady readJSON
	windowsShutdownRequest (*net.UnixListener).Close
service_supervisor.go
	import context errors fmt os runtime slices time
	(*Cell).Run (*os.File).Write x3
	(*supervisor).call (*os.File).Write
	(*supervisor).load readJSON
	(*supervisor).rememberProcesses readJSON x2
	(*supervisor).save writeAtomic
service_systemd.go
	import context path/filepath strconv strings unicode
service_systemd_adapter.go
	import bytes context encoding/json errors fmt io os path/filepath reflect strings time
	(*systemdUserAdapter).Apply (*os.File).Write (*systemdUserAdapter).cleanupPartial (*systemdUserAdapter).inspectDirectories (*systemdUserAdapter).removeCreatedUnit x2 os.OpenFile syncDirectory
	(*systemdUserAdapter).Remove os.Remove syncDirectory
	(*systemdUserAdapter).capabilityForRoot (*systemdUserAdapter).inspectDirectories
	(*systemdUserAdapter).cleanupPartial (*systemdUserAdapter).removeCreatedUnit
	(*systemdUserAdapter).inspectDirectories os.Mkdir
	(*systemdUserAdapter).ownedUnit (*systemdUserAdapter).inspectDirectories openPlainReader
	(*systemdUserAdapter).removeCreatedUnit os.Remove syncDirectory
	systemdUserDirectories path/filepath.EvalSymlinks
service_systemd_errors.go
	import errors fmt os os/exec regexp strings unicode unicode/utf8
service_systemd_linux.go
	import context golang.org/x/sys/unix os os/exec os/user path/filepath strconv syscall time
service_task_host.go
service_unsupported.go
service_windows_task.go
	import bytes context encoding/json errors fmt io os path/filepath reflect time
	(*windowsLogonTaskAdapter).Apply createWindowsTaskRecord x2
	(*windowsLogonTaskAdapter).Remove removePrivateFile x2 syncDirectory
	(*windowsLogonTaskAdapter).records privateDir readWindowsTaskRecord x2
	createWindowsTaskRecord (*os.File).Write openPrivateFile syncDirectory
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
	(*Cell).terminateChild (*Cell).stopChildOutcome
	(*Cell).windowsStopRecord readJSON
	attachConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	freeConsoleProc (*golang.org/x/sys/windows.LazyDLL).NewProc golang.org/x/sys/windows.NewLazySystemDLL
	windowsConsoleBreak (*golang.org/x/sys/windows.LazyProc).Call x2
types.go
	import context errors github.com/witwave-ai/witself/internal/runtimeconfig net net/http os path/filepath sync time
	CanonicalRoot path/filepath.EvalSymlinks
update_fence.go
	import bytes context encoding/json errors io os time
	(*Cell).readMaintenanceFence readMaintenanceFenceJSON
	(*Cell).readMaintenanceFenceWith privateDir
	readMaintenanceFenceJSON openRegular
update_record.go
	import bytes context encoding/hex encoding/json errors io os strings time
	(*Cell).appendUpdateJournalEntry (*os.File).Write openPrivateFile syncDirectory
	(*Cell).authenticateUpdateInstallation privateDir
	(*Cell).readDerivedUpdateRecord openServiceRecord
	(*Cell).readUpdateIntent readJSON
	(*Cell).reconcileUpdateRecord (*Cell).writeUpdateRecord
	(*Cell).updateStep (*Cell).appendUpdateJournalEntry x2 (*Cell).writeUpdateRecord
	(*Cell).writeUpdateRecord writeAtomic
	openUpdateJournalFile openPrivateFile
	syncUpdateJournalDirectory syncDirectory
`
