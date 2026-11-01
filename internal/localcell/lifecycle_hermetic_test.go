package localcell_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	t.Run("InterruptedPostgresRecordRepair", func(t *testing.T) { testInterruptedPostgresRecordRepair(t, binaries) })
	t.Run("ReadinessFailureRollsBack", func(t *testing.T) { testWaitsForReadiness(t, binaries, nil) })
	t.Run("MigrationWaitExtendsServerReadiness", func(t *testing.T) { testMigrationWaitReadiness(t, binaries) })
	t.Run("EarlyExitCauseIsJournaled", func(t *testing.T) { testEarlyExitCause(t, binaries) })
}

const concurrentStartTimeoutFloor = 5 * time.Second

type concurrentStartReporter interface {
	Fatalf(string, ...any)
	Errorf(string, ...any)
	Logf(string, ...any)
}

func selectConcurrentStartTimeout(elapsed time.Duration) time.Duration {
	return max(concurrentStartTimeoutFloor, 10*elapsed)
}

func TestSelectConcurrentStartTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"below floor", 100 * time.Millisecond, 5 * time.Second},
		{"crossover", 500 * time.Millisecond, 5 * time.Second},
		{"above floor", 700 * time.Millisecond, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectConcurrentStartTimeout(tc.elapsed); got != tc.want {
				t.Fatalf("concurrent start timeout selection = %s, want %s", got, tc.want)
			}
		})
	}
}

// Calibrate with the same held-lock path on one installed root. The sample
// includes a complete synthetic Start, with its barrier released immediately;
// Stop restores the root before the concurrency assertion. Multiplying the
// sample by ten tolerates scheduling variation without weakening the barrier.
func measureConcurrentStartTimeout(fixture *testing.T, t concurrentStartReporter, binaries localcell.Binaries, root string) time.Duration {
	fixture.Helper()
	started := time.Now()
	cell, entered, done, release := startBlockedAfterPostgres(fixture, binaries, root, 12*concurrentStartTimeoutFloor)
	awaitConcurrentStart(t, entered, done, 12*concurrentStartTimeoutFloor)
	barrierElapsed := time.Since(started)
	release()
	if err := <-done; err != nil {
		t.Fatalf("single-cell calibration Start did not finish: %v", err)
	}
	elapsed := time.Since(started)
	if err := cell.Stop(fixture.Context()); err != nil {
		t.Fatalf("single-cell calibration Stop did not finish: %v", err)
	}
	timeout := selectConcurrentStartTimeout(elapsed)
	if timeout < concurrentStartTimeoutFloor {
		t.Fatalf("concurrent start timeout is below its operation floor")
	}
	if timeout < 10*elapsed {
		t.Fatalf("concurrent start timeout is below its measured allowance")
	}
	if 2*timeout < 10*time.Second {
		t.Fatalf("concurrent start barrier deadline is below its floor")
	}
	t.Logf("synthetic single-cell start: barrier=%s complete=%s timeout=%s barrier_deadline=%s", barrierElapsed, elapsed, timeout, 2*timeout)
	return timeout
}

// Block after a durable startup step while Start still owns its root lock. The
// channel barrier proves overlap independently of scheduler or process timing.
func startBlockedAfterPostgres(t *testing.T, binaries localcell.Binaries, root string, timeout time.Duration) (*localcell.Cell, *concurrentStartArrival, <-chan error, func()) {
	t.Helper()
	return startBlockedAfterPostgresInOrder(t, binaries, root, timeout, nil)
}

// Only the scenario goroutine observes arrivals; the failpoint just signals.
// This distinction makes premature release fail even if a process runs early.
type concurrentStartArrival struct {
	entered  <-chan struct{}
	observed bool
}

type concurrentStartOrder struct {
	before   <-chan struct{}
	returned chan<- struct{}
}

