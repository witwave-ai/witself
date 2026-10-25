package localcell_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/localcell"
)

// These synthetic peers exercise journal recovery and process supervision. They
// do not claim to validate PostgreSQL storage or native initdb behavior.
func TestInstallRepair(t *testing.T) {
	binaries := buildLifecycleFixture(t)
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Run("CompletedRequiredStepsResume", func(t *testing.T) {
		cell, root := disposableCell(t, binaries, nil)
		if err := cell.Install(t.Context(), localcell.InstallOptions{LeaveRunning: true}); err != nil {
			t.Fatalf("synthetic install: %v", err)
		}
		stageInstallInterruption(t, cell, root, "database")
		assertPreserved := preserveInstallData(t, root)
		inspection, err := cell.Inspect(t.Context())
		if err != nil || inspection.Interrupted == nil || inspection.Interrupted.Operation != "install" || inspection.Interrupted.Step != "database" {
			t.Fatal("inspect did not report the interruption after the durable database step")
		}
		if inspection.Manifest == nil || inspection.Manifest.Installed || !inspection.Processes["postgres"].Alive {
			t.Fatal("interrupted install must be unpublished with PostgreSQL still running")
		}
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatalf("repair completed install: %v", err)
		}
		assertStopped(t, cell)
		inspection, err = cell.Inspect(t.Context())
		if err != nil || inspection.Manifest == nil || !inspection.Manifest.Installed || inspection.Interrupted != nil {
			t.Fatal("repair did not publish the completed install and settle its journal")
		}
		assertPreserved()
		assertInstallRepairNoop(t, cell, root)
		if err := cell.Start(t.Context()); err != nil {
			t.Fatalf("start after resumed install: %v", err)
		}
		assertReady(t, cell)
		assertPreserved()
	})
	t.Run("PostgresDoneWithoutDatabaseRemainsIncomplete", func(t *testing.T) {
		cell, root := disposableCell(t, binaries, nil)
		if err := cell.Install(t.Context(), localcell.InstallOptions{LeaveRunning: true}); err != nil {
			t.Fatalf("synthetic install: %v", err)
		}
		stageInstallInterruption(t, cell, root, "postgres")
		assertPreserved := preserveInstallData(t, root)
		inspection, err := cell.Inspect(t.Context())
		if err != nil || inspection.Interrupted == nil || inspection.Interrupted.Operation != "install" || inspection.Interrupted.Step != "postgres" {
			t.Fatal("inspect did not report the interruption after the durable postgres step")
		}
		if inspection.Manifest == nil || inspection.Manifest.Installed || !inspection.Processes["postgres"].Alive {
			t.Fatal("interrupted install must be unpublished with PostgreSQL still running")
		}
		if inspection.InstallState != "install incomplete; purge to start over" {
			t.Fatal("inspect accepted an install with no durable database step")
		}
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatalf("repair install without database step: %v", err)
		}
		assertStopped(t, cell)
		inspection, err = cell.Inspect(t.Context())
		if err != nil || inspection.Manifest == nil || inspection.Manifest.Installed || inspection.Interrupted != nil {
			t.Fatal("repair published an install with no durable database step")
		}
		if inspection.InstallState != "install incomplete; purge to start over" {
			t.Fatal("repair omitted the exact incomplete-install recovery state")
		}
		assertPreserved()
		assertInstallRepairNoop(t, cell, root)
		if err := cell.Start(t.Context()); err == nil || err.Error() != "local cell installation is incomplete" {
			t.Fatal("start accepted an installation with no durable database step")
		}
		assertStopped(t, cell)
		assertPreserved()
	})
	t.Run("MissingRequiredStepsRemainIncomplete", func(t *testing.T) {
		interrupted := false
		cell, root := disposableCell(t, binaries, func(operation, step string) error {
			if operation == "install" && step == "initdb" && !interrupted {
				interrupted = true
				return errors.New("injected install interruption")
			}
			return nil
		})
		if err := cell.Install(t.Context(), localcell.InstallOptions{}); err == nil || !interrupted {
			t.Fatal("install did not reach the initdb interruption")
		}
		assertPreserved := preserveInstallData(t, root)
		if err := cell.Repair(t.Context()); err != nil {
			t.Fatalf("repair incomplete install: %v", err)
		}
		assertStopped(t, cell)
		inspection, err := cell.Inspect(t.Context())
		if err != nil || inspection.Manifest == nil || inspection.Manifest.Installed || inspection.Interrupted != nil {
			t.Fatal("repair did not retain the incomplete install while settling its journal")
		}
		if inspection.InstallState != "install incomplete; purge to start over" {
			t.Fatal("inspect omitted the explicit incomplete-install recovery state")
		}
		assertPreserved()
		assertInstallRepairNoop(t, cell, root)
		if err := cell.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "installation is incomplete") {
			t.Fatal("start accepted an installation missing required durable steps")
		}
		assertStopped(t, cell)
		assertPreserved()
	})
}

