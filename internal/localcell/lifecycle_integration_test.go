package localcell_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
	"github.com/witwave-ai/witself/internal/store"
)

// One build supplies all real-lifecycle scenarios. Missing PostgreSQL is an
// explicit NOT RUN result; a present but broken toolchain is a test failure.
func TestRealLifecycle(t *testing.T) {
	pgbin := os.Getenv("WITSELF_LOCALCELL_PGBIN")
	binaries := buildRealComponents(t, pgbin)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("InstallStartRestartRemove", func(t *testing.T) { testInstallStartRestartRemove(t, binaries) })
	t.Run("TwoCellsAreIndependent", func(t *testing.T) { testTwoCellsAreIndependent(t, binaries, assertPostgresEncodingAndUnicode) })
	t.Run("InterruptedStartRepair", func(t *testing.T) { testInterruptedStartRepair(t, binaries, assertPostgresEncodingAndUnicode) })
	t.Run("WaitsForReadiness", func(t *testing.T) { testWaitsForReadiness(t, binaries, assertPostgresEncodingAndUnicode) })
}

func postgresBinariesPresent(directory string) bool {
	for _, name := range []string{"postgres", "initdb", "pg_ctl", "psql", "createdb"} {
		info, err := os.Stat(filepath.Join(directory, lifecycleExecutableName(name)))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func buildRealComponents(t *testing.T, pgbin string) localcell.Binaries {
	t.Helper()
	if pgbin == "" {
		_, _ = fmt.Fprintf(os.Stderr, "NOT RUN %s: no PostgreSQL binaries\n", t.Name())
		t.Skip("no PostgreSQL binaries")
	}
	if !postgresBinariesPresent(pgbin) {
		t.Fatal("configured PostgreSQL distribution is incomplete")
	}
	requirePostgres16(t, pgbin)
	buildDir := t.TempDir()
	source, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal("cannot resolve source checkout")
	}
	binaries := localcell.Binaries{PostgresDir: pgbin}
	for _, component := range []struct {
		name string
		path *string
	}{{"witself-server", &binaries.Server}, {"witself-worker", &binaries.Worker}} {
		*component.path = filepath.Join(buildDir, lifecycleExecutableName(component.name))
		cmd := offlineBuildCommand(t.Context(), "build", "-o", *component.path, "./cmd/"+component.name)
		cmd.Dir = source
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err != nil {
			t.Fatal("cannot build lifecycle component")
		}
	}
	return binaries
}

func offlineBuildCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	return cmd
}

func disposableCell(t *testing.T, binaries localcell.Binaries, failpoint func(string, string) error) (*localcell.Cell, string) {
	t.Helper()
	root := shortLifecycleRoot(t)
	witness := newLifecycleProcessWitness(t, root)
	observe := func(operation, step string) error {
		witness.capture(t)
		if failpoint != nil {
			return failpoint(operation, step)
		}
		return nil
	}
	cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 60 * time.Second, Failpoint: observe})
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatalf("construct cell: %v", err)
	}
	t.Cleanup(func() {
		witness.capture(t)
		defer witness.assertStopped(t)
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		cleanupCell, newErr := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 30 * time.Second})
		if newErr != nil {
			t.Error("cannot construct cleanup cell")
			return
		}
		// Recovery uses the durable fence, so even a test interrupted by
		// its own failure point cannot strand child processes.
		if repairErr := cleanupCell.Repair(ctx); repairErr != nil {
			t.Errorf("cleanup repair: %v", repairErr)
			return
		}
		// Repair can recover a postmaster's missing durable identity. Retain
		// that recovered incarnation before Remove deletes the run directory.
		witness.capture(t)
		if removeErr := cleanupCell.Remove(ctx, localcell.RemoveOptions{Purge: true}); removeErr != nil {
			t.Errorf("cleanup remove: %v", removeErr)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error("cannot remove disposable cell root")
		}
	})
	return cell, root
}