func startBlockedAfterPostgresInOrder(t *testing.T, binaries localcell.Binaries, root string, timeout time.Duration, order *concurrentStartOrder) (*localcell.Cell, *concurrentStartArrival, <-chan error, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 4*timeout)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	cell, err := localcell.New(localcell.Config{
		Root: root, Binaries: binaries, Timeout: timeout,
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
		defer close(done)
		if order != nil && order.before != nil {
			select {
			case <-order.before:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
		done <- cell.Start(ctx)
		if order != nil && order.returned != nil {
			close(order.returned)
		}
	}()
	// This cleanup is registered after disposableCell's cleanup, so no fixture
	// child or lifecycle operation can remain active when root removal begins.
	t.Cleanup(func() {
		unblock()
		cancel()
		<-done
	})
	return cell, &concurrentStartArrival{entered: entered}, done, unblock
}

func awaitConcurrentStart(t concurrentStartReporter, arrival *concurrentStartArrival, done <-chan error, timeout time.Duration) {
	select {
	case <-arrival.entered:
		arrival.observed = true
	case err := <-done:
		t.Fatalf("start ended before owning the startup barrier: %v", err)
	case <-time.After(2 * timeout):
		t.Fatalf("start did not reach the startup barrier within its fixture deadline")
	}
}

func testConcurrentStartSameRoot(t *testing.T, binaries localcell.Binaries) {
	installed, root := disposableCell(t, binaries, nil)
	if err := installed.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("synthetic install: %v", err)
	}
	timeout := measureConcurrentStartTimeout(t, t, binaries, root)
	first, entered, done, release := startBlockedAfterPostgres(t, binaries, root, timeout)
	awaitConcurrentStart(t, entered, done, timeout)
	contender, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: timeout})
	if err != nil {
		t.Fatal("cannot construct competing lifecycle handle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
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
	t.Run("OverlappingStarts", func(t *testing.T) {
		runConcurrentStartIndependentRoots(t, t, binaries, false)
	})
	t.Run("RejectsSerializedStarts", func(t *testing.T) {
		recorded := recordConcurrentStart(func(reporter concurrentStartReporter) {
			runConcurrentStartIndependentRoots(t, reporter, binaries, true)
		})
		if len(recorded.fatal) == 0 && len(recorded.errors) == 0 {
			t.Fatal("serialized independent Start unexpectedly passed")
		}
		const want = "start did not reach the startup barrier within its fixture deadline"
		if len(recorded.fatal) != 1 || recorded.fatal[0] != want || len(recorded.errors) != 0 {
			t.Fatalf("serialized independent Start missed its expected barrier failure: fatal=%v errors=%v", recorded.fatal, recorded.errors)
		}
	})
	t.Run("RejectsPrematureSecondRelease", func(t *testing.T) {
		firstEntered, secondEntered := make(chan struct{}), make(chan struct{})
		close(secondEntered)
		var released [2]bool
		barrier := &concurrentStartBarrier{
			arrivals: [2]*concurrentStartArrival{{entered: firstEntered}, {entered: secondEntered}},
			unblock:  [2]func(){func() { released[0] = true }, func() { released[1] = true }},
		}
		t.Cleanup(barrier.cleanup)
		// Second has arrived; first cannot arrive because its channel is open
		// and no goroutine can close it. No scheduler timing enters this proof.
		awaitConcurrentStart(t, barrier.arrivals[1], nil, concurrentStartTimeoutFloor)
		recorded := recordConcurrentStart(func(reporter concurrentStartReporter) {
			barrier.release(reporter, 1)
		})
		const want = "concurrent start release requires both observed startup barriers"
		if len(recorded.fatal) != 1 || recorded.fatal[0] != want || len(recorded.errors) != 0 {
			t.Fatalf("premature second release missed its expected barrier failure: fatal=%v errors=%v", recorded.fatal, recorded.errors)
		}
		if released[0] || released[1] {
			t.Fatal("premature release unblocked a startup barrier")
		}
	})
}

type concurrentStartBarrier struct {
	arrivals [2]*concurrentStartArrival
	unblock  [2]func()
}

func (b *concurrentStartBarrier) start(t *testing.T, binaries localcell.Binaries, root string, timeout time.Duration, index int, order *concurrentStartOrder) (*localcell.Cell, *concurrentStartArrival, <-chan error) {
	t.Helper()
	cell, arrival, done, unblock := startBlockedAfterPostgresInOrder(t, binaries, root, timeout, order)
	b.arrivals[index], b.unblock[index] = arrival, unblock
	return cell, arrival, done
}

func (b *concurrentStartBarrier) release(t concurrentStartReporter, index int) {
	for _, arrival := range b.arrivals {
		if arrival == nil || !arrival.observed {
			t.Fatalf("concurrent start release requires both observed startup barriers")
			return
		}
	}
	b.unblock[index]()
}

// Unconditional release belongs only to cleanup. Release both before joining
// either Start, including when the recording reporter aborts the shared body.
func (b *concurrentStartBarrier) cleanup() {
	for _, unblock := range b.unblock {
		if unblock != nil {
			unblock()
		}
	}
}

// Test both real overlap and a deliberately serialized Start through the same
// assertions. The testing.T owns fixtures; all scenario assertions use t.
func runConcurrentStartIndependentRoots(fixture *testing.T, t concurrentStartReporter, binaries localcell.Binaries, serialized bool) {
	firstInstalled, firstRoot := disposableCell(fixture, binaries, nil)
	secondInstalled, secondRoot := disposableCell(fixture, binaries, nil)
	for _, cell := range []*localcell.Cell{firstInstalled, secondInstalled} {
		if err := cell.Install(fixture.Context(), localcell.InstallOptions{}); err != nil {
			t.Fatalf("synthetic install: %v", err)
		}
	}
	timeout := measureConcurrentStartTimeout(fixture, t, binaries, firstRoot)
	var firstOrder, secondOrder *concurrentStartOrder
	if serialized {
		firstReturned := make(chan struct{})
		firstOrder = &concurrentStartOrder{returned: firstReturned}
		secondOrder = &concurrentStartOrder{before: firstReturned}
	}
	barrier := &concurrentStartBarrier{}
	// Also covers Fatalf before both starts have registered their cleanups.
	defer barrier.cleanup()
	first, firstEntered, firstDone := barrier.start(fixture, binaries, firstRoot, timeout, 0, firstOrder)
	second, secondEntered, secondDone := barrier.start(fixture, binaries, secondRoot, timeout, 1, secondOrder)
	// Both roots must reach the held-lock barrier before either is released.
	// A process-global lifecycle lock would prevent the second arrival.
	awaitConcurrentStart(t, firstEntered, firstDone, timeout)
	awaitConcurrentStart(t, secondEntered, secondDone, timeout)
	barrier.release(t, 0)
	barrier.release(t, 1)
	for _, done := range []<-chan error{firstDone, secondDone} {
		if err := <-done; err != nil {
			t.Fatalf("independent concurrent Start did not finish: %v", err)
		}
	}
	for _, cell := range []*localcell.Cell{first, second} {
		status, err := cell.Status(fixture.Context())
		if err != nil || !status.ServerReady || !status.WorkerReady || status.Manifest == nil {
			t.Fatalf("both local readiness endpoints must be ready when start returns")
		}
	}
}

type concurrentStartFatal struct{}

type recordingConcurrentStartReporter struct {
	fatal  []string
	errors []string
}

func (r *recordingConcurrentStartReporter) Fatalf(format string, args ...any) {
	r.fatal = append(r.fatal, fmt.Sprintf(format, args...))
	panic(concurrentStartFatal{})
}

func (r *recordingConcurrentStartReporter) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (*recordingConcurrentStartReporter) Logf(string, ...any) {}

func recordConcurrentStart(body func(concurrentStartReporter)) (recorded *recordingConcurrentStartReporter) {
	recorded = &recordingConcurrentStartReporter{}
	defer func() {
		if recovered := recover(); recovered != nil {
			if _, ok := recovered.(concurrentStartFatal); !ok {
				panic(recovered)
			}
		}
	}()
	body(recorded)
	return recorded
}

func TestConcurrentStartRecordingReporter(t *testing.T) {
	continued := false
	recorded := recordConcurrentStart(func(reporter concurrentStartReporter) {
		reporter.Logf("fixture log")
		reporter.Errorf("fixture error %s", "recorded")
		reporter.Fatalf("fixture fatal %s", "recorded")
		continued = true
	})
	if continued || len(recorded.fatal) != 1 || recorded.fatal[0] != "fixture fatal recorded" ||
		len(recorded.errors) != 1 || recorded.errors[0] != "fixture error recorded" {
		t.Fatal("recording reporter did not stop and retain the scenario assertions")
	}
}

func testInterruptedPostgresRecordRepair(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, func(operation, step string) error {
		if operation == "install" && step == "postgres" {
			return errors.New("injected PostgreSQL record interruption")
		}
		return nil
	})
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err == nil {
		t.Fatal("install did not stop at the PostgreSQL interruption")
	}
	// The process is live but its durable ownership record was lost. Repair
	// must recover from the fixture's pidfile and native process creation time.
	path := filepath.Join(root, "run", "postgres.json")
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot retain interrupted PostgreSQL ownership record")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal("cannot remove interrupted PostgreSQL ownership record")
	}
	if err := cell.Repair(t.Context()); err != nil {
		// Preserve cleanup authority even when the recovery assertion fails.
		if restoreErr := lifecycleWriteFile(path, record); restoreErr != nil {
			t.Error("cannot restore interrupted PostgreSQL cleanup authority")
		}
		t.Fatalf("repair could not recover synthetic PostgreSQL ownership: %v", err)
	}
	assertStopped(t, cell)
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