func stageInstallInterruption(t *testing.T, cell *localcell.Cell, root, lastStep string) {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil || inspection.Manifest == nil || !inspection.Manifest.Installed {
		t.Fatal("cannot prepare interrupted install manifest")
	}
	journalPath := filepath.Join(root, "journal", "operations.jsonl")
	journal, err := os.ReadFile(journalPath)
	if err != nil || !bytes.HasSuffix(journal, []byte("\n")) {
		t.Fatal("cannot prepare interrupted install journal")
	}
	lines := bytes.Split(bytes.TrimSuffix(journal, []byte("\n")), []byte("\n"))
	if len(lines) < 4 {
		t.Fatal("install journal omitted required progress")
	}
	for i, expected := range []struct{ step, state string }{{"postgres", "done"}, {"database", "started"}, {"database", "done"}, {"operation", "done"}} {
		var entry struct {
			Operation string `json:"operation"`
			Step      string `json:"step"`
			State     string `json:"state"`
		}
		if err := json.Unmarshal(lines[len(lines)-4+i], &entry); err != nil || entry.Operation != "install" || entry.Step != expected.step || entry.State != expected.state {
			t.Fatal("install journal lacks the expected durable completion records")
		}
	}
	// Replay a crash before final manifest publication. Keep the existing
	// PostgreSQL process and its identity fence, with only acknowledged steps.
	keep := len(lines) - 1
	switch lastStep {
	case "postgres":
		keep = len(lines) - 3
	case "database":
	default:
		t.Fatal("unsupported interrupted install boundary")
	}
	inspection.Manifest.Installed = false
	manifest, err := json.Marshal(inspection.Manifest)
	if err != nil {
		t.Fatal("cannot encode interrupted install manifest")
	}
	if err := lifecycleWriteFile(filepath.Join(root, "manifest.json"), manifest); err != nil {
		t.Fatal("cannot stage interrupted install manifest")
	}
	prefix := append(bytes.Join(lines[:keep], []byte("\n")), '\n')
	if err := lifecycleWriteFile(journalPath, prefix); err != nil {
		t.Fatal("cannot stage interrupted install journal")
	}
}

func preserveInstallData(t *testing.T, root string) func() {
	t.Helper()
	witness := filepath.Join(root, "pg", "repair-preserved-data")
	if err := lifecycleWriteFile(witness, []byte("existing cluster data")); err != nil {
		t.Fatal("cannot prepare PGDATA preservation witness")
	}
	paths := []string{filepath.Join(root, "pg", "PG_VERSION"), witness}
	contents := make([][]byte, len(paths))
	identities := make([]os.FileInfo, len(paths))
	for i, path := range paths {
		var err error
		contents[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read PGDATA preservation witness")
		}
		identities[i], err = os.Stat(path)
		if err != nil {
			t.Fatal("cannot inspect PGDATA preservation witness")
		}
	}
	return func() {
		t.Helper()
		for i, path := range paths {
			content, readErr := os.ReadFile(path)
			identity, statErr := os.Stat(path)
			if readErr != nil || statErr != nil || !bytes.Equal(content, contents[i]) || !os.SameFile(identity, identities[i]) {
				t.Fatal("install repair reinitialized or deleted existing PGDATA")
			}
		}
	}
}

func assertInstallRepairNoop(t *testing.T, cell *localcell.Cell, root string) {
	t.Helper()
	paths := []string{filepath.Join(root, "manifest.json"), filepath.Join(root, "journal", "operations.jsonl")}
	before := make([][]byte, len(paths))
	identities := make([]os.FileInfo, len(paths))
	for i, path := range paths {
		var err error
		before[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read settled install recovery state")
		}
		identities[i], err = os.Stat(path)
		if err != nil {
			t.Fatal("cannot inspect settled install recovery state")
		}
	}
	if err := cell.Repair(t.Context()); err != nil {
		t.Fatalf("second install repair: %v", err)
	}
	for i, path := range paths {
		after, readErr := os.ReadFile(path)
		identity, statErr := os.Stat(path)
		if readErr != nil || statErr != nil || !bytes.Equal(before[i], after) ||
			!os.SameFile(identities[i], identity) || !identities[i].ModTime().Equal(identity.ModTime()) {
			t.Fatal("second install repair changed the manifest or journal")
		}
	}
}
