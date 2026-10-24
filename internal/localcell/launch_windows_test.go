//go:build windows

package localcell

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func buildWindowsLaunchFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "main.go")
	const program = `package main
import ("encoding/json"; "io"; "os"; "path/filepath"; "time"; "unsafe"; "golang.org/x/sys/windows")
// The launcher's final publication replaces this file immediately after
// resume. Retain delete sharing so the observation itself cannot block it.
func readRecord(path string) ([]byte, error) {
  name, err := windows.UTF16PtrFromString(path); if err != nil { return nil, err }
  handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0); if err != nil { return nil, err }
  file := os.NewFile(uintptr(handle), "launch record fixture")
  defer file.Close()
  return io.ReadAll(file)
}
func writePrivate(path string, data []byte) error {
  user, err := windows.GetCurrentProcessToken().GetTokenUser(); if err != nil { return err }
  system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid); if err != nil { return err }
  sddl := "O:"+user.User.Sid.String()+"D:P(A;;FA;;;"+user.User.Sid.String()+")"
  if !user.User.Sid.Equals(system) { sddl += "(A;;FA;;;SY)" }
  sd, err := windows.SecurityDescriptorFromString(sddl); if err != nil { return err }
  name, err := windows.UTF16PtrFromString(path); if err != nil { return err }
  sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
  handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0); if err != nil { return err }
  file := os.NewFile(uintptr(handle), "private fixture file")
  _, err = file.Write(data); closeErr := file.Close(); if err != nil { return err }; return closeErr
}
func main() {
 root, _ := os.Getwd()
 data, err := readRecord(filepath.Join(root,"run","server.json"))
 var record struct { PID int; Root string; Component string }
 marker := "unfenced"
 if err == nil && json.Unmarshal(data,&record) == nil && record.PID == os.Getpid() && record.Root == root && record.Component == "server" { marker = "fenced" }
 _ = writePrivate(filepath.Join(root,"tmp","first-run"), []byte(marker))
 for { time.Sleep(time.Second) }
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal("cannot write launch fixture")
	}
	binary := filepath.Join(directory, "launch fixture.exe")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		t.Fatal("cannot build launch fixture")
	}
	return binary
}

func retainedLaunchHandle(t *testing.T, handle windows.Handle) windows.Handle {
	t.Helper()
	var retained windows.Handle
	if windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &retained, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
		t.Fatal("cannot retain launch witness")
	}
	t.Cleanup(func() {
		_ = windows.TerminateProcess(retained, 125)
		_, _ = windows.WaitForSingleObject(retained, 5000)
		_ = windows.CloseHandle(retained)
	})
	return retained
}

func assertWindowsProcessExited(t *testing.T, handle windows.Handle) {
	t.Helper()
	state, err := windows.WaitForSingleObject(handle, 5000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatal("owned process survived cleanup")
	}
}

func TestWindowsLaunchSyncPublishResumeOrder(t *testing.T) {
	binary := buildWindowsLaunchFixture(t)
	c, m := unitCell(t)
	c.cfg.Binaries.Server = binary
	if err := c.makePassword(); err != nil {
		t.Fatal(err)
	}
	originalSync, originalCheckpoint := syncFile, windowsLaunchCheckpoint
	t.Cleanup(func() { syncFile, windowsLaunchCheckpoint = originalSync, originalCheckpoint })
	var process, thread windows.Handle
	var syncs int
	syncFile = func(file *os.File) error {
		syncs++
		if syncs == 1 {
			// Observe the native suspend count, so a resume-before-sync
			// mutation fails even if the child has not been scheduled yet.
			suspend := windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")
			previous, _, _ := suspend.Call(uintptr(thread))
			if previous != 1 {
				t.Error("component thread was not suspended at record sync")
			}
			if previous != uintptr(^uint32(0)) {
				if _, err := windows.ResumeThread(thread); err != nil {
					t.Error("cannot restore fixture thread suspension")
				}
			}
			if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
				t.Error("component ran before its record was synced")
			}
		}
		return originalSync(file)
	}
	var stages []string
	windowsLaunchCheckpoint = func(stage string, pi windows.ProcessInformation) error {
		stages = append(stages, stage)
		if stage == "created" {
			process = retainedLaunchHandle(t, pi.Process)
			thread = pi.Thread
		}
		if stage == "published" {
			var record ProcessRecord
			if syncs != 1 || readJSON(c.path("run", "server.json"), &record) != nil || !record.LaunchPending || record.PID != int(pi.ProcessId) {
				t.Error("pending process record not durably published before resume")
			}
			if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
				t.Error("component ran while launch was suspended")
			}
		}
		return nil
	}
	if err := c.startChild(m, "server"); err != nil {
		t.Fatal(err)
	}
	if len(stages) != 5 || stages[0] != "created" || stages[1] != "published" || stages[2] != "resumed" || stages[3] != "confirmed" || stages[4] != "transferred" || syncs != 2 || process == 0 {
		t.Fatal("launch ownership stages did not preserve sync/publish/resume/transfer ordering")
	}
	var record ProcessRecord
	if readJSON(c.path("run", "server.json"), &record) != nil || record.LaunchPending {
		t.Fatal("successful launch retained a pending record")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(c.path("tmp", "first-run"))
		if err == nil {
			if string(data) != "fenced" {
				t.Fatal("component first ran without its own durable PID record")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("component failed to run after publication")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWindowsPublicationFailureTerminatesSuspendedChild(t *testing.T) {
	binary := buildWindowsLaunchFixture(t)
	for _, failAt := range []string{"sync", "publish"} {
		t.Run(failAt, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.Server = binary
			if c.makePassword() != nil {
				t.Fatal("cannot create private fixture password")
			}
			before, err := os.ReadFile(c.path("manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			originalSync, originalCheckpoint := syncFile, windowsLaunchCheckpoint
			t.Cleanup(func() { syncFile, windowsLaunchCheckpoint = originalSync, originalCheckpoint })
			var process windows.Handle
			windowsLaunchCheckpoint = func(stage string, pi windows.ProcessInformation) error {
				if stage == "created" {
					process = retainedLaunchHandle(t, pi.Process)
					if failAt == "publish" {
						if err := makePrivateDir(c.path("run", "server.json")); err != nil {
							return err
						}
					}
				} else {
					t.Error("publication failure advanced past suspended creation")
				}
				return nil
			}
			if failAt == "sync" {
				syncFile = func(*os.File) error { return errors.New("synthetic sync refusal") }
			}
			if err := c.startChild(m, "server"); err == nil || process == 0 {
				t.Fatal("publication failure was ignored")
			}
			assertWindowsProcessExited(t, process)
			if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unpublished component ran")
			}
			after, err := os.ReadFile(c.path("manifest.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("publication failure removed or changed unrelated state")
			}
			if failAt == "publish" {
				if info, err := os.Stat(c.path("run", "server.json")); err != nil || !info.IsDir() {
					t.Fatal("publication failure removed another owner's obstruction")
				}
			}
		})
	}
}

func TestWindowsLaunchCrashParentHelper(_ *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-4] != "localcell-launch-crash" {
		return
	}
	root, stage, binary := os.Args[len(os.Args)-3], os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	c, err := New(Config{Root: root, Binaries: Binaries{Server: binary}, Timeout: 10 * time.Second})
	if err != nil {
		os.Exit(125)
	}
	windowsLaunchCheckpoint = func(current string, pi windows.ProcessInformation) error {
		if current == stage {
			identity, alive, err := windowsProcessIdentity(pi.Process)
			if err != nil || !alive {
				os.Exit(125)
			}
			if json.NewEncoder(os.Stdout).Encode(ProcessRecord{PID: int(pi.ProcessId), StartTime: identity, Root: root, Component: "server"}) != nil {
				os.Exit(125)
			}
			var permit [1]byte
			_, _ = os.Stdin.Read(permit[:])
			os.Exit(125)
		}
		return nil
	}
	m, err := readManifest(root)
	if err != nil || c.startChild(m, "server") != nil {
		os.Exit(125)
	}
	os.Exit(126)
}

func TestWindowsLaunchParentCrashWindows(t *testing.T) {
	binary := buildWindowsLaunchFixture(t)
	for _, stage := range []string{"created", "published", "resumed", "confirmed", "transferred"} {
		t.Run(stage, func(t *testing.T) {
			c, _ := unitCell(t)
			if c.makePassword() != nil || c.journal("start", "operation", "started") != nil || c.journal("start", "server", "started") != nil {
				t.Fatal("cannot prepare crash journal")
			}
			parent := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsLaunchCrashParentHelper$", "--", "localcell-launch-crash", c.cfg.Root, stage, binary)
			parent.Env, _ = c.baseEnvironment()
			parent.Env = appendEnvironment(parent.Env, "WITSELF_HOME="+c.path("home"), "DSH_HOME="+c.path("home"))
			input, err := parent.StdinPipe()
			if err != nil {
				t.Fatal("cannot control crash parent")
			}
			output, err := parent.StdoutPipe()
			if err != nil {
				t.Fatal("cannot read crash witness")
			}
			parent.Stderr = io.Discard
			if parent.Start() != nil {
				t.Fatal("cannot start crash parent")
			}
			t.Cleanup(func() { _ = input.Close(); _ = parent.Process.Kill(); _ = parent.Wait() })
			var record ProcessRecord
			if json.NewDecoder(bufio.NewReader(output)).Decode(&record) != nil || record.PID <= 1 {
				t.Fatal("parent did not reach crash fence")
			}
			handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
			if err != nil || !alive {
				t.Fatal("cannot retain crash child identity")
			}
			process := retainedLaunchHandle(t, handle)
			_ = windows.CloseHandle(handle)
			if parent.Process.Kill() != nil {
				t.Fatal("cannot simulate parent death")
			}
			_ = parent.Wait()
			if stage == "transferred" {
				// Final record already owns this surviving component. Terminate
				// via the exact retained handle (console behavior has its own suite).
				_, alive, err := windowsProcessIdentity(process)
				if err != nil || !alive {
					t.Fatal("successful ownership transfer retained parent-lifetime coupling")
				}
				_ = windows.TerminateProcess(process, 0)
			}
			assertWindowsProcessExited(t, process)
			if err := c.Repair(t.Context()); err != nil {
				t.Fatal("Repair failed to settle crashed launch")
			}
			progress, err := c.readJournal()
			if err != nil || progress.pending != nil {
				t.Fatal("Repair left launch journal interrupted")
			}
			if stage == "created" {
				if _, err := os.Stat(c.path("run", "server.json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unpublished crash unexpectedly adopted a PID")
				}
			}
			if stage == "created" || stage == "published" {
				if _, err := os.Stat(c.path("tmp", "first-run")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("suspended crash child executed")
				}
			}
		})
	}
}
