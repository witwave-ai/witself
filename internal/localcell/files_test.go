package localcell

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func unitCell(t *testing.T) (*Cell, *Manifest) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	c, err := New(Config{Root: root, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.layout(); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		LayoutVersion: LayoutVersion, InstallationID: "lc_test", CreatedAt: time.Now().UTC(),
		Database: localDatabaseName, Role: "localcell", Installed: true,
		Ports: Ports{Postgres: 21101, ServerAPI: 21102, ServerHealth: 21103, ServerMetrics: 21104, WorkerHealth: 21105, WorkerMetrics: 21106},
	}
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	return c, m
}

func TestManifestAtomicPublishInterruptionPreservesOld(t *testing.T) {
	c, before := unitCell(t)
	after := *before
	after.LastOperation = "start"
	injected := errors.New("interrupted before publish")
	called := false
	err := writeAtomic(c.path("manifest.json"), &after, func() error {
		called = true
		observed, err := readManifest(c.cfg.Root)
		if err != nil || !reflect.DeepEqual(observed, before) {
			t.Fatal("old manifest changed before atomic rename")
		}
		pending, err := filepath.Glob(c.path(".pending-*"))
		if err != nil || len(pending) != 1 {
			t.Fatal("atomic publisher did not stage exactly one temporary record")
		}
		return injected
	})
	if !called || !errors.Is(err, injected) {
		t.Fatal("atomic publication interruption seam was not honored")
	}
	observed, err := readManifest(c.cfg.Root)
	if err != nil || !reflect.DeepEqual(observed, before) {
		t.Fatal("interrupted publication replaced the durable manifest")
	}
	if err := writeAtomic(c.path("manifest.json"), &after, nil); err != nil {
		t.Fatal(err)
	}
	observed, err = readManifest(c.cfg.Root)
	if err != nil || !reflect.DeepEqual(observed, &after) {
		t.Fatal("successful atomic publication did not replace the manifest")
	}
}

func TestManifestRejectsUnknownLayoutVersion(t *testing.T) {
	c, m := unitCell(t)
	m.LayoutVersion = LayoutVersion + 1
	if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(c.cfg.Root); err == nil {
		t.Fatal("unknown manifest layout version was accepted")
	}
}