func testInstallStartRestartRemove(t *testing.T, binaries localcell.Binaries) {
	cell, root := disposableCell(t, binaries, nil)
	ctx := t.Context()
	if err := cell.Install(ctx, localcell.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	assertStopped(t, cell)
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start: %v", err)
	}
	manifest := assertReady(t, cell)
	account := readStoreState(t, root, manifest)
	assertPostgresEncodingAndUnicode(t, root, manifest)
	if manifest.LastVerifiedSchemaVersion != store.SchemaVersion() {
		t.Fatal("manifest did not record the verified applied schema")
	}
	if manifest.AccountID != account {
		t.Fatal("manifest bootstrap account differs from the database")
	}
	if manifest.PostgreSQLVersion == "" || manifest.ServerVersion == "" || manifest.WorkerVersion == "" {
		t.Fatal("manifest omitted component versions")
	}
	assertVersionEndpoint(t, manifest)
	if err := cell.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertStopped(t, cell)
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if readStoreState(t, root, assertReady(t, cell)) != account {
		t.Fatal("restart changed the default account")
	}
	if err := cell.Remove(ctx, localcell.RemoveOptions{}); err != nil {
		t.Fatalf("remove retaining data: %v", err)
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
	if err := cell.Remove(ctx, localcell.RemoveOptions{Purge: true}); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, name := range []string{"pg", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("purge retained %s", name)
		}
	}
}

type lifecycleDatabaseProof func(*testing.T, string, *localcell.Manifest)

func testTwoCellsAreIndependent(t *testing.T, binaries localcell.Binaries, databaseProof lifecycleDatabaseProof) {
	first, firstRoot := disposableCell(t, binaries, nil)
	second, secondRoot := disposableCell(t, binaries, nil)
	ports := make(map[int]bool)
	roots := map[*localcell.Cell]string{first: firstRoot, second: secondRoot}
	for _, cell := range []*localcell.Cell{first, second} {
		root := roots[cell]
		if err := cell.Install(t.Context(), localcell.InstallOptions{LeaveRunning: true}); err != nil {
			t.Fatalf("install: %v", err)
		}
		if err := startLifecycle(t, cell, root); err != nil {
			t.Fatalf("start: %v", err)
		}
		manifest := assertReady(t, cell)
		for _, port := range []int{manifest.Ports.Postgres, manifest.Ports.ServerAPI, manifest.Ports.ServerHealth, manifest.Ports.ServerMetrics, manifest.Ports.WorkerHealth, manifest.Ports.WorkerMetrics} {
			if port == 0 || ports[port] {
				t.Fatal("two live cells allocated a duplicate or missing port")
			}
			ports[port] = true
		}
	}
	if err := lifecycleWriteFile(filepath.Join(firstRoot, "spool", "first-cell-only"), []byte("isolated")); err != nil {
		t.Fatal("cannot create spool isolation witness")
	}
	if _, err := os.Stat(filepath.Join(secondRoot, "spool", "first-cell-only")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cells share spool contents")
	}
	if databaseProof != nil {
		databaseProof(t, firstRoot, assertReady(t, first))
		databaseProof(t, secondRoot, assertReady(t, second))
	}
	if err := first.Stop(t.Context()); err != nil {
		t.Fatalf("stop first cell: %v", err)
	}
	assertStopped(t, first)
	assertReady(t, second)
}

func testInterruptedStartRepair(t *testing.T, binaries localcell.Binaries, databaseProof lifecycleDatabaseProof) {
	interrupted := false
	cell, root := disposableCell(t, binaries, func(operation, step string) error {
		if operation == "start" && step == "postgres" && !interrupted {
			interrupted = true
			return errors.New("injected process-start interruption")
		}
		return nil
	})
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := startLifecycle(t, cell, root); err == nil || !interrupted {
		t.Fatal("start did not reach the interruption point")
	}
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Interrupted == nil {
		t.Fatal("inspect failed to report the interrupted operation")
	}
	if !inspection.Processes["postgres"].Alive || inspection.Processes["server"].Alive {
		t.Fatal("interruption did not occur between PostgreSQL and server startup")
	}
	if err := cell.Repair(t.Context()); err != nil {
		t.Fatalf("repair: %v", err)
	}
	assertStopped(t, cell)
	inspection, err = cell.Inspect(t.Context())
	if err != nil || inspection.Interrupted != nil || !inspection.PGDataPresent {
		t.Fatal("repair did not settle the journal while preserving PGDATA")
	}
	if _, err := os.Stat(filepath.Join(root, "pg", "PG_VERSION")); err != nil {
		t.Fatal("repair deleted the PostgreSQL cluster")
	}
	if err := cell.Repair(t.Context()); err != nil {
		t.Fatalf("second repair: %v", err)
	}
	assertStopped(t, cell)
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start after repair: %v", err)
	}
	manifest := assertReady(t, cell)
	if databaseProof != nil {
		databaseProof(t, root, manifest)
	}
}

