package localcell_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	t.Run("InterruptedStartRepair", func(t *testing.T) { testInterruptedStartRepair(t, binaries, nil) })
	t.Run("ReadinessFailureRollsBack", func(t *testing.T) { testWaitsForReadiness(t, binaries, nil) })
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
