//go:build windows

package localcell

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This is deliberately separate from lifecyclefixture: it proves the Windows
// launch boundary without claiming PostgreSQL distribution or lifecycle proof.
func buildWindowsPostgresLaunchFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "postgres_fixture.go")
	const program = `package main
import ("encoding/json"; "fmt"; "os"; "os/exec"; "path/filepath"; "strconv"; "time"; "unsafe"; "golang.org/x/sys/windows")
type record struct { PID int; Root string; Component string; LaunchPending bool ` + "`json:\"launch_pending\"`" + ` }
// The synthetic peer creates private files explicitly. It makes no claim that
// native PostgreSQL-created files satisfy PW1's non-inheritable DACL policy.
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
  root, err := os.Getwd(); if err != nil { os.Exit(101) }
  if len(os.Args) == 2 && os.Args[1] == "--descendant" {
    var created, exited, kernel, user windows.Filetime
    if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user) != nil { os.Exit(110) }
    data, err := json.Marshal(map[string]any{"pid": os.Getpid(), "start_time": fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime), "root": root, "component": "postgres"})
    if err != nil || writePrivate(filepath.Join(root, "tmp", "postgres-fixture-descendant"), data) != nil { os.Exit(111) }
    for { time.Sleep(time.Hour) }
  }
  if filepath.Base(os.Args[0]) != "postgres.exe" {
    _ = writePrivate(filepath.Join(root, "tmp", "unexpected-shell-launch"), []byte("called"))
    os.Exit(102)
  }
  if len(os.Args) != 3 || os.Args[1] != "-D" || os.Args[2] != filepath.Join(root, "pg") { os.Exit(103) }
  data, err := os.ReadFile(filepath.Join(root, "run", "postgres.json")); if err != nil { os.Exit(104) }
  var r record
  if json.Unmarshal(data, &r) != nil || r.PID != os.Getpid() || r.Root != root || r.Component != "postgres" || !r.LaunchPending { os.Exit(105) }
  if writePrivate(filepath.Join(root, "tmp", "postgres-fixture-record"), data) != nil { os.Exit(106) }
  pid := os.Getpid()
  status := "ready"
  partial := false
  stale := false
  if mode, err := os.ReadFile(filepath.Join(root, "tmp", "postgres-fixture-mode")); err == nil {
    if string(mode) == "starting-descendant" {
      status = "starting"
      child := exec.Command(os.Args[0], "--descendant")
      child.Stdout, child.Stderr = os.Stdout, os.Stderr
      if child.Start() != nil { os.Exit(112) }
      if child.Process.Release() != nil { os.Exit(113) }
      deadline := time.Now().Add(5*time.Second)
      for {
        if _, err := os.Stat(filepath.Join(root, "tmp", "postgres-fixture-descendant")); err == nil { break }
        if time.Now().After(deadline) { os.Exit(114) }
        time.Sleep(10*time.Millisecond)
      }
    } else if string(mode) == "starting" { status = "starting" } else if string(mode) == "partial" { partial = true } else if string(mode) == "stale" { stale = true } else {
      pid, err = strconv.Atoi(string(mode)); if err != nil { os.Exit(107) }
    }
  }
  if partial {
    if writePrivate(filepath.Join(root, "pg", "postmaster.pid"), []byte(fmt.Sprintf("%d\r\n", pid))) != nil { os.Exit(109) }
    time.Sleep(200*time.Millisecond)
  }
  if stale { time.Sleep(200*time.Millisecond) }
  text := fmt.Sprintf("%d\r\n%s\r\n%d\r\n21101\r\n\r\n127.0.0.1\r\n\r\n%s\r\n", pid, os.Args[2], time.Now().Unix(), status)
  if writePrivate(filepath.Join(root, "pg", "postmaster.pid"), []byte(text)) != nil { os.Exit(108) }
  for { time.Sleep(time.Hour) }
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal("cannot write PostgreSQL launch fixture source")
	}
	binary := filepath.Join(directory, "postgres.exe")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot build PostgreSQL launch fixture")
	}
	data, err := os.ReadFile(binary)
	if err != nil || os.WriteFile(filepath.Join(directory, "pg_ctl.exe"), data, 0o700) != nil {
		t.Fatal("cannot prepare PostgreSQL shell-launch trap")
	}
	return directory
}

func cleanupWindowsPostgresFixture(t *testing.T, c *Cell) {
	t.Helper()
	for _, marker := range []string{"postgres-fixture-record", "postgres-fixture-descendant"} {
		cleanupWindowsPostgresRecord(t, c, marker)
	}
}

func cleanupWindowsPostgresRecord(t *testing.T, c *Cell, marker string) {
	t.Helper()
	t.Cleanup(func() {
		var record ProcessRecord
		if err := readJSON(c.path("tmp", marker), &record); errors.Is(err, os.ErrNotExist) {
			return
		} else if err != nil {
			t.Error("cannot read PostgreSQL fixture cleanup identity")
			return
		}
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(record.PID))
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return
		}
		if err != nil {
			t.Error("cannot open PostgreSQL fixture cleanup handle")
			return
		}
		defer func() { _ = windows.CloseHandle(handle) }()
		state, err := windows.WaitForSingleObject(handle, 0)
		if err == nil && state == windows.WAIT_OBJECT_0 {
			return
		}
		var created, exited, kernel, user windows.Filetime
		if err != nil || state != uint32(windows.WAIT_TIMEOUT) || windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil || fmt.Sprintf("windows:%d:%d", created.HighDateTime, created.LowDateTime) != record.StartTime {
			t.Error("refusing mismatched PostgreSQL fixture cleanup identity")
			return
		}
		if windows.TerminateProcess(handle, 1) != nil {
			t.Error("cannot terminate PostgreSQL fixture during cleanup")
			return
		}
		if state, err := windows.WaitForSingleObject(handle, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
			t.Error("PostgreSQL fixture survived cleanup")
		}
	})
}

func TestWindowsPostgresLaunchUsesPublishedPostmasterIdentity(t *testing.T) {
	directory := buildWindowsPostgresLaunchFixture(t)
	for _, mode := range []string{"ready", "partial", "stale"} {
		t.Run(mode, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.PostgresDir = directory
			c.cfg.Timeout = 5 * time.Second
			if err := testMkdir(c.path("pg"), 0o700); err != nil {
				t.Fatal(err)
			}
			if mode != "ready" {
				if err := testWriteFile(c.path("tmp", "postgres-fixture-mode"), []byte(mode), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "stale" {
				previous := signalSafetyChild(t)
				handle, alive, err := openVerifiedWindowsProcess(previous, windows.PROCESS_TERMINATE)
				if err != nil || !alive {
					t.Fatal("cannot retain old PostgreSQL fixture identity")
				}
				defer func() { _ = windows.CloseHandle(handle) }()
				if windows.TerminateProcess(handle, 0) != nil {
					t.Fatal("cannot stop old PostgreSQL fixture")
				}
				if state, err := windows.WaitForSingleObject(handle, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
					t.Fatal("old PostgreSQL fixture did not exit")
				}
				stale := fmt.Sprintf("%d\r\n%s\r\n1\r\n21101\r\n\r\n127.0.0.1\r\n\r\nready\r\n", previous.PID, c.path("pg"))
				if err := testWriteFile(c.path("pg", "postmaster.pid"), []byte(stale), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cleanupWindowsPostgresFixture(t, c)
			if err := c.startPostgres(t.Context(), m); err != nil {
				t.Fatal("direct PostgreSQL fixture did not launch")
			}
			var launched, complete ProcessRecord
			if readJSON(c.path("tmp", "postgres-fixture-record"), &launched) != nil || !launched.LaunchPending {
				t.Fatal("PostgreSQL executed without a published pending identity")
			}
			if readJSON(c.path("run", "postgres.json"), &complete) != nil || complete.LaunchPending || complete.PID != launched.PID || complete.StartTime != launched.StartTime {
				t.Fatal("PostgreSQL launch replaced its suspended creation identity")
			}
			pid, err := c.postmaster()
			if err != nil || pid != complete.PID {
				t.Fatal("Windows CRLF postmaster identity did not match the actual executable")
			}
			if _, err := os.Stat(c.path("tmp", "unexpected-shell-launch")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("PostgreSQL launch adopted a shell instead of creating postgres.exe")
			}
		})
	}
}

func TestWindowsPostgresLaunchRefusesForeignPIDAndUnreadyState(t *testing.T) {
	directory := buildWindowsPostgresLaunchFixture(t)
	for _, mode := range []string{"foreign-pid", "starting", "starting-descendant"} {
		t.Run(mode, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.PostgresDir = directory
			c.cfg.Timeout = time.Second
			if err := testMkdir(c.path("pg"), 0o700); err != nil {
				t.Fatal(err)
			}
			var unrelated ProcessRecord
			control := mode
			if mode == "foreign-pid" {
				unrelated = signalSafetyChild(t)
				control = fmt.Sprint(unrelated.PID)
			}
			if err := testWriteFile(c.path("tmp", "postgres-fixture-mode"), []byte(control), 0o600); err != nil {
				t.Fatal(err)
			}
			cleanupWindowsPostgresFixture(t, c)
			if err := c.startPostgres(t.Context(), m); err == nil {
				t.Fatal("PostgreSQL launch accepted a different PID or unfinished startup")
			}
			var launched, retained ProcessRecord
			if readJSON(c.path("tmp", "postgres-fixture-record"), &launched) != nil || readJSON(c.path("run", "postgres.json"), &retained) != nil {
				t.Fatal("failed PostgreSQL launch discarded its published ownership evidence")
			}
			if retained.PID != launched.PID || retained.StartTime != launched.StartTime || !retained.LaunchPending {
				t.Fatal("failed PostgreSQL launch adopted a postmaster.pid identity")
			}
			if _, alive, err := processIdentity(launched.PID); err != nil || alive {
				t.Fatal("failed PostgreSQL launch left its actual process alive")
			}
			if mode == "foreign-pid" {
				requireSafetyChildAlive(t, unrelated)
			}
			if mode == "starting-descendant" {
				var descendant ProcessRecord
				if readJSON(c.path("tmp", "postgres-fixture-descendant"), &descendant) != nil {
					t.Fatal("PostgreSQL fixture did not create its pre-readiness descendant")
				}
				handle, alive, err := openVerifiedWindowsProcess(descendant, 0)
				if err != nil {
					t.Fatal("cannot verify PostgreSQL descendant cleanup")
				}
				if alive {
					defer func() { _ = windows.CloseHandle(handle) }()
					if state, err := windows.WaitForSingleObject(handle, 5000); err != nil || state != windows.WAIT_OBJECT_0 {
						t.Fatal("failed PostgreSQL launch left its pre-readiness descendant alive")
					}
				}
			}
		})
	}
}

func TestWindowsPostgresReadinessRefusesChangedStaleIdentity(t *testing.T) {
	c, _ := unitCell(t)
	if err := testMkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := []byte(fmt.Sprintf("123\r\n%s\r\n1\r\n21101\r\n\r\n127.0.0.1\r\n\r\nready\r\n", c.path("pg")))
	changed := []byte(fmt.Sprintf("789\r\n%s\r\n2\r\n21101\r\n\r\n127.0.0.1\r\n\r\nready\r\n", c.path("pg")))
	if err := testWriteFile(c.path("pg", "postmaster.pid"), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if ready, err := c.windowsPostgresReady(ProcessRecord{PID: 456}, previous); err == nil || ready {
		t.Fatal("stale snapshot exception suppressed a changed conflicting identity")
	}
}

func TestWindowsRepairReconcilesPartialPendingPostmaster(t *testing.T) {
	for _, part := range []string{"empty", "pid", "directory", "epoch"} {
		t.Run(part, func(t *testing.T) {
			c, _ := unitCell(t)
			if err := testMkdir(c.path("pg"), 0o700); err != nil {
				t.Fatal(err)
			}
			record := signalSafetyChild(t)
			record.Root, record.Component, record.LaunchPending = c.cfg.Root, "postgres", true
			handle, alive, err := openVerifiedWindowsProcess(record, windows.PROCESS_TERMINATE)
			if err != nil || !alive {
				t.Fatal("cannot retain partial postmaster witness")
			}
			defer func() { _ = windows.CloseHandle(handle) }()
			if windows.TerminateProcess(handle, 0) != nil {
				t.Fatal("cannot simulate postmaster crash")
			}
			assertWindowsProcessExited(t, handle)
			partial := ""
			if part != "empty" {
				partial = fmt.Sprintf("%d\r\n", record.PID)
			}
			if part == "directory" || part == "epoch" {
				partial += c.path("pg") + "\r\n"
			}
			if part == "epoch" {
				partial += "1"
			}
			if writeAtomic(c.path("run", "postgres.json"), record, nil) != nil || testWriteFile(c.path("pg", "postmaster.pid"), []byte(partial), 0o600) != nil || c.journal("start", "operation", "started") != nil || c.journal("start", "postgres", "started") != nil {
				t.Fatal("cannot publish partial postmaster crash state")
			}
			if err := c.Repair(t.Context()); err != nil {
				t.Fatal("Repair could not reconcile prepublished dead postmaster identity")
			}
			progress, err := c.readJournal()
			if err != nil || progress.pending != nil {
				t.Fatal("partial pidfile left launch journal interrupted")
			}
			data, err := readPrivateFile(c.path("pg", "postmaster.pid"))
			if err != nil || string(data) != partial {
				t.Fatal("Repair modified the retained partial PostgreSQL pidfile")
			}
			if _, err := c.windowsStalePostmaster(); err != nil {
				t.Fatal("verified partial pidfile prevents a subsequent launch")
			}
		})
	}
}

func TestWindowsPendingPostmasterRefusesConflictingPartialIdentity(t *testing.T) {
	c, _ := unitCell(t)
	record := signalSafetyChild(t)
	record.Root, record.Component, record.LaunchPending = c.cfg.Root, "postgres", true
	if err := writeAtomic(c.path("run", "postgres.json"), record, nil); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"0\n", fmt.Sprintf("%d\nforeign-directory", record.PID)} {
		if _, handled, err := c.pendingPostmasterPID([]byte(data)); !handled || err == nil {
			t.Fatal("pending fence authorized a conflicting partial pidfile")
		}
		requireSafetyChildAlive(t, record)
	}
}