func testWaitsForReadiness(t *testing.T, binaries localcell.Binaries, databaseProof lifecycleDatabaseProof) {
	cell, root := disposableCell(t, binaries, nil)
	if err := cell.Install(t.Context(), localcell.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if databaseProof != nil {
		if err := startLifecycle(t, cell, root); err != nil {
			t.Fatalf("start before readiness fault: %v", err)
		}
		// Prove the database before substituting the server that deliberately
		// cannot become ready or serve the Unicode HTTP request.
		databaseProof(t, root, assertReady(t, cell))
		if err := cell.Stop(t.Context()); err != nil {
			t.Fatalf("stop before readiness fault: %v", err)
		}
	}
	buildDir := t.TempDir()
	source := filepath.Join(buildDir, "notready.go")
	const program = `package main
import ("fmt"; "net/http"; "os")
func main() {
 if len(os.Args)>1 && os.Args[1]=="--version" { fmt.Println("localcell readiness fixture"); return }
 handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
 if http.ListenAndServe(os.Getenv("WITSELF_HEALTH_ADDR"), handler)!=nil { os.Exit(1) }
}`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal("cannot write readiness fixture")
	}
	binaries.Server = filepath.Join(buildDir, lifecycleExecutableName("notready"))
	cmd := offlineBuildCommand(t.Context(), "build", "-o", binaries.Server, source)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal("cannot build readiness fixture")
	}
	// Ten seconds bounds non-server steps; a Windows PostgreSQL restart alone takes about 2.5 s
	// (oracle round 6), so a two-second bound failed the postgres step before the server
	// readiness fault under test was ever reached.
	cell, err := localcell.New(localcell.Config{Root: root, Binaries: binaries, Timeout: 10 * time.Second, MigrationWait: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("cannot set bounded readiness timeout")
	}
	started := time.Now()
	err = startLifecycle(t, cell, root)
	if err == nil || !strings.Contains(err.Error(), "server") {
		t.Fatal("start succeeded without server readiness or omitted the failed component")
	}
	component, cause, state, observationFailed, ok := localcell.ReadinessFailureForTest(err)
	if !ok || component != "server" || cause != "local cell server readiness deadline exceeded" ||
		state.PID <= 1 || !state.Alive || !state.Matches || observationFailed {
		t.Fatal("readiness deadline lost the live matching identity observed before rollback")
	}
	if time.Since(started) < time.Second {
		t.Fatal("start skipped the bounded readiness wait")
	}
	assertStopped(t, cell)
}

func assertReady(t *testing.T, cell *localcell.Cell) *localcell.Manifest {
	t.Helper()
	status, err := cell.Status(t.Context())
	if err != nil || !status.ServerReady || !status.WorkerReady || status.Manifest == nil {
		t.Fatal("both local readiness endpoints must be ready when start returns")
	}
	return status.Manifest
}

func assertStopped(t *testing.T, cell *localcell.Cell) {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil {
		t.Fatalf("inspect stopped cell: %v", err)
	}
	for component, process := range inspection.Processes {
		if process.Alive {
			t.Fatalf("%s remained alive", component)
		}
	}
}

func readStoreState(t *testing.T, root string, manifest *localcell.Manifest) string {
	t.Helper()
	if manifest.Database != "witself" {
		t.Fatal("disposable cell database must be witself")
	}
	dsn := lifecycleDSN(t, root, manifest)
	st, err := store.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal("cannot open test-owned local store")
	}
	defer st.Close()
	schema, err := st.AppliedSchemaVersion(t.Context())
	if err != nil || schema != store.SchemaVersion() {
		t.Fatal("local database applied schema does not match the compiled schema")
	}
	account, err := st.LocalBootstrapAccount(t.Context())
	if err != nil || account == "" {
		t.Fatal("local bootstrap account is absent")
	}
	return account
}

func assertVersionEndpoint(t *testing.T, manifest *localcell.Manifest) {
	t.Helper()
	endpoint := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(manifest.Ports.ServerAPI)) + "/v1/version"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal("cannot construct version request")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		t.Fatal("local version request failed")
	}
	defer func() { _ = response.Body.Close() }()
	var version struct {
		Schema int `json:"store_schema_version"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&version); err != nil || response.StatusCode != http.StatusOK || version.Schema != store.SchemaVersion() {
		t.Fatal("version endpoint did not report the applied local schema")
	}
}