func TestManifestRejectsUnknownFields(t *testing.T) {
	c, m := unitCell(t)
	withExtra := struct {
		*Manifest
		Extra string `json:"extra"`
	}{Manifest: m, Extra: "unsupported"}
	if err := writeAtomic(c.path("manifest.json"), withExtra, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(c.cfg.Root); err == nil {
		t.Fatal("manifest with an unknown field was accepted")
	}
}

func TestManifestRejectsOtherDatabaseNames(t *testing.T) {
	for _, database := range []string{"", "codex_b", "postgres", "other", "Witself", localDatabaseName + " "} {
		t.Run(database, func(t *testing.T) {
			c, m := unitCell(t)
			m.Database = database
			if err := writeAtomic(c.path("manifest.json"), m, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := readManifest(c.cfg.Root); err == nil {
				t.Fatal("manifest with a different database name was accepted")
			}
		})
	}
}

func TestJournalInterruptionRepairPreservesDataAndRepeats(t *testing.T) {
	c, _ := unitCell(t)
	if err := os.Mkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal(err)
	}
	dataPath := c.path("pg", "retained-data")
	const retained = "synthetic database bytes"
	if err := os.WriteFile(dataPath, []byte(retained), 0o600); err != nil {
		t.Fatal(err)
	}
	c.cfg.Failpoint = func(operation, step string) error {
		if operation == "start" && step == "postgres" {
			return errors.New("synthetic interruption")
		}
		return nil
	}
	if err := c.journal("start", "operation", "started"); err != nil {
		t.Fatal(err)
	}
	if err := c.step("start", "postgres", func() error { return nil }); !errors.Is(err, errInterrupted) {
		t.Fatal("journal failure injection did not interrupt the step")
	}
	inspection, err := c.Inspect(t.Context())
	if err != nil || inspection.Interrupted == nil || inspection.Interrupted.Operation != "start" || inspection.Interrupted.Step != "postgres" {
		t.Fatal("Inspect did not report the interrupted journal step")
	}
	c.cfg.Failpoint = nil
	if err := c.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Repair(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(c.path("journal", "operations.jsonl"))
	if err != nil || string(first) != string(second) {
		t.Fatal("second Repair changed the completed journal")
	}
	inspection, err = c.Inspect(t.Context())
	if err != nil || inspection.Interrupted != nil {
		t.Fatal("Repair left an interrupted operation")
	}
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != retained {
		t.Fatal("Repair deleted or modified retained PostgreSQL data")
	}
}

func TestJournalRejectsUnfencedOrMismatchedRecords(t *testing.T) {
	now := time.Now().UTC()
	begin := journalEntry{Operation: "start", Step: "operation", State: "started", At: now}
	for _, test := range []struct {
		name    string
		entries []journalEntry
	}{
		{"unfenced", []journalEntry{{Operation: "start", Step: "server", State: "done", At: now}}},
		{"mismatched", []journalEntry{begin, {Operation: "remove", Step: "operation", State: "done", At: now}}},
		{"overlapping", []journalEntry{begin, begin}},
		{"unknown-step", []journalEntry{begin, {Operation: "start", Step: "arbitrary", State: "started", At: now}}},
		{"missing-time", []journalEntry{{Operation: "start", Step: "operation", State: "started"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := unitCell(t)
			var data []byte
			for _, entry := range test.entries {
				encoded, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, append(encoded, '\n')...)
			}
			if err := os.WriteFile(c.path("journal", "operations.jsonl"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Inspect(t.Context()); err == nil {
				t.Fatal("Inspect accepted an untrusted journal fence")
			}
			if err := c.Repair(t.Context()); err == nil {
				t.Fatal("Repair accepted an untrusted journal fence")
			}
		})
	}
}

func TestRemoveCleanupInterruptionRepairsFromManifest(t *testing.T) {
	for _, boundary := range []string{"cleanup_run", "cleanup_journal"} {
		t.Run(boundary, func(t *testing.T) {
			c, _ := unitCell(t)
			if err := os.Mkdir(c.path("pg"), 0o700); err != nil {
				t.Fatal(err)
			}
			const retained = "synthetic retained database bytes"
			if err := os.WriteFile(c.path("pg", "retained-data"), []byte(retained), 0o600); err != nil {
				t.Fatal(err)
			}
			c.cfg.Failpoint = func(operation, step string) error {
				if operation == "remove" && step == boundary {
					return errors.New("synthetic cleanup interruption")
				}
				return nil
			}
			if err := c.Remove(t.Context(), RemoveOptions{}); !errors.Is(err, errInterrupted) {
				t.Fatal("removal did not stop at the cleanup boundary")
			}
			inspection, err := c.Inspect(t.Context())
			if err != nil || inspection.Interrupted == nil || inspection.Interrupted.Operation != "remove" || inspection.Interrupted.Step != "runtime_cleanup" || inspection.Manifest == nil || inspection.Manifest.Cleanup == nil {
				t.Fatal("Inspect lost the manifest-backed cleanup interruption")
			}
			c.cfg.Failpoint = nil
			if err := c.Repair(t.Context()); err != nil {
				t.Fatal(err)
			}
			manifestBefore, err := os.ReadFile(c.path("manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Repair(t.Context()); err != nil {
				t.Fatal(err)
			}
			manifestAfter, err := os.ReadFile(c.path("manifest.json"))
			if err != nil || string(manifestBefore) != string(manifestAfter) {
				t.Fatal("second cleanup Repair changed the retained manifest")
			}
			for _, name := range []string{"run", "spool", "journal", "tmp", "logs"} {
				if _, err := os.Lstat(c.path(name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cleanup Repair left or recreated runtime directory %s", name)
				}
			}
			data, err := os.ReadFile(c.path("pg", "retained-data"))
			if err != nil || string(data) != retained {
				t.Fatal("cleanup Repair deleted or modified PostgreSQL data")
			}
			inspection, err = c.Inspect(t.Context())
			if err != nil || inspection.Interrupted != nil || inspection.Manifest.Cleanup != nil {
				t.Fatal("cleanup Repair did not settle its durable checkpoint")
			}
		})
	}
}
