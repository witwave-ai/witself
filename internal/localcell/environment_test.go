package localcell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func buildEnvironmentNamesChild(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "names.go")
	const child = `package main
import ("fmt"; "os"; "sort"; "strings"; "time")
func main() {
  var names []string
  for _, entry := range os.Environ() { name, _, _ := strings.Cut(entry, "="); names = append(names, name) }
  sort.Strings(names)
  for _, name := range names { fmt.Println(name) }
  if len(os.Args) > 1 && os.Args[1] == "exit" { return }
  time.Sleep(time.Minute)
}
`
	if err := os.WriteFile(source, []byte(child), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, executable("environment-names"))
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("could not build environment-name test child")
	}
	return binary
}

func TestChildrenReceiveOnlyAllowlistedEnvironmentNames(t *testing.T) {
	binary := buildEnvironmentNamesChild(t)
	t.Setenv("WITSELF_PROVISION_TOKEN", "synthetic-parent-only")
	t.Setenv("LOCALCELL_PARENT_SENTINEL", "synthetic-parent-only")
	t.Setenv("PGHOST", "synthetic-parent-only")
	t.Setenv("LC_ALL", "synthetic-parent-only")
	t.Setenv("LANG", "synthetic-parent-only")
	if runtime.GOOS == "windows" {
		t.Setenv("SystemRoot", "synthetic-parent-only")
		t.Setenv("COMSPEC", "synthetic-parent-only")
		t.Setenv("USERPROFILE", "synthetic-parent-only")
	}
	for _, component := range []string{"server", "worker"} {
		t.Run(component, func(t *testing.T) {
			for _, bound := range []bool{false, true} {
				name := "without-account"
				if bound {
					name = "with-account"
				}
				t.Run(name, func(t *testing.T) {
					c, m := unitCell(t)
					c.cfg.Binaries = Binaries{PostgresDir: t.TempDir(), Server: binary, Worker: binary}
					if bound {
						m.AccountID = "acc_test"
					}
					if err := c.makePassword(); err != nil {
						t.Fatal(err)
					}
					want := []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "WITSELF_RUNTIME", "WITSELF_DATABASE_URL", "WITSELF_API_ADDR", "WITSELF_HEALTH_ADDR", "WITSELF_METRICS_ADDR", "WITSELF_LOCAL_SPOOL_DIR", "WITSELF_LOCAL_JOBS"}
					if runtime.GOOS == "windows" {
						want = append(want, "SystemRoot", "USERPROFILE")
					}
					if bound {
						want = append(want, "WITSELF_LOCAL_ACCOUNT_ID")
					}
					slices.Sort(want)
					if err := c.startChild(m, component); err != nil {
						t.Fatal(err)
					}
					var record ProcessRecord
					if err := readJSON(c.path("run", component+".json"), &record); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := cleanupEnvironmentFixture(record, 5*time.Second, processIdentity, terminateEnvironmentFixture); err != nil {
							t.Error(err)
						}
					})
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					for {
						data, err := os.ReadFile(c.path("logs", component+".log"))
						if err != nil {
							t.Fatal("cannot read child environment-name output")
						}
						if len(strings.Fields(string(data))) >= len(want) {
							got := strings.Fields(string(data))
							if !slices.Equal(got, want) || strings.Contains(string(data), "=") {
								t.Fatal("child variable names differ from the exact allowlist")
							}
							break
						}
						select {
						case <-ctx.Done():
							t.Fatal("child did not report expected variable names")
						case <-time.After(10 * time.Millisecond):
						}
					}
				})
			}
		})
	}
}

