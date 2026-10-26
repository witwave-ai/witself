package localcell_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// This suite verifies real process and HTTP supervision with synthetic PG
// peers. It deliberately makes no claim about PostgreSQL storage or migration.
func TestHermeticLifecycle(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("LifecycleOrderAndRemoval", func(t *testing.T) { testLifecycleOrderAndRemoval(t, binaries) })
	t.Run("TwoCellsAreIndependent", func(t *testing.T) { testTwoCellsAreIndependent(t, binaries, nil) })
	t.Run("ConcurrentStartSameRoot", func(t *testing.T) { testConcurrentStartSameRoot(t, binaries) })
	t.Run("ConcurrentStartIndependentRoots", func(t *testing.T) { testConcurrentStartIndependentRoots(t, binaries) })
	t.Run("InterruptedStartRepair", func(t *testing.T) { testInterruptedStartRepair(t, binaries, nil) })
	t.Run("ReadinessFailureRollsBack", func(t *testing.T) { testWaitsForReadiness(t, binaries, nil) })
	t.Run("MigrationWaitExtendsServerReadiness", func(t *testing.T) { testMigrationWaitReadiness(t, binaries) })
	t.Run("EarlyExitCauseIsJournaled", func(t *testing.T) { testEarlyExitCause(t, binaries) })
}

const concurrentStartTimeout = 5 * time.Second

// Block after a durable startup step while Start still owns its root lock. The
// channel barrier proves overlap independently of scheduler or process timing.
func startBlockedAfterPostgres(t *testing.T, binaries localcell.Binaries, root string) (*localcell.Cell, <-chan struct{}, <-chan error, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*concurrentStartTimeout)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	cell, err := localcell.New(localcell.Config{
		Root: root, Binaries: binaries, Timeout: concurrentStartTimeout,
		Failpoint: func(operation, step string) error {
			if operation == "start" && step == "postgres" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	})
	if err != nil {
		cancel()
		t.Fatal("cannot construct concurrent lifecycle fixture")
	}
	done := make(chan error, 1)
	go func() {
		done <- cell.Start(ctx)
		close(done)
	}()
	// This cleanup is registered after disposableCell's cleanup, so no fixture
	// child or lifecycle operation can remain active when root removal begins.
	t.Cleanup(func() {
		unblock()
		cancel()
		<-done
	})
	return cell, entered, done, unblock
}

func awaitConcurrentStart(t *testing.T, entered <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("start ended before owning the startup barrier: %v", err)
	case <-time.After(2 * concurrentStartTimeout):
		t.Fatal("start did not reach the startup barrier within its fixture deadline")
	}
}

func testConcurrentStartSameRoot(t *testing.T, binaries localcell.Binaries) {
	installed, root := disposableCell(t, binaries, nil)
	if err := installed.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("synthetic install: %v", err)
	}
	first, entered, done, release := startBlockedAfterPostgres(t, binaries, root)
	awaitConcurrentStart(t, entered, done)
	contender, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: concurrentStartTimeout})
	if err != nil {
		t.Fatal("cannot construct competing lifecycle handle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), concurrentStartTimeout)
	defer cancel()
	if err := contender.Start(ctx); !errors.Is(err, localcell.ErrLocked) {
		t.Fatalf("concurrent same-root Start = %v, want ErrLocked", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("first same-root Start did not finish after release: %v", err)
	}
	assertReady(t, first)
}

func testConcurrentStartIndependentRoots(t *testing.T, binaries localcell.Binaries) {
	firstInstalled, firstRoot := disposableCell(t, binaries, nil)
	secondInstalled, secondRoot := disposableCell(t, binaries, nil)
	for _, cell := range []*localcell.Cell{firstInstalled, secondInstalled} {
		if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
			t.Fatalf("synthetic install: %v", err)
		}
	}
	first, firstEntered, firstDone, releaseFirst := startBlockedAfterPostgres(t, binaries, firstRoot)
	second, secondEntered, secondDone, releaseSecond := startBlockedAfterPostgres(t, binaries, secondRoot)
	// Both roots must reach the held-lock barrier before either is released.
	// A process-global lifecycle lock would prevent the second arrival.
	awaitConcurrentStart(t, firstEntered, firstDone)
	awaitConcurrentStart(t, secondEntered, secondDone)
	releaseFirst()
	releaseSecond()
	for _, done := range []<-chan error{firstDone, secondDone} {
		if err := <-done; err != nil {
			t.Fatalf("independent concurrent Start did not finish: %v", err)
		}
	}
	assertReady(t, first)
	assertReady(t, second)
}

func testMigrationWaitReadiness(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("synthetic install: %v", err)
	}
	const timeout = 2 * time.Second
	const migrationWait = 2 * time.Second
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-server-delay"), []byte((timeout + timeout/2).String())); err != nil {
		t.Fatal("cannot configure synthetic migration delay")
	}
	cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: timeout, MigrationWait: migrationWait, JobRestartPolicy: "on-failure"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("server migration did not receive its composed readiness deadline: %v", err)
	}
	manifest := assertReady(t, cell)
	if manifest.MigrationWait != migrationWait || manifest.JobRestartPolicy != "on-failure" {
		t.Fatal("runtime settings were not persisted")
	}
	// A dead process must never remain ready, even after a previously good probe.
	status, err := cell.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(status.Processes["server"].PID)
	if err != nil || process.Kill() != nil {
		t.Fatal("cannot stop synthetic server")
	}
	_ = process.Release()
	for deadline := time.Now().Add(timeout); ; {
		status, err := cell.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !status.Processes["server"].Alive {
			if status.ServerReady {
				t.Fatal("exited server remained ready")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic server did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testEarlyExitCause(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("synthetic install: %v", err)
	}
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-server-exit"), nil); err != nil {
		t.Fatal("cannot configure synthetic server exit")
	}
	err := cell.Start(t.Context())
	const cause = "local cell server exited before readiness (exit status 7): witself-server: migration wait exceeded 250ms during advisory lock"
	if err == nil || !strings.Contains(err.Error(), cause) || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("early exit omitted its bounded value-free cause: %v", err)
	}
	component, fixedCause, state, observationFailed, ok := localcell.ReadinessFailureForTest(err)
	if !ok || component != "server" || fixedCause != "local cell server exited before readiness" || state.PID <= 1 || state.Alive || state.Matches || observationFailed {
		t.Fatal("early exit detail lost the pre-rollback process observation")
	}
	journal, err := os.ReadFile(filepath.Join(root, "journal", "operations.jsonl"))
	if err != nil {
		t.Fatal("cannot read synthetic journal")
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(journal)), "\n") {
		var entry struct{ Step, State, Cause string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal("cannot decode synthetic journal")
		}
		if entry.Step == "server_ready" && entry.State == "failed" && entry.Cause == cause {
			found = true
		}
	}
	if !found || strings.Contains(string(journal), "synthetic-private") {
		t.Fatal("journal did not retain exactly the safe readiness cause")
	}
	assertStopped(t, cell)
}

