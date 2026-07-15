package memorycurator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStateStoreIsAtomicPrivateAndValueFree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "curation", "agent-test")
	store := FileStateStore{Root: root}
	state := validLaunchState(PhasePlanning)
	state.PlanHash = strings.Repeat("c", 64)
	state.PlanRevision = 1
	state.PlanReceiptID = "mplan_receipt"
	state.Client.Runtime = "codex"
	state.Client.Model = "provider-model"

	if err := store.Save(state); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	path := filepath.Join(root, state.LaunchID+".json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %#o, want 0600", got)
	}
	directoryInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("state directory mode = %#o, want 0700", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"materialized_inputs":`, `"transcript_entries":`, `"draft":`, `"memory_content":`, `"evidence_artifact":`, "WITSELF_TOKEN"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("state contains forbidden payload field %q: %s", forbidden, raw)
		}
	}
	loaded, err := store.Load(state.LaunchID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.PlanHash != state.PlanHash || loaded.Phase != state.Phase || loaded.Client.Runtime != "codex" {
		t.Fatalf("Load() = %#v", loaded)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != state.LaunchID+".json" {
		t.Fatalf("unexpected state directory entries: %#v", entries)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(state.LaunchID); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("Load() with broad permissions error = %v", err)
	}
}

func TestFileStateStoreRejectsPathTraversal(t *testing.T) {
	store := FileStateStore{Root: t.TempDir()}
	if _, err := store.Load("../escape"); err == nil {
		t.Fatal("Load() accepted path traversal")
	}
	state := validLaunchState(PhaseStarted)
	state.LaunchID = "../escape"
	if err := store.Save(state); err == nil {
		t.Fatal("Save() accepted path traversal")
	}
}
