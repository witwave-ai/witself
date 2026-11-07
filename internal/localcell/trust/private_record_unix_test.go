//go:build !windows

package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func recordUnixTestHomes(t *testing.T) {
	t.Helper()
	for _, key := range []string{"WITSELF_HOME", "DSH_HOME", "HOME", "USERPROFILE"} {
		t.Setenv(key, t.TempDir())
	}
}
func recordUnixRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("disposable record read failed")
	}
	return b
}
func recordUnixWrite(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if os.WriteFile(path, b, mode) != nil {
		t.Fatal("disposable record write failed")
	}
}

// recordUnixTempDir resolves the disposable directory to its canonical path:
// on darwin the temporary root lives under the /var symlink, which the private
// path-component policy rejects by design (owner fix, 2026-11-07).
func recordUnixTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("disposable record directory resolution failed")
	}
	return dir
}

func recordUnixMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if os.Chmod(path, mode) != nil {
		t.Fatal("disposable record mode change failed")
	}
}

func TestTrustPrivateRecordUnixCompatibility(t *testing.T) {
	recordUnixTestHomes(t)
	t.Run("state-authority-roundtrip-and-detachment", func(t *testing.T) {
		dir := recordUnixTempDir(t)
		recordUnixMode(t, dir, 0700)
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("private state creation failed")
		}
		expected := emptyState()
		expected.InitialRootID = "root-reference"
		expected.RootSequence = 17
		expected.RootHistory = []string{"first-root", "current-root"}
		expected.DelegationSequence = 23
		expected.DelegationDigest = "delegation-reference"
		expected.Releases["stable"] = ReleaseRecord{Sequence: 31, Tag: "release-reference", Digest: "release-digest", AllowedTargets: []uint64{19, 29}}
		expected.Installed["installed-reference"] = InstalledRecord{EnvelopeDigest: "envelope-reference", KeyID: "key-reference", RootSequence: 17, DelegationSequence: 23, ReleaseSequence: 29, ReleaseTag: "rollback-reference", OS: "linux", Arch: "amd64", IssuedAt: "2026-01-01T00:00:00Z", ExpiresAt: "2027-01-01T00:00:00Z"}
		if state.Update(func(next *StateSnapshot) error { *next = cloneState(expected); return nil }) != nil {
			t.Fatal("state authority update failed")
		}
		reopened, err := NewFileState(dir)
		if err != nil {
			t.Fatal("state adapter reopen failed")
		}
		actual, err := reopened.Load()
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatal("state authority or high-water fields changed on reopen")
		}
		actual.RootHistory[0] = "detached-change"
		actual.Releases["stable"].AllowedTargets[0]++
		delete(actual.Installed, "installed-reference")
		again, err := state.Load()
		if err != nil || !reflect.DeepEqual(again, expected) {
			t.Fatal("loaded state was not detached from committed state")
		}
		before := recordUnixRead(t, filepath.Join(dir, "trust-state.json"))
		err = state.Update(func(next *StateSnapshot) error {
			next.RootSequence++
			delete(next.Releases, "stable")
			return ReasonReplay
		})
		if err != ReasonReplay || !bytes.Equal(before, recordUnixRead(t, filepath.Join(dir, "trust-state.json"))) {
			t.Fatal("failed callback changed committed state bytes or classification")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 || entries[0].Name() != "trust-state.json" {
			t.Fatal("state transaction left a temporary file or fence")
		}
	})
	t.Run("before-rename-and-confined-artifacts", func(t *testing.T) {
		parent := recordUnixTempDir(t)
		dir := filepath.Join(parent, "records")
		state, err := NewFileState(dir)
		if err != nil || state.Update(func(next *StateSnapshot) error { next.RootSequence = 5; return nil }) != nil {
			t.Fatal("initial state update failed")
		}
		outside := filepath.Join(parent, "sentinel")
		sentinel := []byte("outside record remains unchanged")
		recordUnixWrite(t, outside, sentinel, 0600)
		before := recordUnixRead(t, filepath.Join(dir, "trust-state.json"))
		observed := false
		state.beforeRename = func() error {
			observed = true
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal("cannot inspect disposable transaction artifacts")
			}
			tempCount, fenceCount := 0, 0
			for _, entry := range entries {
				switch {
				case entry.Name() == "trust-state.json":
				case entry.Name() == ".trust-state.lock":
					fenceCount++
				case strings.HasPrefix(entry.Name(), ".trust-state-"):
					tempCount++
				default:
					t.Fatal("unexpected state artifact name")
				}
				info, infoErr := entry.Info()
				if infoErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatal("state artifact permissions changed")
				}
			}
			if tempCount != 1 || fenceCount != 1 {
				t.Fatal("private temporary file and exclusive fence must exist before publication")
			}
			if !bytes.Equal(before, recordUnixRead(t, filepath.Join(dir, "trust-state.json"))) {
				t.Fatal("temporary write changed committed bytes before publication")
			}
			return ReasonStateIO
		}
		if state.Update(func(next *StateSnapshot) error { next.RootSequence++; return nil }) != ReasonStateIO || !observed {
			t.Fatal("before-rename failure injection did not refuse")
		}
		if !bytes.Equal(before, recordUnixRead(t, filepath.Join(dir, "trust-state.json"))) || !bytes.Equal(sentinel, recordUnixRead(t, outside)) {
			t.Fatal("pre-publication failure modified committed or outside bytes")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatal("pre-publication failure left owned temporary artifacts")
		}
		outsideEntries, err := os.ReadDir(parent)
		if err != nil || len(outsideEntries) != 2 {
			t.Fatal("record artifact escaped selected directory")
		}
	})
	t.Run("generation-receipt-and-journal-profiles", func(t *testing.T) {
		dir := recordUnixTempDir(t)
		recordUnixMode(t, dir, 0700)
		generation := Generation{Version: 1, Name: "generation-reference", ManifestDigest: "manifest-reference", Channel: "stable", Sequence: 41, Tag: "release-reference", OS: "linux", Arch: "amd64", Document: bytes.Repeat([]byte{'x'}, 8192), Envelope: []byte("signed-envelope-reference"), Trust: emptyState(), Host: "bin/host", Args: []string{"serve"}, InventoryDigest: "inventory-reference", InventoryBytes: 97, StagedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
		journal := HandoffJournal{Version: 1, Revision: 11, Generations: map[string]HandoffRecord{generation.Name: {State: "activated", Receipt: &ActivationReceipt{Version: 1, Generation: generation.Name, Host: generation.Host, Args: generation.Args, Identity: HostIdentity{Device: 7, Inode: 13}, ActivatedAt: generation.StagedAt}}}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := withPrivateTransaction(ctx, dir, ".handoff.lock", func() error {
			if err := writePrivateJSON(dir, "generation.json", generation); err != nil {
				return err
			}
			return writePrivateJSON(dir, "handoff.json", journal)
		})
		if err != nil {
			t.Fatal("local record transaction failed")
		}
		var actualGeneration Generation
		var actualJournal HandoffJournal
		if readPrivateJSON(dir, "generation.json", &actualGeneration) != nil || !reflect.DeepEqual(actualGeneration, generation) {
			t.Fatal("generation receipt record fields changed on reopen")
		}
		if readPrivateJSON(dir, "handoff.json", &actualJournal) != nil || !reflect.DeepEqual(actualJournal, journal) {
			t.Fatal("handoff journal record fields changed on reopen")
		}
		raw := recordUnixRead(t, filepath.Join(dir, "generation.json"))
		if localJSONProfile(raw) != nil || StrictProfile(raw) == nil {
			t.Fatal("local record and signed document JSON profiles were merged")
		}
		before := recordUnixRead(t, filepath.Join(dir, "handoff.json"))
		if withPrivateTransaction(ctx, dir, ".handoff.lock", func() error { return ReasonHandoffTransition }) != ReasonHandoffTransition || !bytes.Equal(before, recordUnixRead(t, filepath.Join(dir, "handoff.json"))) {
			t.Fatal("failed handoff callback changed committed journal")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 2 {
			t.Fatal("local record transaction left a temporary file or fence")
		}
		recordUnixMode(t, dir, 0500)
		defer recordUnixMode(t, dir, 0700)
		if readPrivateJSON(dir, "generation.json", &actualGeneration) != nil {
			t.Fatal("read-only generation directory no longer allows private record reads")
		}
		if writePrivateJSON(dir, "handoff.json", journal) != ReasonStateIO {
			t.Fatal("read-only generation directory allowed private record write")
		}
	})
	t.Run("classification-and-optional-absence", func(t *testing.T) {
		dir := recordUnixTempDir(t)
		recordUnixMode(t, dir, 0700)
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("private state creation failed")
		}
		empty, err := state.Load()
		if err != nil || !reflect.DeepEqual(empty, emptyState()) {
			t.Fatal("missing state no longer loads empty state")
		}
		var journal HandoffJournal
		if !errors.Is(readPrivateJSON(dir, "handoff.json", &journal), os.ErrNotExist) {
			t.Fatal("missing optional record classification changed")
		}
		malformed := []byte(`{"format_version":1,"format_version":1}`)
		recordUnixWrite(t, filepath.Join(dir, "trust-state.json"), malformed, 0600)
		if _, err := state.Load(); err != ReasonStateInvalid {
			t.Fatal("malformed state must retain invalid-state classification")
		}
		malformedLocal := []byte(`{"version":1,"version":1}`)
		recordUnixWrite(t, filepath.Join(dir, "handoff.json"), malformedLocal, 0600)
		if readPrivateJSON(dir, "handoff.json", &journal) != ReasonStateInvalid {
			t.Fatal("malformed local JSON must retain invalid-state classification")
		}
		valid, err := json.Marshal(emptyState())
		if err != nil {
			t.Fatal("state fixture encoding failed")
		}
		recordUnixWrite(t, filepath.Join(dir, "trust-state.json"), valid, 0600)
		recordUnixMode(t, filepath.Join(dir, "trust-state.json"), 0644)
		if _, err := state.Load(); err != ReasonStateIO {
			t.Fatal("unsafe state permissions must retain I/O classification")
		}
		recordUnixMode(t, filepath.Join(dir, "handoff.json"), 0644)
		if readPrivateJSON(dir, "handoff.json", &journal) != ReasonStateIO {
			t.Fatal("unsafe local record permissions must retain I/O classification")
		}
		recordUnixMode(t, dir, 0755)
		if _, err := NewFileState(dir); err != ReasonStateIO {
			t.Fatal("unsafe state directory mode accepted")
		}
		if readPrivateJSON(dir, "handoff.json", &journal) != ReasonStateIO {
			t.Fatal("unsafe local directory mode accepted")
		}
	})
	t.Run("symlink-and-name-denials", func(t *testing.T) {
		parent := recordUnixTempDir(t)
		dir := filepath.Join(parent, "records")
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("private state creation failed")
		}
		outside := filepath.Join(parent, "sentinel")
		sentinel := []byte("outside complete record")
		recordUnixWrite(t, outside, sentinel, 0600)
		if os.Symlink(outside, filepath.Join(dir, "trust-state.json")) != nil {
			t.Fatal("disposable state symlink setup failed")
		}
		calls := 0
		if _, err := state.Load(); err != ReasonStateIO {
			t.Fatal("state symlink read accepted")
		}
		if state.Update(func(_ *StateSnapshot) error { calls++; return nil }) != ReasonStateIO || calls != 0 {
			t.Fatal("state symlink reached write callback")
		}
		if os.Symlink(outside, filepath.Join(dir, "generation.json")) != nil {
			t.Fatal("disposable receipt symlink setup failed")
		}
		var generation Generation
		if readPrivateJSON(dir, "generation.json", &generation) != ReasonStateIO || writePrivateJSON(dir, "generation.json", generation) != ReasonStateIO {
			t.Fatal("local record symlink accepted")
		}
		if os.Symlink(dir, filepath.Join(parent, "alias")) != nil {
			t.Fatal("disposable directory symlink setup failed")
		}
		if _, err := NewFileState(filepath.Join(parent, "alias")); err != ReasonStateIO {
			t.Fatal("state directory symlink accepted")
		}
		if readPrivateJSON(filepath.Join(parent, "alias"), "generation.json", &generation) != ReasonStateIO {
			t.Fatal("local record directory symlink accepted")
		}
		for _, name := range []string{"../sentinel", `..\sentinel`, "record:stream"} {
			if writePrivateJSON(dir, name, generation) != ReasonStateIO || readPrivateJSON(dir, name, &generation) != ReasonStateIO {
				t.Fatal("local record path escaped selected directory")
			}
			if withPrivateTransaction(context.Background(), dir, name, func() error { calls++; return nil }) != ReasonStateIO {
				t.Fatal("unsafe fence name accepted")
			}
		}
		if calls != 0 || !bytes.Equal(sentinel, recordUnixRead(t, outside)) {
			t.Fatal("unsafe path callback ran or outside sentinel changed")
		}
	})
	t.Run("existing-fence-and-cancelled-context", func(t *testing.T) {
		dir := recordUnixTempDir(t)
		recordUnixMode(t, dir, 0700)
		state, err := NewFileState(dir)
		if err != nil {
			t.Fatal("private state creation failed")
		}
		marker := []byte("retained abandoned fence")
		recordUnixWrite(t, filepath.Join(dir, ".trust-state.lock"), marker, 0600)
		calls := 0
		if state.Update(func(_ *StateSnapshot) error { calls++; return nil }) != ReasonStateIO || calls != 0 {
			t.Fatal("existing state fence reached callback")
		}
		if !bytes.Equal(marker, recordUnixRead(t, filepath.Join(dir, ".trust-state.lock"))) {
			t.Fatal("existing state fence was removed or rewritten")
		}
		recordUnixWrite(t, filepath.Join(dir, ".handoff.lock"), marker, 0600)
		if withPrivateTransaction(context.Background(), dir, ".handoff.lock", func() error { calls++; return nil }) != ReasonStateIO || calls != 0 {
			t.Fatal("existing handoff fence reached callback")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if withPrivateTransaction(ctx, dir, ".other.lock", func() error { calls++; return nil }) != ReasonStateIO || calls != 0 {
			t.Fatal("cancelled handoff transaction reached callback")
		}
		if _, err := os.Lstat(filepath.Join(dir, ".other.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("cancelled transaction created a fence")
		}
	})
}