// The lifecycle launcher owns Wait (or the native process handle). Wait here
// for the recorded incarnation to disappear instead of racing its exit with
// Kill, or trying to reap a child a second time. Refuse a known reused PID.
func cleanupEnvironmentFixture(record ProcessRecord, timeout time.Duration, identity func(int) (string, bool, error), terminate func(ProcessRecord) error) error {
	current, alive, err := identity(record.PID)
	if err != nil {
		return errors.New("cannot identify environment fixture for cleanup")
	}
	if !alive || current != record.StartTime {
		return nil
	}
	deadline := time.Now().Add(timeout)
	stopErr := terminate(record)
	if errors.Is(stopErr, os.ErrProcessDone) || errors.Is(stopErr, syscall.ESRCH) {
		stopErr = nil
	}
	for {
		current, alive, err = identity(record.PID)
		if err == nil && (!alive || current != record.StartTime) {
			// Windows can also return access denied when exit wins the race.
			// Confirmed exit wins over any stale termination error.
			return nil
		}
		if !time.Now().Before(deadline) {
			if stopErr != nil {
				return errors.New("cannot terminate environment fixture for cleanup")
			}
			return errors.New("environment fixture remained alive or unverifiable after cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnvironmentFixtureCleanup(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		initialDead bool
		stopErr     error
		staysAlive  bool
		reused      bool
		waitProbes  int
	}{
		{name: "already-exited", initialDead: true},
		{name: "exits-before-kill-ESRCH", stopErr: &os.SyscallError{Syscall: "kill", Err: syscall.ESRCH}},
		{name: "exits-before-kill-process-finished", stopErr: os.ErrProcessDone},
		{name: "exits-before-kill-other-error", stopErr: os.ErrPermission},
		{name: "running-child-terminated"},
		{name: "delayed-child-exit", waitProbes: 2},
		{name: "running-child-not-terminated", staysAlive: true},
		{name: "termination-fails", staysAlive: true, stopErr: os.ErrPermission},
		{name: "false-exit-error-still-alive", staysAlive: true, stopErr: syscall.ESRCH},
		{name: "pid-reused", reused: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			record := ProcessRecord{PID: 42, StartTime: "fixture-identity"}
			terminated := false
			waitProbes := 0
			identity := func(pid int) (string, bool, error) {
				if pid != record.PID {
					t.Fatal("cleanup inspected a different process")
				}
				if fixture.reused {
					return "different-identity", true, nil
				}
				if terminated && waitProbes < fixture.waitProbes {
					waitProbes++
					return record.StartTime, true, nil
				}
				return record.StartTime, !fixture.initialDead && (!terminated || fixture.staysAlive), nil
			}
			terminate := func(got ProcessRecord) error {
				if got != record || terminated {
					t.Fatal("cleanup changed the process fence or terminated twice")
				}
				terminated = true
				return fixture.stopErr
			}
			// Zero deadlines exercise failure without sleeps. The delayed-exit
			// case must actually retry the identity probe before it can succeed.
			var timeout time.Duration
			if fixture.waitProbes > 0 {
				timeout = time.Second
			}
			err := cleanupEnvironmentFixture(record, timeout, identity, terminate)
			if (err != nil) != fixture.staysAlive {
				t.Fatal("cleanup did not distinguish an exited child from a surviving child")
			}
			if terminated == (fixture.initialDead || fixture.reused) {
				t.Fatal("cleanup did not terminate exactly the live owned child")
			}
		})
	}
}

func TestPostgresCommandsUseCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "synthetic-parent-only")
	t.Setenv("LANG", "synthetic-parent-only")
	directory := t.TempDir()
	source := filepath.Join(directory, "locale.go")
	const child = `package main
import "os"
func main() {
  if os.Getenv("LC_ALL") != "C" || os.Getenv("LANG") != "C" { os.Exit(1) }
}
`
	if err := os.WriteFile(source, []byte(child), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, executable("locale-check"))
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("could not build PostgreSQL locale-check test child")
	}
	for _, command := range []string{"postgres", "database"} {
		t.Run(command, func(t *testing.T) {
			c, m := unitCell(t)
			c.cfg.Binaries.PostgresDir = directory
			if err := c.makePassword(); err != nil {
				t.Fatal(err)
			}
			var err error
			if command == "postgres" {
				err = c.pgCommand(t.Context(), "locale-check")
			} else {
				_, err = c.databaseCommand(t.Context(), m, "locale-check")
			}
			if err != nil {
				t.Fatal("PostgreSQL tool did not receive both C locale settings")
			}
		})
	}
}

func TestPostgresConfigDisablesUnixSockets(t *testing.T) {
	c, m := unitCell(t)
	if err := testMkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.configurePostgres(m); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.path("pg", "postgresql.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(data), "\n"), "unix_socket_directories = ''") {
		t.Fatal("PostgreSQL configuration did not disable Unix sockets")
	}
}

func TestCommandsRequireExplicitBinaryPaths(t *testing.T) {
	c, _ := unitCell(t)
	for _, directory := range []string{"", "relative-postgres"} {
		c.cfg.Binaries.PostgresDir = directory
		if err := c.pgCommand(t.Context(), "pg_ctl", "--version"); err == nil || err.Error() != "explicit PostgreSQL binary directory is required" {
			t.Fatal("PostgreSQL command did not refuse before parent PATH lookup")
		}
	}
	for _, binary := range []string{"", executable("go"), filepath.Join("relative-components", executable("witself-server"))} {
		if _, err := c.reportedVersion(t.Context(), binary); err == nil || err.Error() != "component binary is required" {
			t.Fatal("version command did not refuse before parent PATH lookup")
		}
	}
}
