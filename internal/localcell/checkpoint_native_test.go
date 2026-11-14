package localcell_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/localcell"
)

// checkpointNativeEntry is one copied object: a directory, or a file's size and
// SHA-256, held in memory only and never printed.
type checkpointNativeEntry struct {
	directory bool
	size      int64
	digest    string
}

// checkpointNativeTree walks each named path below base, keyed by slash path
// relative to base. Every object must be a directory or a regular file.
func checkpointNativeTree(t *testing.T, base string, names ...string) map[string]checkpointNativeEntry {
	t.Helper()
	tree := map[string]checkpointNativeEntry{}
	for _, name := range names {
		err := filepath.WalkDir(filepath.Join(base, filepath.FromSlash(name)), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, path)
			if err != nil || (!entry.IsDir() && !entry.Type().IsRegular()) {
				return errors.New("unexpected native checkpoint object")
			}
			value := checkpointNativeEntry{directory: entry.IsDir()}
			if !entry.IsDir() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(data)
				value.size, value.digest = int64(len(data)), hex.EncodeToString(sum[:])
			}
			tree[filepath.ToSlash(rel)] = value
			return nil
		})
		if err != nil {
			t.Fatal("cannot walk a native checkpoint tree")
		}
	}
	return tree
}

func checkpointNativeRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read a native checkpoint record")
	}
	return data
}

