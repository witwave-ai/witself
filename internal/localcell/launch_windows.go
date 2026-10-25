//go:build windows

package localcell

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows 10+: assigning the kill-on-close job at creation is atomic. Assigning
// it AFTER CreateProcess would leave an unowned suspended process on a crash.
// x/sys exposes the attribute API but not this Windows SDK constant.
const procThreadAttributeJobList = 0x0002000d

// Native test seam. Production has no callback and never pauses at these fences.
var windowsLaunchCheckpoint func(string, windows.ProcessInformation) error

func launchCheckpoint(stage string, process windows.ProcessInformation) error {
	if windowsLaunchCheckpoint != nil {
		return windowsLaunchCheckpoint(stage, process)
	}
	return nil
}

func (c *Cell) launchChild(binary, component string, environment []string, log *os.File) error {
	return c.launchWindows(binary, []string{"serve"}, component, environment, log, nil)
}

// launchWindows creates the actual executable, never a shell or reexec proxy.
// The parent exclusively owns a non-inherited kill-on-close job through resume
// and final durable publication. Any crash before transfer kills the whole job,
// including PostgreSQL descendants; Repair reconciles the fenced dead record.
// Repair also terminates any identity-verified pending launch still alive.
// Successful children outlive the harness: only after final publication is the
// job disarmed, transferring cleanup authority to the durable process fence.
func (c *Cell) launchWindows(binary string, args []string, component string, environment []string, log *os.File, ready func(ProcessRecord) error) error {
	if !filepath.IsAbs(binary) || (component != "server" && component != "worker" && component != "postgres") {
		return errors.New("invalid local cell component launch")
	}
	application, err := windows.UTF16PtrFromString(binary)
	if err != nil {
		return errors.New("invalid local cell component executable")
	}
	command, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{binary}, args...)))
	if err != nil {
		return errors.New("invalid local cell component arguments")
	}
	directory, err := windows.UTF16PtrFromString(c.cfg.Root)
	if err != nil {
		return errors.New("invalid local cell component directory")
	}
	block, err := windowsEnvironmentBlock(environment)
	if err != nil {
		return err
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return errors.New("cannot create local cell launch job")
	}
	defer func() { _ = windows.CloseHandle(job) }()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return errors.New("cannot protect local cell launch job")
	}
	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return errors.New("cannot create local cell launch attributes")
	}
	defer attributes.Delete()
	if err := attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return errors.New("local cell requires atomic Windows job assignment")
	}
	// Inherit only duplicates of NUL stdin and the private log. In particular,
	// never inherit the job, process, thread, lock, or caller console handles.
	null, err := os.Open(os.DevNull)
	if err != nil {
		return errors.New("cannot open local cell input")
	}
	defer func() { _ = null.Close() }()
	var input, output windows.Handle
	self := windows.CurrentProcess()
	if err := windows.DuplicateHandle(self, windows.Handle(null.Fd()), self, &input, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return errors.New("cannot isolate local cell input")
	}
	defer func() { _ = windows.CloseHandle(input) }()
	if err := windows.DuplicateHandle(self, windows.Handle(log.Fd()), self, &output, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return errors.New("cannot isolate local cell output")
	}
	defer func() { _ = windows.CloseHandle(output) }()
	handles := []windows.Handle{input, output}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return errors.New("cannot restrict local cell inherited handles")
	}
	startup := windows.StartupInfoEx{ProcThreadAttributeList: attributes.List()}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.StdInput, startup.StdOutput, startup.StdErr = input, output, output
	var process windows.ProcessInformation
	// Share the console association for CTRL_BREAK, but create an isolated
	// process group. No NEW_CONSOLE (which ignores NEW_PROCESS_GROUP), detached
	// process, or ambient stdio inheritance. If the target has no console,
	// graceful stop explicitly refuses; service semantics remain deferred.
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	err = windows.CreateProcess(application, command, nil, nil, true, flags, &block[0], directory, &startup.StartupInfo, &process)
	runtime.KeepAlive(handles)
	runtime.KeepAlive(job)
	runtime.KeepAlive(block)
	if err != nil {
		return errors.New("cannot create suspended local cell component")
	}
	defer func() { _ = windows.CloseHandle(process.Process) }()
	defer func() { _ = windows.CloseHandle(process.Thread) }()
	completed := false
	defer func() {
		if !completed {
			// Retained handle, never a reopened PID. Do not remove any record:
			// Repair retains authority even when cleanup itself is interrupted.
			_ = windows.TerminateProcess(process.Process, 125)
			_, _ = windows.WaitForSingleObject(process.Process, 5000)
		}
	}()
	if err := launchCheckpoint("created", process); err != nil {
		return err
	}
	identity, alive, err := windowsProcessIdentity(process.Process)
	if err != nil || !alive {
		return errors.New("cannot identify suspended local cell component")
	}
	record := ProcessRecord{PID: int(process.ProcessId), StartTime: identity, Root: c.cfg.Root, Component: component, LaunchPending: true}
	if err := writeAtomic(c.path("run", component+".json"), record, nil); err != nil {
		return err
	}
	if err := launchCheckpoint("published", process); err != nil {
		return err
	}
	if previous, err := windows.ResumeThread(process.Thread); err != nil || previous != 1 {
		return errors.New("cannot resume local cell component")
	}
	if err := launchCheckpoint("resumed", process); err != nil {
		return err
	}
	if ready != nil {
		if err := ready(record); err != nil {
			return err
		}
	}
	record.LaunchPending = false
	if err := writeAtomic(c.path("run", component+".json"), record, nil); err != nil {
		return err
	}
	if err := launchCheckpoint("confirmed", process); err != nil {
		return err
	}
	// This is the only transfer of cleanup ownership, after file Sync and
	// publication of the final record. Windows power-loss durability is not claimed.
	limits.BasicLimitInformation.LimitFlags = 0
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return errors.New("cannot transfer local cell launch ownership")
	}
	if err := launchCheckpoint("transferred", process); err != nil {
		return err
	}
	completed = true
	return nil
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	environment = appendEnvironment(nil, environment...)
	slices.SortFunc(environment, func(a, b string) int { return strings.Compare(strings.ToUpper(a), strings.ToUpper(b)) })
	hasSystemRoot := false
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			return nil, errors.New("invalid local cell child environment")
		}
		if strings.EqualFold(name, "SystemRoot") && value != "" {
			hasSystemRoot = true
		}
	}
	if !hasSystemRoot {
		return nil, errors.New("local cell child environment lacks trusted SystemRoot")
	}
	return utf16.Encode([]rune(strings.Join(environment, "\x00") + "\x00\x00")), nil
}
