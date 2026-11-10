package localcell

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

const installerArgumentFixtureMarker = "installer-argument-fixture"

// The production launcher re-execs this test binary with ordinary serve
// arguments. A value-free marker in the private test root selects the fixture,
// without changing child argv or the production environment allowlist.
func init() {
	if len(os.Args) < 2 || os.Args[1] != "serve" || os.Getenv("WITSELF_LOCAL_MANAGED_PRINCIPALS") != "1" {
		return
	}
	root, err := os.Getwd()
	if err != nil {
		return
	}
	marker := filepath.Join(root, "run", installerArgumentFixtureMarker)
	if data, readErr := os.ReadFile(marker); readErr != nil || string(data) != "enabled" {
		return
	}
	result := "refused"
	if installerChildArgumentsPrivate(root) {
		result = "accepted"
	}
	if writeAtomicBytes(marker+".result", []byte(result), nil) != nil {
		os.Exit(125)
	}
	// Keep the incarnation alive for ownership-fenced parent cleanup.
	time.Sleep(time.Minute)
	os.Exit(0)
}

func installerChildArgumentsPrivate(root string) bool {
	c, err := New(Config{Root: root})
	if err != nil {
		return false
	}
	m, err := readManifest(root)
	if err != nil || m.PrincipalLayoutVersion != 1 {
		return false
	}
	seen := make(map[string]bool)
	for _, purpose := range append([]credentialPurpose{credentialBootstrap}, principalCredentialPurposes[:]...) {
		secret, secretErr := c.credentialPassword(m, purpose)
		connection, connectionErr := c.principalDSN(m, purpose)
		path, pathErr := c.credentialPath(purpose)
		if secretErr != nil || connectionErr != nil || pathErr != nil || secret == "" || connection == "" || path == "" || seen[secret] {
			return false
		}
		seen[secret] = true
		for _, argument := range os.Args {
			if strings.Contains(argument, secret) || strings.Contains(argument, connection) || strings.Contains(argument, path) {
				return false
			}
		}
	}
	return true
}