// checkpointNativeInventory decodes a format-1 inventory of this identifier into
// the walk's shape; each entry is a directory or a sized, digested file.
func checkpointNativeInventory(t *testing.T, checkpointID string, data []byte) map[string]checkpointNativeEntry {
	t.Helper()
	var inventory struct {
		Format       int    `json:"format"`
		CheckpointID string `json:"checkpoint_id"`
		Entries      []struct {
			Path   string `json:"path"`
			Type   string `json:"type"`
			Size   *int64 `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"entries"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&inventory) != nil || inventory.Format != 1 || inventory.CheckpointID != checkpointID {
		t.Fatal("a native checkpoint inventory is not format 1 for its identifier")
	}
	entries := map[string]checkpointNativeEntry{}
	for _, entry := range inventory.Entries {
		value := checkpointNativeEntry{directory: entry.Type == "dir"}
		if entry.Type == "file" && entry.Size != nil {
			value.size, value.digest = *entry.Size, entry.SHA256
		} else if !value.directory || entry.Size != nil || entry.SHA256 != "" {
			t.Fatal("a native checkpoint inventory entry is malformed")
		}
		if _, repeated := entries[entry.Path]; repeated {
			t.Fatal("a native checkpoint inventory repeats an entry")
		}
		entries[entry.Path] = value
	}
	return entries
}

type checkpointNativeControl struct {
	ControlVersion   string `json:"control_version"`
	CatalogVersion   string `json:"catalog_version"`
	SystemIdentifier string `json:"system_identifier"`
	State            string `json:"state"`
	CheckpointLSN    string `json:"checkpoint_lsn"`
}

// checkpointNativeReadControl independently reads the stopped cluster once;
// neither command output nor parsed values may appear in diagnostics.
func checkpointNativeReadControl(t *testing.T, binaries localcell.Binaries, root string) checkpointNativeControl {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(binaries.PostgresDir, lifecycleExecutableName("pg_controldata")), "-D", filepath.Join(root, "pg"))
	cmd.Dir, cmd.Env = root, []string{"LC_ALL=C", "LC_MESSAGES=C", "LANG=C"}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	defer func() { clear(output.Bytes()) }()
	if cmd.Run() != nil {
		t.Fatal("cannot independently read native checkpoint control data")
	}
	var control checkpointNativeControl
	fields := map[string]*string{
		"pg_control version number": &control.ControlVersion, "Catalog version number": &control.CatalogVersion,
		"Database system identifier": &control.SystemIdentifier, "Database cluster state": &control.State,
		"Latest checkpoint location": &control.CheckpointLSN,
	}
	for _, line := range strings.Split(strings.ReplaceAll(output.String(), "\r\n", "\n"), "\n") {
		key, value, found := strings.Cut(line, ":")
		if field := fields[strings.TrimSpace(key)]; found && field != nil {
			if *field != "" {
				t.Fatal("independent native checkpoint control data repeats a field")
			}
			*field = strings.TrimSpace(value)
		}
	}
	for _, field := range fields {
		if *field == "" {
			t.Fatal("independent native checkpoint control data lacks a field")
		}
	}
	if control.State != "shut down" {
		t.Fatal("independent native checkpoint control data is not shut down")
	}
	return control
}

// checkpointNativeWitness checks witness.json against its inventory bytes and
// the live copy set, and returns all five control fields.
func checkpointNativeWitness(t *testing.T, checkpointID, installationID string, inventory, record []byte, live map[string]checkpointNativeEntry) checkpointNativeControl {
	t.Helper()
	var witness struct {
		Format          int                     `json:"format"`
		CheckpointID    string                  `json:"checkpoint_id"`
		InstallationID  string                  `json:"installation_id"`
		PostgresMajor   string                  `json:"postgres_major"`
		Control         checkpointNativeControl `json:"control"`
		Files           int                     `json:"files"`
		Directories     int                     `json:"directories"`
		Bytes           int64                   `json:"bytes"`
		InventorySHA256 string                  `json:"inventory_sha256"`
	}
	files, directories, total := 0, 0, int64(0)
	for _, entry := range live {
		if entry.directory {
			directories++
		} else {
			files, total = files+1, total+entry.size
		}
	}
	sum := sha256.Sum256(inventory)
	if json.Unmarshal(record, &witness) != nil || witness.Format != 1 ||
		witness.CheckpointID != checkpointID || witness.InstallationID != installationID || witness.PostgresMajor != "16" ||
		witness.Control.State != "shut down" || witness.Control.SystemIdentifier == "" || witness.Control.CheckpointLSN == "" ||
		witness.Files != files || witness.Directories != directories || witness.Bytes != total || witness.InventorySHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("a native checkpoint witness does not match its inventory and the live tree")
	}
	return witness.Control
}

// checkpointNativeListed is Inspect's checkpoint view, identifier to
// completeness; Inspect must succeed after every checkpoint operation.
func checkpointNativeListed(t *testing.T, cell *localcell.Cell) map[string]bool {
	t.Helper()
	inspection, err := cell.Inspect(t.Context())
	if err != nil {
		t.Fatalf("inspect after a checkpoint operation: %v", err)
	}
	listed := map[string]bool{}
	for _, summary := range inspection.Checkpoints {
		listed[summary.ID] = summary.Complete
	}
	return listed
}

// checkpointNativeNew is the one identifier Inspect lists after a call but not
// before it; that checkpoint must be complete.
func checkpointNativeNew(t *testing.T, before, after map[string]bool) string {
	t.Helper()
	var created []string
	for checkpointID := range after {
		if _, existed := before[checkpointID]; !existed {
			created = append(created, checkpointID)
		}
	}
	if len(created) != 1 || len(after) != len(before)+1 || !after[created[0]] {
		t.Fatal("a checkpoint call did not add exactly one complete checkpoint")
	}
	return created[0]
}

func checkpointNativeStop(t *testing.T, cell *localcell.Cell) time.Duration {
	t.Helper()
	started := time.Now()
	if err := cell.Stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	return time.Since(started)
}

// testCheckpointNative (N1) proves the dark checkpoint API on a real, stopped
// PostgreSQL 16 cluster in the legacy layout; hermetic C2 covers selected
// custody. A new identifier comes from Inspect before and after each call,
// never from an error. Only counts and elapsed times are logged.
func testCheckpointNative(t *testing.T, binaries localcell.Binaries) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	t.Setenv("WITSELF_RUNTIME", "local")
	var timeout time.Duration
	cell, root := disposableCell(t, binaries, nil, func(config *localcell.Config) { timeout = config.Timeout })
	if timeout <= 0 {
		t.Fatal("the disposable cell timeout was not observed")
	}
	ctx := t.Context()
	// Step 1: install, prove readiness once, stop.
	if err := cell.Install(ctx, localcell.InstallOptions{}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start: %v", err)
	}
	manifest := assertReady(t, cell)
	if err := cell.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertStopped(t, cell)
	// Step 2: another identity is refused; recovery stays absent, Inspect's
	// checkpoint listing stays empty, and journal bytes stay equal.
	journal := filepath.Join(root, "journal", "operations.jsonl")
	before, prefix := checkpointNativeListed(t, cell), checkpointNativeRead(t, journal)
	err := cell.Checkpoint(ctx, localcell.CheckpointOptions{InstallationID: manifest.InstallationID + "-other"})
	_, statErr := os.Lstat(filepath.Join(root, "recovery"))
	if !errors.Is(err, localcell.ErrCheckpointIdentityMismatchForTest) || !errors.Is(statErr, fs.ErrNotExist) || len(before) != 0 ||
		len(checkpointNativeListed(t, cell)) != 0 || !bytes.Equal(prefix, checkpointNativeRead(t, journal)) {
		t.Fatal("a wrong-identity checkpoint was not refused without changes")
	}
	// Step 3: one stop (one own() walk) with no checkpoint.
	baseline := checkpointNativeStop(t, cell)
	independent := checkpointNativeReadControl(t, binaries, root)
	liveBefore := checkpointNativeTree(t, root, "pg", "manifest.json", "run/postgres.password")
	liveBefore["run"] = checkpointNativeEntry{directory: true}
	// Step 4: two checkpoints, each an exact copy of the stopped live tree.
	options := localcell.CheckpointOptions{InstallationID: manifest.InstallationID}
	var ids []string
	for range 2 {
		before := checkpointNativeListed(t, cell)
		err := cell.Checkpoint(ctx, options)
		after := checkpointNativeListed(t, cell)
		if err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		ids = append(ids, checkpointNativeNew(t, before, after))
	}
	live := checkpointNativeTree(t, root, "pg", "manifest.json", "run/postgres.password")
	live["run"] = checkpointNativeEntry{directory: true}
	if !reflect.DeepEqual(liveBefore, live) {
		t.Fatal("checkpoints changed the stopped live copy set")
	}
	var control [2]checkpointNativeControl
	var inventories, witnesses [2][]byte
	for index, checkpointID := range ids {
		dir := filepath.Join(root, "recovery", checkpointID)
		inventory := checkpointNativeRead(t, filepath.Join(dir, "inventory.json"))
		inventories[index], witnesses[index] = inventory, checkpointNativeRead(t, filepath.Join(dir, "witness.json"))
		copied := checkpointNativeTree(t, dir, ".")
		for _, name := range []string{".", "inventory.json", "witness.json"} {
			delete(copied, name)
		}
		if !reflect.DeepEqual(live, checkpointNativeInventory(t, checkpointID, inventory)) || !reflect.DeepEqual(live, copied) {
			t.Fatal("a checkpoint is not an exact copy of the stopped live tree")
		}
		control[index] = checkpointNativeWitness(t, checkpointID, manifest.InstallationID, inventory, witnesses[index], live)
		if control[index] != independent {
			t.Fatal("a checkpoint witness differs from the independent cluster control reading")
		}
	}
	if control[0] != control[1] {
		t.Fatal("the two checkpoints witnessed different control data")
	}
	// Step 5 (A-02): the bound detects a stalled walk, not mere inclusion of
	// recovery descendants. TestWindowsPrivateTreeRecoveryBoundary's count row
	// is E0's guard against including them in ordinary walks.
	elapsed := checkpointNativeStop(t, cell)
	t.Logf("checkpoint copy set: %d objects; stop: %d ms with none, %d ms with two", len(live), baseline.Milliseconds(), elapsed.Milliseconds())
	if elapsed > timeout/5 {
		t.Fatalf("checkpoint walk budget exceeded: %d ms", elapsed.Milliseconds())
	}
	// Step 6: a third checkpoint is refused at the limit.
	before = checkpointNativeListed(t, cell)
	prefix = checkpointNativeRead(t, journal)
	if err := cell.Checkpoint(ctx, options); !errors.Is(err, localcell.ErrCheckpointLimitReachedForTest) || !maps.Equal(before, checkpointNativeListed(t, cell)) {
		t.Fatal("a third checkpoint was not refused at the limit")
	}
	if !bytes.Equal(prefix, checkpointNativeRead(t, journal)) {
		t.Fatal("the checkpoint limit refusal changed the journal")
	}
	// Step 7: the live cell is unaffected.
	if err := startLifecycle(t, cell, root); err != nil {
		t.Fatalf("start after checkpoints: %v", err)
	}
	if ready := assertReady(t, cell); ready.InstallationID != manifest.InstallationID || ready.AccountID != manifest.AccountID {
		t.Fatal("checkpoints changed the live installation")
	}
	if err := cell.Stop(ctx); err != nil {
		t.Fatalf("stop after checkpoints: %v", err)
	}
	assertStopped(t, cell)
	// Step 8: removal and purge keep both checkpoints.
	for _, remove := range []localcell.RemoveOptions{{}, {Purge: true}} {
		if err := cell.Remove(ctx, remove); err != nil {
			t.Fatalf("remove: %v", err)
		}
	}
	listed := checkpointNativeListed(t, cell)
	if _, err := os.Lstat(filepath.Join(root, "pg")); !errors.Is(err, fs.ErrNotExist) || len(listed) != 2 || !listed[ids[0]] || !listed[ids[1]] {
		t.Fatal("purge did not remove PGDATA while keeping both complete checkpoints")
	}
	for index, checkpointID := range ids {
		dir := filepath.Join(root, "recovery", checkpointID)
		kept := checkpointNativeTree(t, dir, ".")
		for _, name := range []string{".", "inventory.json", "witness.json"} {
			delete(kept, name)
		}
		if !reflect.DeepEqual(live, kept) ||
			!bytes.Equal(inventories[index], checkpointNativeRead(t, filepath.Join(dir, "inventory.json"))) ||
			!bytes.Equal(witnesses[index], checkpointNativeRead(t, filepath.Join(dir, "witness.json"))) {
			t.Fatal("removal or purge changed a kept checkpoint")
		}
	}
	// Step 9: only the exact-identifier deletions empty recovery/.
	for _, checkpointID := range ids {
		if err := cell.DeleteCheckpoint(ctx, localcell.DeleteCheckpointOptions{CheckpointID: checkpointID}); err != nil {
			t.Fatalf("delete checkpoint: %v", err)
		}
		if _, kept := checkpointNativeListed(t, cell)[checkpointID]; kept {
			t.Fatal("a deleted checkpoint is still listed")
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "recovery")); err != nil || len(entries) != 0 {
		t.Fatal("recovery/ is not empty after both deletions")
	}
}
