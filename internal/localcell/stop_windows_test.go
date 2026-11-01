//go:build windows

package localcell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Both processes have explicit release gates. The test retains pg_ctl's handle
// before releasing it, proving its exit independently of postmaster completion.
func buildWindowsPostgresStopFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "stop_fixture.go")
	const program = `package main
import ("os"; "path/filepath"; "strconv"; "time")
func main() {
  root, err := os.Getwd(); if err != nil { os.Exit(101) }
  name := "postmaster"
  if filepath.Base(os.Args[0]) == "pg_ctl.exe" { name = "pg-ctl" }
  if os.WriteFile(filepath.Join(root, "tmp", name+"-ready"), []byte(strconv.Itoa(os.Getpid())), 0600) != nil { os.Exit(102) }
  deadline := time.Now().Add(30*time.Second)
  for {
    if _, err := os.Stat(filepath.Join(root, "tmp", name+"-release")); err == nil { return }
    if time.Now().After(deadline) { os.Exit(103) }
    time.Sleep(5*time.Millisecond)
  }
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal("stop fixture source creation failed")
	}
	binary := filepath.Join(directory, "postmaster.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	build.Stdout, build.Stderr = io.Discard, io.Discard
	if err := build.Run(); err != nil {
		t.Fatal("stop fixture executable build failed")
	}
	data, err := os.ReadFile(binary)
	if err != nil || os.WriteFile(filepath.Join(directory, "pg_ctl.exe"), data, 0o700) != nil {
		t.Fatal("stop fixture pg_ctl preparation failed")
	}
	return directory
}

func windowsPostgresStopFixture(t *testing.T, directory string) (*Cell, ProcessState, windows.Handle) {
	t.Helper()
	c, _ := unitCell(t)
	c.cfg.Binaries.PostgresDir = directory
	c.cfg.Timeout = 10 * time.Second
	if err := testMkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal("stop fixture data directory creation failed")
	}
	environment, err := c.baseEnvironment()
	if err != nil {
		t.Fatal("stop fixture private environment creation failed")
	}
	cmd := exec.Command(filepath.Join(directory, "postmaster.exe"))
	cmd.Env, cmd.Dir = environment, c.cfg.Root
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal("stop fixture postmaster launch failed")
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	waitWindowsControlFile(t, c.path("tmp", "postmaster-ready"))
	identity, alive, err := processIdentity(cmd.Process.Pid)
	if err != nil || !alive {
		t.Fatal("stop fixture process identity unavailable")
	}
	record := ProcessRecord{PID: cmd.Process.Pid, StartTime: identity, Root: c.cfg.Root, Component: "postgres"}
	if err := writeAtomic(c.path("run", "postgres.json"), record, nil); err != nil {
		t.Fatal("stop fixture ownership publication failed")
	}
	pidfile := fmt.Sprintf("%d\r\n%s\r\n1\r\n", record.PID, c.path("pg"))
	if err := testWriteFile(c.path("pg", "postmaster.pid"), []byte(pidfile), 0o600); err != nil {
		t.Fatal("stop fixture postmaster publication failed")
	}
	handle, alive, err := openVerifiedWindowsProcess(record, 0)
	if err != nil || !alive {
		t.Fatal("stop fixture process handle unavailable")
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return c, ProcessState{PID: record.PID, Alive: true, Matches: true}, handle
}

func releaseWindowsPostgresStopFixture(t *testing.T, c *Cell, name string) {
	t.Helper()
	if err := testWriteFile(c.path("tmp", name+"-release"), []byte("release"), 0o600); err != nil {
		t.Fatal("stop fixture release publication failed")
	}
}

func retainWindowsPostgresStopCommand(t *testing.T, c *Cell) windows.Handle {
	t.Helper()
	waitWindowsControlFile(t, c.path("tmp", "pg-ctl-ready"))
	data, err := os.ReadFile(c.path("tmp", "pg-ctl-ready"))
	if err != nil {
		t.Fatal("stop fixture pg_ctl marker unavailable")
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil || pid <= 1 {
		t.Fatal("stop fixture pg_ctl marker invalid")
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal("stop fixture pg_ctl handle unavailable")
	}
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
	return handle
}

func TestWindowsPostgresStopCompletion(t *testing.T) {
	directory := buildWindowsPostgresStopFixture(t)
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "waits-for-postmaster", true: "uses-remaining-timeout"}[timeout], func(t *testing.T) {
			c, state, handle := windowsPostgresStopFixture(t, directory)
			if timeout {
				c.cfg.Timeout = 5 * time.Second
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stopErr error
			done := make(chan struct{})
			started := time.Now()
			go func() {
				stopErr = c.stopPostgres(ctx, state)
				close(done)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("stop fixture cancellation did not finish")
				}
			})
			command := retainWindowsPostgresStopCommand(t, c)
			if timeout {
				// Spend most of the budget inside pg_ctl. A fresh five-second
				// wait after its exit must fail the six-second outer deadline.
				time.Sleep(max(0, 3*time.Second-time.Since(started)))
			}
			releaseWindowsPostgresStopFixture(t, c, "pg-ctl")
			if result, err := windows.WaitForSingleObject(command, 1000); err != nil || result != windows.WAIT_OBJECT_0 {
				t.Fatal("stop fixture pg_ctl did not finish independently")
			}
			select {
			case <-done:
				t.Fatal("stop returned before its retained postmaster exited")
			case <-time.After(100 * time.Millisecond):
			}
			if !timeout {
				releaseWindowsPostgresStopFixture(t, c, "postmaster")
			}
			select {
			case <-done:
			case <-time.After(max(0, 6*time.Second-time.Since(started))):
				t.Fatal("stop exceeded the shared command and exit deadline")
			}
			result, err := windows.WaitForSingleObject(handle, 0)
			if err != nil {
				t.Fatal("stop fixture final process observation failed")
			}
			if timeout {
				var cause fixedCause
				if !errors.As(stopErr, &cause) || cause.Error() != "PostgreSQL did not exit after stop" {
					t.Fatal("stop timeout did not preserve its fixed cause")
				}
				if result != uint32(windows.WAIT_TIMEOUT) {
					t.Fatal("stop timeout changed the retained postmaster state")
				}
			} else {
				if stopErr != nil {
					t.Fatal("stop rejected confirmed postmaster completion")
				}
				if result != windows.WAIT_OBJECT_0 {
					t.Fatal("stop succeeded without postmaster process completion")
				}
			}
		})
	}
	t.Run("identity-mismatch", func(t *testing.T) {
		c, state, handle := windowsPostgresStopFixture(t, directory)
		var record ProcessRecord
		if err := readJSON(c.path("run", "postgres.json"), &record); err != nil {
			t.Fatal("stop fixture ownership record unavailable")
		}
		record.StartTime = "mismatched-creation-identity"
		if err := writeAtomic(c.path("run", "postgres.json"), record, nil); err != nil {
			t.Fatal("stop fixture mismatched ownership publication failed")
		}
		if err := c.stopPostgres(t.Context(), state); err == nil {
			t.Fatal("stop accepted a mismatched postmaster creation identity")
		}
		if _, err := os.Stat(c.path("tmp", "pg-ctl-ready")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("stop launched pg_ctl for mismatched process ownership")
		}
		if result, err := windows.WaitForSingleObject(handle, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
			t.Fatal("stop changed the mismatched postmaster process state")
		}
	})
}
