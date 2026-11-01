package localcell_test

import (
	"encoding/json"
	"fmt" // FLAKE-3 fix 5 (W1)
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings" // FLAKE-3 fix 5 (W1)
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

func TestFixtureEmptyPostgresRecordPublication(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("NeverRelease", func(t *testing.T) { // FLAKE-3 fix 4 (F9)
		cell, root := disposableCell(t, binaries, nil)                                // FLAKE-3 fix 4 (F9)
		if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil { // FLAKE-3 fix 4 (F9)
			t.Fatal("pubhold-a01 fixture install failed") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if err := lifecycleWriteFile(filepath.Join(root, "fixture-postgres-empty-record"), nil); err != nil { // FLAKE-3 fix 4 (F9)
			t.Fatal("pubhold-a02 empty record setup failed") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		command := exec.CommandContext(t.Context(), filepath.Join(binaries.PostgresDir, lifecycleExecutableName("postgres")), "-D", filepath.Join(root, "pg"), "--fixture-publication-gate", "held") // FLAKE-3 fix 4 (F9)
		command.Dir, command.Env = root, []string{}                                                                                                                                                  // FLAKE-3 fix 4 (F9)
		command.Stdout, command.Stderr = io.Discard, io.Discard                                                                                                                                      // FLAKE-3 fix 4 (F9)
		if err := command.Start(); err != nil {                                                                                                                                                      // FLAKE-3 fix 4 (F9)
			t.Fatal("pubhold-a03 fixture launch failed") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		done := make(chan struct{}) // FLAKE-3 fix 4 (F9)
		var commandErr error        // FLAKE-3 fix 4 (F9)
		go func() {                 // FLAKE-3 fix 4 (F9)
			commandErr = command.Wait() // FLAKE-3 fix 4 (F9)
			close(done)                 // FLAKE-3 fix 4 (F9)
		}() // FLAKE-3 fix 4 (F9)
		pidfile := filepath.Join(root, "pg", "postmaster.pid") // FLAKE-3 fix 4 (F9)
		t.Cleanup(func() {                                     // FLAKE-3 fix 4 (F9)
			select { // FLAKE-3 fix 4 (F9)
			case <-done: // FLAKE-3 fix 4 (F9)
			default: // FLAKE-3 fix 4 (F9)
				_ = command.Process.Kill() // FLAKE-3 fix 4 (F9)
				<-done                     // FLAKE-3 fix 4 (F9)
			} // FLAKE-3 fix 4 (F9)
			if err := os.Remove(pidfile); err != nil && !os.IsNotExist(err) { // FLAKE-3 fix 4 (F9)
				t.Error("pubhold-a04 fixture record cleanup failed") // FLAKE-3 fix 4 (F9)
			} // FLAKE-3 fix 4 (F9)
		}) // FLAKE-3 fix 4 (F9)
		// Match the fixture's unchanged publication deadline; wait for exit, not elapsed-time proof. // FLAKE-3 fix 4 (F9)
		const fixturePublicationTimeout = 5 * time.Second // FLAKE-3 fix 4 (F9)
		select {                                          // FLAKE-3 fix 4 (F9)
		case <-done: // FLAKE-3 fix 4 (F9)
		case <-time.After(2 * fixturePublicationTimeout): // FLAKE-3 fix 4 (F9)
			t.Fatal("pubhold-a05 fixture did not exit within its deadline bound") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if marker, err := os.ReadFile(filepath.Join(root, "fixture-postgres-publication-expired")); err != nil || len(marker) != 0 { // FLAKE-3 fix 4 (F9)
			t.Error("pubhold-a06 empty gate expiry marker missing") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if exitErr, ok := commandErr.(*exec.ExitError); !ok || exitErr.ExitCode() <= 0 { // FLAKE-3 fix 4 (F9)
			t.Error("pubhold-a07 fixture did not exit with nonzero status") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if pending, err := os.ReadFile(pidfile + ".pending"); err != nil || len(pending) != 0 { // FLAKE-3 fix 4 (F9)
			t.Error("pubhold-a08 empty pending record missing after gate expiry") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if _, err := os.Stat(pidfile); !os.IsNotExist(err) { // FLAKE-3 fix 4 (F9)
			t.Error("pubhold-a09 final record appeared without gate release") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
		if _, err := os.Stat(filepath.Join(root, "fixture-postgres-publication-release")); !os.IsNotExist(err) { // FLAKE-3 fix 4 (F9)
			t.Error("pubhold-a10 release file appeared in never release control") // FLAKE-3 fix 4 (F9)
		} // FLAKE-3 fix 4 (F9)
	}) // FLAKE-3 fix 4 (F9)
	t.Run("Released", func(t *testing.T) { // FLAKE-3 fix 4 (F9)
		testFixtureEmptyPostgresRecordPublicationReleased(t, binaries) // FLAKE-3 fix 4 (F9)
	}) // FLAKE-3 fix 4 (F9)
} // FLAKE-3 fix 4 (F9)
func testFixtureEmptyPostgresRecordPublicationReleased(t *testing.T, binaries localcell.Binaries) { // FLAKE-3 fix 4 (F9)
	t.Helper() // FLAKE-3 fix 4 (F9)
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("publication fixture install failed: %v", err)
	}
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-postgres-empty-record"), nil); err != nil {
		t.Fatal("cannot stage publication fixture empty record")
	}
	// Launch this one fixture directly so the gate argument stays outside the
	// product's deliberately fixed PostgreSQL arguments and environment.
	command := exec.CommandContext(t.Context(), filepath.Join(binaries.PostgresDir, lifecycleExecutableName("postgres")), "-D", filepath.Join(root, "pg"), "--fixture-publication-gate", "held")
	command.Dir, command.Env = root, []string{}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("cannot start gated publication fixture")
	}
	done := make(chan struct{})
	var commandErr error
	go func() {
		commandErr = command.Wait()
		close(done)
	}()
	pidfile := filepath.Join(root, "pg", "postmaster.pid")
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = command.Process.Kill()
			<-done
		}
		if err := os.Remove(pidfile); err != nil && !os.IsNotExist(err) {
			t.Error("cannot remove publication fixture empty record")
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
			t.Fatal("publication gate exposed the final record before release")
		}
		if staged, err := os.ReadFile(pidfile + ".pending"); err == nil {
			if len(staged) != 0 {
				t.Fatal("publication gate staged a nonempty negative record")
			}
			break
		} else if !os.IsNotExist(err) {
			t.Fatal("cannot read publication fixture pending record")
		}
		select {
		case <-done:
			t.Fatal("publication fixture exited before staging its pending record")
		default:
		}
		if !time.Now().Before(deadline) {
			t.Fatal("publication fixture did not stage its pending record")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-postgres-publication-release"), nil); err != nil {
		t.Fatal("cannot release publication fixture gate")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication fixture did not exit after gate release")
	}
	if commandErr != nil {
		t.Fatal("publication fixture failed after gate release")
	}
	if published, err := os.ReadFile(pidfile); err != nil || len(published) != 0 {
		t.Fatal("publication fixture did not publish its empty final record")
	}
	if _, err := os.Stat(pidfile + ".pending"); !os.IsNotExist(err) {
		t.Fatal("publication fixture retained its pending name after rename")
	}
	// Retain the product's exact returned and journaled cause assertions after
	// proving the fixture's publication protocol independently of launch speed.
	testEmptyPostgresRecordCause(t, binaries)
}

func TestFixturePostgresStopWaitsForExit(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("CellStop", func(t *testing.T) {
		testFixturePostgresStopWaitsForExit(t, binaries, false, false) // FLAKE-3 fix 5 (W1)
	})
	t.Run("PGCtlStop", func(t *testing.T) {
		testFixturePostgresStopWaitsForExit(t, binaries, true, false) // FLAKE-3 fix 5 (W1)
	})
}

func testFixturePostgresStopWaitsForExit(t *testing.T, binaries localcell.Binaries, direct, logicalPWD bool) { // FLAKE-3 fix 5 (W1)
	t.Helper()
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("delayed exit fixture install failed: %v", err)
	}
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-postgres-exit-delay"), nil); err != nil {
		t.Fatal("cannot stage delayed postmaster exit fixture")
	}
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("delayed exit fixture startup failed: %v", err)
	}
	var previous localcell.ProcessRecord
	data, err := os.ReadFile(filepath.Join(root, "run", "postgres.json"))
	if err != nil || json.Unmarshal(data, &previous) != nil || previous.PID <= 1 {
		t.Fatal("cannot retain delayed postmaster ownership record")
	}
	if !filepath.IsAbs(previous.Root) { // FLAKE-3 fix 5 (W1)
		t.Fatal("stoproot-a01 published fixture root is not absolute") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	if logicalPWD && previous.Root == root { // FLAKE-3 fix 5 (W1)
		t.Fatal("stoproot-a02 aliased fixture root did not diverge") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	var stderr fixtureStopStderr // FLAKE-3 fix 5 (W1)
	done := make(chan struct{})
	var stopErr error
	go func() {
		if direct {
			command := exec.CommandContext(t.Context(), filepath.Join(binaries.PostgresDir, lifecycleExecutableName("pg_ctl")), "stop", "-D", filepath.Join(previous.Root, "pg"), "-w", "-t", "5") // FLAKE-3 fix 5 (W1)
			command.Dir, command.Env = previous.Root, []string{}                                                                                                                                   // FLAKE-3 fix 5 (W1)
			if logicalPWD {                                                                                                                                                                        // FLAKE-3 fix 5 (W1)
				// Unix Getwd otherwise resolves aliases even when Dir uses one. // FLAKE-3 fix 5 (W1)
				command.Env = []string{"PWD=" + command.Dir} // FLAKE-3 fix 5 (W1)
			} // FLAKE-3 fix 5 (W1)
			command.Stdout, command.Stderr = io.Discard, &stderr // FLAKE-3 fix 5 (W1)
			stopErr = command.Run()
		} else {
			stopErr = cell.Stop(t.Context())
		}
		close(done)
	}()
	t.Cleanup(func() {
		<-done
		// A premature-return mutant must not make the normal cell cleanup race
		// this fixture's intentional delayed exit and lose its other children.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if alive, err := localcell.RecordAliveForTest(previous); err == nil && !alive {
				return
			}
		}
		t.Error("delayed postmaster did not exit before fixture cleanup")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("postmaster stop did not finish within its fixture deadline")
	}
	if stopErr != nil {
		if direct { // FLAKE-3 fix 5 (W1)
			exitCode := -1                                    // FLAKE-3 fix 5 (W1)
			if exitErr, ok := stopErr.(*exec.ExitError); ok { // FLAKE-3 fix 5 (W1)
				exitCode = exitErr.ExitCode() // FLAKE-3 fix 5 (W1)
			} // FLAKE-3 fix 5 (W1)
			reportFixtureDirectStopFailure(t, exitCode, stderr.String()) // FLAKE-3 fix 5 (W1)
		} // FLAKE-3 fix 5 (W1)
		t.Fatal("cellstop-a01 delayed postmaster stop failed") // FLAKE-3 fix 5 (W1)
	}
	if _, err := os.Stat(filepath.Join(root, "pg", "postmaster.pid")); !os.IsNotExist(err) {
		t.Fatal("postmaster stop did not remove its published pidfile")
	}
	if alive, err := localcell.RecordAliveForTest(previous); err != nil || alive {
		t.Fatal("postmaster stop returned while its owned process was alive")
	}
	// Restart immediately, without polling or sleeping after Stop returns.
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("immediate restart after delayed postmaster stop failed: %v", err)
	}
	assertReady(t, cell)
}                               // FLAKE-3 fix 5 (W1)
type fixtureStopStderr struct { // FLAKE-3 fix 5 (W1)
	data [4096]byte // FLAKE-3 fix 5 (W1)
	used int        // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
func (b *fixtureStopStderr) Write(data []byte) (int, error) { // FLAKE-3 fix 5 (W1)
	b.used += copy(b.data[b.used:], data) // FLAKE-3 fix 5 (W1)
	return len(data), nil                 // FLAKE-3 fix 5 (W1)
}                                           // FLAKE-3 fix 5 (W1)
func (b *fixtureStopStderr) String() string { return string(b.data[:b.used]) } // FLAKE-3 fix 5 (W1)
type fixtureStopReporter interface {        // FLAKE-3 fix 5 (W1)
	Fatal(...any)          // FLAKE-3 fix 5 (W1)
	Fatalf(string, ...any) // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
func reportFixtureDirectStopFailure(t fixtureStopReporter, exitCode int, stderr string) { // FLAKE-3 fix 5 (W1)
	switch { // FLAKE-3 fix 5 (W1)
	case exitCode == 124: // FLAKE-3 fix 5 (W1)
		t.Fatal("helper console attach failed during direct stop") // FLAKE-3 fix 5 (W1)
	case exitCode == 125: // FLAKE-3 fix 5 (W1)
		t.Fatal("helper identity mismatch during direct stop") // FLAKE-3 fix 5 (W1)
	case strings.Contains(stderr, "fixture PostgreSQL shutdown timeout"): // FLAKE-3 fix 5 (W1)
		t.Fatal("fixture shutdown timeout during direct stop") // FLAKE-3 fix 5 (W1)
	case strings.Contains(stderr, "fixture process creation identity mismatch"): // FLAKE-3 fix 5 (W1)
		t.Fatal("fixture creation identity mismatch during direct stop") // FLAKE-3 fix 5 (W1)
	case strings.Contains(stderr, "fixture process identity mismatch"): // FLAKE-3 fix 5 (W1)
		t.Fatal("fixture identity mismatch during direct stop") // FLAKE-3 fix 5 (W1)
	default: // FLAKE-3 fix 5 (W1)
		t.Fatalf("unclassified direct stop failure %d", exitCode) // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
}                                                             // FLAKE-3 fix 5 (W1)
type fixtureStopRecordingReporter struct{ messages []string } // FLAKE-3 fix 5 (W1)
func (r *fixtureStopRecordingReporter) Fatal(args ...any) { // FLAKE-3 fix 5 (W1)
	r.messages = append(r.messages, fmt.Sprint(args...)) // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
func (r *fixtureStopRecordingReporter) Fatalf(format string, args ...any) { // FLAKE-3 fix 5 (W1)
	r.messages = append(r.messages, fmt.Sprintf(format, args...)) // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
func TestFixtureDirectStopDiagnostics(t *testing.T) { // FLAKE-3 fix 5 (W1)
	for _, tc := range []struct { // FLAKE-3 fix 5 (W1)
		name         string // FLAKE-3 fix 5 (W1)
		code         int    // FLAKE-3 fix 5 (W1)
		stderr, want string // FLAKE-3 fix 5 (W1)
	}{ // FLAKE-3 fix 5 (W1)
		{"Attach", 124, "fixture process identity mismatch", "helper console attach failed during direct stop"},                  // FLAKE-3 fix 5 (W1)
		{"HelperIdentity", 125, "fixture PostgreSQL shutdown timeout", "helper identity mismatch during direct stop"},            // FLAKE-3 fix 5 (W1)
		{"Shutdown", 1, "fixture PostgreSQL shutdown timeout\n", "fixture shutdown timeout during direct stop"},                  // FLAKE-3 fix 5 (W1)
		{"Creation", 1, "fixture process creation identity mismatch\n", "fixture creation identity mismatch during direct stop"}, // FLAKE-3 fix 5 (W1)
		{"RootIdentity", 1, "fixture process identity mismatch\n", "fixture identity mismatch during direct stop"},               // FLAKE-3 fix 5 (W1)
		{"Unknown", 9, "private fixture sentinel", "unclassified direct stop failure 9"},                                         // FLAKE-3 fix 5 (W1)
		{"Launch", -1, "private fixture sentinel", "unclassified direct stop failure -1"},                                        // FLAKE-3 fix 5 (W1)
	} { // FLAKE-3 fix 5 (W1)
		t.Run(tc.name, func(t *testing.T) { // FLAKE-3 fix 5 (W1)
			var reporter fixtureStopRecordingReporter                           // FLAKE-3 fix 5 (W1)
			reportFixtureDirectStopFailure(&reporter, tc.code, tc.stderr)       // FLAKE-3 fix 5 (W1)
			if len(reporter.messages) != 1 || reporter.messages[0] != tc.want { // FLAKE-3 fix 5 (W1)
				t.Fatal("stopdiag-a01 direct stop classification changed") // FLAKE-3 fix 5 (W1)
			} // FLAKE-3 fix 5 (W1)
		}) // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
func TestFixtureStopStderrBound(t *testing.T) { // FLAKE-3 fix 5 (W1)
	var stderr fixtureStopStderr                                      // FLAKE-3 fix 5 (W1)
	first := []byte("fixture process identity mismatch\n")            // FLAKE-3 fix 5 (W1)
	if n, err := stderr.Write(first); n != len(first) || err != nil { // FLAKE-3 fix 5 (W1)
		t.Fatal("stopbuf-b001 initial diagnostic write failed") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	large := []byte(strings.Repeat("x", 8192)) // FLAKE-3 fix 5 (W1)
	for range 2 {                              // FLAKE-3 fix 5 (W1)
		if n, err := stderr.Write(large); n != len(large) || err != nil { // FLAKE-3 fix 5 (W1)
			t.Fatal("stopbuf-b002 truncated diagnostic write failed") // FLAKE-3 fix 5 (W1)
		} // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
	if got := stderr.String(); len(got) != 4096 || !strings.HasPrefix(got, string(first)) { // FLAKE-3 fix 5 (W1)
		t.Fatal("stopbuf-b003 bounded diagnostic content changed") // FLAKE-3 fix 5 (W1)
	} // FLAKE-3 fix 5 (W1)
} // FLAKE-3 fix 5 (W1)
