package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	runtimepkg "runtime"
	"strings"
	"testing"
)

func TestIntegrationTransactionJournalFileRejectsUnsafeInputs(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "journal.json")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(integrationTransactionJournalReadLimit + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := loadIntegrationTransactionJournalFile(path, "test transaction journal"); err == nil ||
			!strings.Contains(err.Error(), "too large") {
			t.Fatalf("oversize load error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target.json")
		path := filepath.Join(root, "journal.json")
		if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := loadIntegrationTransactionJournalFile(path, "test transaction journal"); err == nil ||
			!strings.Contains(err.Error(), "real 0600 regular file") {
			t.Fatalf("symlink load error = %v", err)
		}
	})

	if runtimepkg.GOOS != "windows" {
		t.Run("permissions", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadIntegrationTransactionJournalFile(path, "test transaction journal"); err == nil ||
				!strings.Contains(err.Error(), "real 0600 regular file") {
				t.Fatalf("permissive load error = %v", err)
			}
		})
	}
}

func TestIntegrationTransactionJournalExactRemovalPreservesReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "journal.json")
	expected := []byte("{\"id\":\"expected\"}\n")
	replacement := []byte("{\"id\":\"replacement\"}\n")
	if err := os.WriteFile(path, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadIntegrationTransactionJournalFile(path, "test transaction journal")
	if err != nil {
		t.Fatal(err)
	}

	previousHook := managedInstructionsBeforeDeleteMutationForTest
	managedInstructionsBeforeDeleteMutationForTest = func(string) {
		managedInstructionsBeforeDeleteMutationForTest = nil
		if err := os.Remove(path); err != nil {
			t.Errorf("remove preimage during simulated race: %v", err)
			return
		}
		if err := os.WriteFile(path, replacement, 0o600); err != nil {
			t.Errorf("write replacement during simulated race: %v", err)
		}
	}
	t.Cleanup(func() {
		managedInstructionsBeforeDeleteMutationForTest = previousHook
	})

	if err := removeIntegrationTransactionJournalFile(snapshot); err == nil {
		t.Fatal("exact journal removal accepted a replacement")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("replacement was not restored: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("replacement changed: %q", got)
	}
}

func TestIntegrationTransactionJournalExactRemovalDeletesUnchangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := os.WriteFile(path, []byte("{\"id\":\"expected\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadIntegrationTransactionJournalFile(path, "test transaction journal")
	if err != nil {
		t.Fatal(err)
	}
	if err := removeIntegrationTransactionJournalFile(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged journal remains: %v", err)
	}
}
