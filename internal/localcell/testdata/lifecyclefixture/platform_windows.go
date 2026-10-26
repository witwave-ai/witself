//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/runtimeconfig"
	"golang.org/x/sys/windows"
)

func fixtureExecutable(name string) string { return name + ".exe" }
func fixtureMkdir(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// The product creates PGDATA with its inheritable private policy before
	// native initdb. Accept only that already-private empty fixture directory.
	if err := runtimeconfig.WindowsPrivatePostgresValidate(path, path, true); err == nil {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			return fmt.Errorf("fixture PGDATA is not empty")
		}
		return nil
	}
	return runtimeconfig.WindowsPrivatePostgresMkdir(path)
}
func fixtureOpenFile(path string, flags int) (*os.File, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return runtimeconfig.WindowsPrivateOpenFile(path, flags)
}
func fixtureWriteFile(path string, data []byte) error {
	file, err := fixtureOpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func notifyFixtureStop(stopped chan<- os.Signal) { signal.Notify(stopped, os.Interrupt) }
func fixturePostmasterRecord(data, port string) []byte {
	return []byte(fmt.Sprintf("%d\n%s\n%d\n%s\n\n127.0.0.1\n\nready\n", os.Getpid(), data, time.Now().Unix(), port))
}

// The synthetic pg_ctl mirrors PW2's verified detached console helper. Actual
// PostgreSQL uses its own pg_ctl shutdown protocol; no PID-only force kill or
// Unix signal emulation is used here. Holding the verified handle reserves its
// PID until the scoped CTRL_BREAK has been sent.
func stopFixturePostgres(data []byte) error {
	pid, err := strconv.Atoi(strings.Split(string(data), "\n")[0])
	if err != nil {
		return err
	}
	handle, err := fixturePostgresHandle(pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	command := exec.Command(os.Args[0], "--fixture-stop-postgres", strconv.Itoa(pid))
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	return command.Run()
}

func fixturePostgresHandle(pid int) (windows.Handle, error) {
	var record localcell.ProcessRecord
	data, err := os.ReadFile(filepath.Join("run", "postgres.json"))
	if err != nil {
		return 0, err
	}
	root, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	if json.Unmarshal(data, &record) != nil || record.PID != pid || pid <= 1 || record.Root != root || record.Component != "postgres" || record.LaunchPending {
		return 0, fmt.Errorf("fixture process identity mismatch")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	var created, exited, kernel, user windows.Filetime
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) || windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil || fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime) != record.StartTime {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("fixture process creation identity mismatch")
	}
	return handle, nil
}

func runFixtureStopHelper() bool {
	if len(os.Args) != 3 || os.Args[1] != "--fixture-stop-postgres" {
		return false
	}
	pid, err := strconv.Atoi(os.Args[2])
	if err != nil {
		os.Exit(125)
	}
	handle, err := fixturePostgresHandle(pid)
	if err != nil {
		os.Exit(125)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	attach := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	free := windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
	if result, _, _ := attach.Call(uintptr(pid)); result == 0 {
		os.Exit(124)
	}
	defer func() { _, _, _ = free.Call() }()
	if windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)) != nil {
		os.Exit(124)
	}
	return true
}