func buildLifecycleFixture(t *testing.T) localcell.Binaries {
	t.Helper()
	buildDir := t.TempDir()
	fixture := filepath.Join(buildDir, lifecycleExecutableName("fixture"))
	cmd := offlineBuildCommand(t.Context(), "build", "-o", fixture, "./testdata/lifecyclefixture")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot build synthetic lifecycle fixture")
	}
	for _, name := range []string{"postgres", "initdb", "pg_ctl", "createdb", "psql", "witself-server", "witself-worker"} {
		if err := os.Link(fixture, filepath.Join(buildDir, lifecycleExecutableName(name))); err != nil {
			t.Fatal("cannot prepare synthetic lifecycle tool")
		}
	}
	return localcell.Binaries{PostgresDir: buildDir, Server: filepath.Join(buildDir, lifecycleExecutableName("witself-server")), Worker: filepath.Join(buildDir, lifecycleExecutableName("witself-worker"))}
}

func testLifecycleOrderAndRemoval(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("synthetic install: %v", err)
	}
	assertStopped(t, cell)
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || inspection.Manifest.Database != "witself" {
		t.Fatal("synthetic install did not record the witself database")
	}
	created, err := os.ReadFile(filepath.Join(root, "pg", "fixture-created-database"))
	if err != nil || string(created) != inspection.Manifest.Database {
		t.Fatal("synthetic install did not create the manifest's witself database")
	}
	if err := lifecycleWriteFile(filepath.Join(root, "fixture-events"), nil); err != nil {
		t.Fatal("cannot reset synthetic operation log")
	}
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("synthetic start: %v", err)
	}
	assertReady(t, cell)
	assertFixtureEvents(t, root, "postgres-start\nserver-start\nworker-start\n")
	if err := cell.Stop(t.Context()); err != nil {
		t.Fatalf("synthetic stop: %v", err)
	}
	assertStopped(t, cell)
	assertFixtureEvents(t, root, "postgres-start\nserver-start\nworker-start\nworker-stop\nserver-stop\npostgres-stop\n")
	if err := cell.Start(t.Context()); err != nil {
		t.Fatalf("synthetic restart: %v", err)
	}
	assertReady(t, cell)
	if err := cell.Remove(t.Context(), localcell.RemoveOptions{}); err != nil {
		t.Fatalf("synthetic remove: %v", err)
	}
	for _, name := range []string{"pg", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("non-purge removal deleted retained %s", name)
		}
	}
	for _, name := range []string{"run", "spool", "journal", "tmp", "logs"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removal retained disposable %s", name)
		}
	}
	if err := cell.Remove(t.Context(), localcell.RemoveOptions{Purge: true}); err != nil {
		t.Fatalf("synthetic purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "pg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("explicit purge retained data")
	}
}

func assertFixtureEvents(t *testing.T, root, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "fixture-events"))
	if err != nil || strings.TrimSpace(string(data)) != strings.TrimSpace(want) {
		t.Fatal("synthetic components ran outside the required lifecycle ordering")
	}
}