func assertInstallerChildArguments(t *testing.T, c *Cell, m *Manifest, component string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("selected argument fixture executable was unavailable")
	}
	c.cfg.Binaries.Server, c.cfg.Binaries.Worker = binary, binary
	marker := c.path("run", installerArgumentFixtureMarker)
	if err := os.Remove(marker + ".result"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal("selected argument fixture previous result could not be retired")
	}
	// The marker stays in the cell tree for later service preparation, so it
	// needs the same explicit private owner and ACL as production control files.
	if testWriteFile(marker, []byte("enabled"), 0o600) != nil {
		t.Fatal("selected argument fixture marker could not be written")
	}
	if err := c.validateRoot(false); err != nil {
		t.Fatalf("selected argument fixture did not retain a private tree%s", installerPrincipalFailureSuffix(err))
	}
	if c.startChild(m, component) != nil {
		t.Fatal("selected argument fixture child could not start")
	}
	var record ProcessRecord
	if readJSON(c.path("run", component+".json"), &record) != nil {
		t.Fatal("selected argument fixture did not publish its process fence")
	}
	t.Cleanup(func() {
		if cleanupEnvironmentFixture(record, 5*time.Second, processIdentity, terminateEnvironmentFixture) != nil {
			t.Error("selected argument fixture child could not be cleaned up")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !waitInstallerArgumentResult(ctx, t, func() ([]byte, error) {
		return os.ReadFile(marker + ".result")
	}) {
		return
	}
	if os.Remove(marker+".result") != nil {
		t.Fatal("selected argument fixture result could not be retired")
	}
}

type environmentFixtureReporter interface {
	Helper()
	Logf(string, ...any)
	Fatal(...any)
}

func waitInstallerArgumentResult(ctx context.Context, t environmentFixtureReporter, read func() ([]byte, error)) bool {
	t.Helper()
	result, readErr := readEnvironmentFixtureOutput(ctx, read)
	if readErr != nil {
		t.Logf("child fixture read class=%s", environmentFixtureReadErrorClass(readErr, runtime.GOOS))
		if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatal("selected argument fixture result was unavailable")
			return false
		}
		t.Fatal("selected argument fixture did not report its private verdict")
		return false
	}
	if string(result) != "accepted" {
		t.Fatal("selected child arguments disclosed principal material")
		return false
	}
	return true
}

// A Windows reader may race publication or a log writer's handle. Retry every
// read error only within the caller's existing context; readable invalid content
// remains an immediate assertion failure at the caller.
func readEnvironmentFixtureOutput(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	for {
		result, readErr := read()
		if readErr == nil {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, readErr
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The platform is explicit so host tests can exercise Win32 classes without
// interpreting Unix errno 5 or 32 as a Windows failure. Never log error text.
func environmentFixtureReadErrorClass(err error, goos string) string {
	if errors.Is(err, os.ErrNotExist) {
		return "not-exist"
	}
	if goos == "windows" {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			switch errno {
			case 32: // ERROR_SHARING_VIOLATION
				return "sharing-violation"
			case 5: // ERROR_ACCESS_DENIED
				return "access-denied"
			}
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return "access-denied"
	}
	return "other"
}

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
					want := []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "WITSELF_RUNTIME", "WITSELF_DATABASE_URL", "WITSELF_API_ADDR", "WITSELF_HEALTH_ADDR", "WITSELF_METRICS_ADDR", "WITSELF_LOCAL_SPOOL_DIR", "WITSELF_LOCAL_JOBS", "WITSELF_LOCAL_MIGRATION_WAIT", "WITSELF_LOCAL_JOB_RESTART_POLICY"}
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
						data, err := readEnvironmentFixtureOutput(ctx, func() ([]byte, error) {
							return os.ReadFile(c.path("logs", component+".log"))
						})
						if err != nil {
							t.Logf("child fixture read class=%s", environmentFixtureReadErrorClass(err, runtime.GOOS))
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

func TestInstallerPrincipalsChildEnvironmentNames(t *testing.T) {
	binary := buildEnvironmentNamesChild(t)
	for _, component := range []string{"server", "worker"} {
		t.Run(component, func(t *testing.T) {
			c, m, _ := selectedCredentialFixture(t)
			c.cfg.Binaries = Binaries{PostgresDir: t.TempDir(), Server: binary, Worker: binary}
			want := []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "WITSELF_RUNTIME", "WITSELF_DATABASE_URL", "WITSELF_API_ADDR", "WITSELF_HEALTH_ADDR", "WITSELF_METRICS_ADDR", "WITSELF_LOCAL_SPOOL_DIR", "WITSELF_LOCAL_JOBS", "WITSELF_LOCAL_MIGRATION_WAIT", "WITSELF_LOCAL_JOB_RESTART_POLICY", "WITSELF_LOCAL_SYNC_SCHEMA", "WITSELF_LOCAL_MANAGED_PRINCIPALS"}
			if runtime.GOOS == "windows" {
				want = append(want, "SystemRoot", "USERPROFILE")
			}
			slices.Sort(want)
			if c.startChild(m, component) != nil {
				t.Fatal("selected environment child could not start")
			}
			var record ProcessRecord
			if readJSON(c.path("run", component+".json"), &record) != nil {
				t.Fatal("selected environment child did not publish its process fence")
			}
			t.Cleanup(func() {
				if cleanupEnvironmentFixture(record, 5*time.Second, processIdentity, terminateEnvironmentFixture) != nil {
					t.Error("selected environment child could not be cleaned up")
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for {
				data, err := readEnvironmentFixtureOutput(ctx, func() ([]byte, error) {
					return os.ReadFile(c.path("logs", component+".log"))
				})
				if err != nil {
					t.Logf("child fixture read class=%s", environmentFixtureReadErrorClass(err, runtime.GOOS))
					t.Fatal("selected environment child output was unavailable")
				}
				if len(strings.Fields(string(data))) >= len(want) {
					if !slices.Equal(strings.Fields(string(data)), want) || strings.Contains(string(data), "=") {
						t.Fatal("selected environment child names differ from the exact allowlist")
					}
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("selected environment child did not report its variable names")
				case <-time.After(10 * time.Millisecond):
				}
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

type environmentFixtureRecorder struct{ events []string }

func (*environmentFixtureRecorder) Helper() {}
func (r *environmentFixtureRecorder) Logf(format string, args ...any) {
	r.events = append(r.events, fmt.Sprintf(format, args...))
}
func (r *environmentFixtureRecorder) Fatal(args ...any) {
	r.events = append(r.events, fmt.Sprint(args...))
}

func TestEnvironmentFixtureReadRetries(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"not-exist", os.ErrNotExist},
		{"sharing-violation", syscall.Errno(32)},
		{"access-denied", syscall.Errno(5)},
		{"permission", os.ErrPermission},
		{"other", errors.New("synthetic private error")},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			recorder := &environmentFixtureRecorder{}
			reads := 0
			accepted := waitInstallerArgumentResult(ctx, recorder, func() ([]byte, error) {
				reads++
				if reads < 3 {
					return []byte("refused"), &os.PathError{Op: "read", Path: "synthetic-private-path", Err: test.err}
				}
				return []byte("accepted"), nil
			})
			if !accepted || reads != 3 || len(recorder.events) != 0 {
				t.Fatal("fixture read failed before the existing context expired instead of retrying")
			}
		})
	}
}

func TestEnvironmentFixtureReadDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	reads := 0
	_, err := readEnvironmentFixtureOutput(ctx, func() ([]byte, error) {
		reads++
		return nil, os.ErrPermission
	})
	if !errors.Is(err, os.ErrPermission) || !errors.Is(ctx.Err(), context.DeadlineExceeded) || reads < 1 {
		t.Fatal("fixture read did not retain the last read error until the caller's deadline")
	}
}

func TestInstallerArgumentResultFailureDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"missing", os.ErrNotExist, "selected argument fixture did not report its private verdict"},
		{"unavailable", os.ErrPermission, "selected argument fixture result was unavailable"},
		{"other", errors.New("synthetic private error"), "selected argument fixture result was unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			recorder := &environmentFixtureRecorder{}
			if waitInstallerArgumentResult(ctx, recorder, func() ([]byte, error) {
				return nil, &os.PathError{Op: "read", Path: "synthetic-private-path", Err: test.err}
			}) {
				t.Fatal("unavailable fixture result was accepted")
			}
			want := []string{"child fixture read class=" + environmentFixtureReadErrorClass(test.err, runtime.GOOS), test.want}
			if !slices.Equal(recorder.events, want) {
				t.Fatal("fixture read did not log only its class before the unchanged fatal message")
			}
		})
	}
}

func TestInstallerArgumentResultRejectsWrongValue(t *testing.T) {
	for _, result := range []string{"", "refused", "accepted\n", "accepted extra"} {
		recorder := &environmentFixtureRecorder{}
		reads := 0
		if waitInstallerArgumentResult(t.Context(), recorder, func() ([]byte, error) {
			reads++
			if reads > 1 {
				t.Fatal("wrong fixture result was retried")
			}
			return []byte(result), nil
		}) || reads != 1 || !slices.Equal(recorder.events, []string{"selected child arguments disclosed principal material"}) {
			t.Fatal("wrong fixture result did not fail immediately with the unchanged assertion")
		}
	}
}

func TestEnvironmentFixtureReadErrorClass(t *testing.T) {
	for _, test := range []struct {
		name, goos, want string
		err              error
	}{
		{"missing", "windows", "not-exist", os.ErrNotExist},
		{"sharing", "windows", "sharing-violation", syscall.Errno(32)},
		{"access", "windows", "access-denied", syscall.Errno(5)},
		{"permission", "linux", "access-denied", os.ErrPermission},
		{"unix-io", "linux", "other", syscall.Errno(5)},
		{"unix-pipe", "linux", "other", syscall.Errno(32)},
		{"other-errno", "windows", "other", syscall.Errno(6)},
		{"other", "windows", "other", errors.New("synthetic private error")},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrapped := fmt.Errorf("synthetic private context: %w", &os.PathError{Op: "read", Path: "synthetic-private-path", Err: test.err})
			if got := environmentFixtureReadErrorClass(wrapped, test.goos); got != test.want {
				t.Fatalf("read error class = %s, want %s", got, test.want)
			}
		})
	}
}

// Keep the injected retry tests attached to all three real child-output readers.
func TestEnvironmentFixtureReadSourceContract(t *testing.T) {
	data, err := os.ReadFile("environment_test.go")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(data), "\r\n", "\n")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "environment_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		function, helper string
		bounded          bool
	}{
		{"assertInstallerChildArguments", "waitInstallerArgumentResult", true},
		{"waitInstallerArgumentResult", "readEnvironmentFixtureOutput", false},
		{"TestChildrenReceiveOnlyAllowlistedEnvironmentNames", "readEnvironmentFixtureOutput", true},
		{"TestInstallerPrincipalsChildEnvironmentNames", "readEnvironmentFixtureOutput", true},
	} {
		t.Run(test.function, func(t *testing.T) {
			var function *ast.FuncDecl
			for _, decl := range file.Decls {
				if candidate, ok := decl.(*ast.FuncDecl); ok && candidate.Name.Name == test.function {
					function = candidate
					break
				}
			}
			if function == nil {
				t.Fatal("child-output reader disappeared")
			}
			calls := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok {
					if name, ok := call.Fun.(*ast.Ident); ok && name.Name == test.helper {
						calls++
					}
				}
				return true
			})
			if calls != 1 {
				t.Fatal("child-output reader bypassed the bounded retry helper")
			}
			body := source[fset.Position(function.Pos()).Offset:fset.Position(function.End()).Offset]
			if test.bounded && !strings.Contains(body, "context.WithTimeout(t.Context(), 5*time.Second)") {
				t.Fatal("child-output reader changed its existing five-second context")
			}
		})
	}
}
