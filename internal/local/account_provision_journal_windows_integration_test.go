//go:build windows

package local

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsAccountProvisionMissingParentRefusesWithoutMutation(t *testing.T) {
	anchor := t.TempDir()
	parent := filepath.Join(anchor, "missing-parent")
	home := filepath.Join(parent, ".witself")
	t.Setenv("WITSELF_HOME", home)
	if _, _, err := BeginAccountProvisionJournal("default", testAccountProvisionFingerprint); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("missing external parent accepted: %v", err)
	}
	if err := AvailableAccountProvisionName("default"); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("missing-parent availability accepted: %v", err)
	}
	if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing parent was created: %v", err)
	}
	entries, err := os.ReadDir(anchor)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsupported parent layout was mutated: %v", err)
	}
}

func TestWindowsAccountProvisionReparseParentRefusesWithoutMutation(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	parent := filepath.Join(root, "parent-link")
	if err := os.Symlink(target, parent); err != nil {
		t.Fatalf("native reparse-parent fixture unavailable: %v", err)
	}
	t.Setenv("WITSELF_HOME", filepath.Join(parent, ".witself"))
	if _, _, err := BeginAccountProvisionJournal("default", testAccountProvisionFingerprint); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("reparse parent authorized creation: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, ".witself")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reparse target was mutated: %v", err)
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("reparse parent was replaced or removed")
	}
}

func TestWindowsAccountProvisionUnsafeExistingHomeIsNotRepaired(t *testing.T) {
	home := privateAccountProvisionTestHome(t)
	t.Setenv("WITSELF_HOME", home)
	makeAccountProvisionTestPathReadable(t, home)
	before, err := os.Lstat(home)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := BeginAccountProvisionJournal("default", testAccountProvisionFingerprint); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
			t.Fatalf("unsafe home was accepted or repaired: %v", err)
		}
		if err := AvailableAccountProvisionName("default"); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
			t.Fatalf("unsafe home passed preflight: %v", err)
		}
	}
	after, err := os.Lstat(home)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("unsafe home identity changed")
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("unsafe home received state: %v", err)
	}
}

func TestWindowsAccountProvisionFixedAnchorDurabilityReplay(t *testing.T) {
	anchor := t.TempDir()
	home := filepath.Join(anchor, ".witself")
	injected := errors.New("external parent flush failed")
	var calls []string
	failedParent := func(pin accountProvisionDirectoryPin) error {
		calls = append(calls, pin.path)
		if pin.path == anchor {
			return injected
		}
		if pin.path != home {
			t.Fatalf("unrelated ancestor requested write/flush: %s", pin.path)
		}
		return syncAccountProvisionDirectoryPin(pin)
	}
	pins, err := pinAccountProvisionDirectoriesWithSync(home, home, true, failedParent)
	if pins != nil || !errors.Is(err, ErrAccountProvisionJournalStorage) {
		t.Fatalf("ambiguous creation returned usable pins: %v", err)
	}
	if len(calls) != 2 || calls[0] != home || calls[1] != anchor {
		t.Fatalf("wrong initial durability boundaries: %v", calls)
	}
	before, err := os.Lstat(home)
	if err != nil {
		t.Fatal(err)
	}
	assertAccountProvisionPrivateTestPath(t, home, true)
	calls = nil
	pins, err = pinAccountProvisionDirectoriesWithSync(home, home, false, failedParent)
	if pins != nil || !errors.Is(err, ErrAccountProvisionJournalStorage) {
		t.Fatalf("visible home bypassed failed anchor: %v", err)
	}
	if len(calls) != 2 || calls[0] != home || calls[1] != anchor {
		t.Fatalf("retry shortened its durability chain: %v", calls)
	}
	after, err := os.Lstat(home)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("retry replaced the existing private home")
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatalf("failed boundary published state: %v", err)
	}
	pins, err = pinAccountProvisionDirectories(home, home, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := pins.close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITSELF_HOME", home)
	if _, _, err := BeginAccountProvisionJournal("default", testAccountProvisionFingerprint); err != nil {
		t.Fatalf("exact home failed after successful anchor retry: %v", err)
	}
}

func TestWindowsAccountProvisionExistingLockReestablishesPublication(t *testing.T) {
	for _, fail := range []string{"file", "directory"} {
		t.Run(fail, func(t *testing.T) {
			home := privateAccountProvisionTestHome(t)
			directory := filepath.Join(home, "journal", "account-provision")
			if err := ensureAccountProvisionJournalDirectories(home, directory); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, accountProvisionJournalLockFile)
			file, created, err := openAccountProvisionPrivateFile(path, true)
			if err != nil || !created {
				t.Fatalf("fresh lock: %v", err)
			}
			injected := errors.New("publication barrier")
			fileCalls, dirCalls := 0, 0
			syncFile := func(f *os.File) error {
				fileCalls++
				if fail == "file" {
					return injected
				}
				return f.Sync()
			}
			syncDir := func(_ string) error { dirCalls++; return injected }
			if err := syncAccountProvisionLockPublicationWith(file, directory, created, syncFile, syncDir); !errors.Is(err, ErrAccountProvisionJournalStorage) {
				t.Fatalf("failed creation barrier: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			file, created, err = openAccountProvisionPrivateFile(path, true)
			if err != nil || created {
				t.Fatalf("retry did not reuse lock: %v", err)
			}
			fileCalls, dirCalls = 0, 0
			if err := syncAccountProvisionLockPublicationWith(file, directory, created, syncFile, syncDir); !errors.Is(err, ErrAccountProvisionJournalStorage) {
				t.Fatalf("existing lock skipped failed barrier: %v", err)
			}
			if fileCalls != 1 || (fail == "file" && dirCalls != 0) || (fail == "directory" && dirCalls != 1) {
				t.Fatalf("retry barrier calls=%d/%d", fileCalls, dirCalls)
			}
			if err := syncAccountProvisionLockPublication(file, directory, false); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsAccountProvisionTemporaryDeletionBindsIdentity(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private.tmp")
	file, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("owned bytes"); err != nil {
		t.Fatal(err)
	}
	owned, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	// A writable no-delete-sharing leaf must be closed before deletion.
	if err := removeAccountProvisionPrivateFile(path, owned); err == nil {
		t.Fatal("deletion ignored the live leaf's sharing fence")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "published.json")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := removeAccountProvisionPrivateFile(path, owned); err != nil {
		t.Fatalf("temporary hard-link deletion: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary link survived success: %v", err)
	}
	if raw, err := os.ReadFile(alias); err != nil || string(raw) != "owned bytes" {
		t.Fatalf("published link damaged: %v", err)
	}
	foreign, err := createAccountProvisionWindowsPrivateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.WriteString("replacement bytes"); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeAccountProvisionPrivateFile(path, owned); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("replacement deletion was accepted: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "replacement bytes" {
		t.Fatalf("foreign replacement was changed: %v", err)
	}
	assertAccountProvisionPrivateTestPath(t, path, false)
}

func TestWindowsAccountProvisionRetainedAncestorsBlockReplacement(t *testing.T) {
	anchor := t.TempDir()
	home := filepath.Join(anchor, ".witself")
	pins, err := pinAccountProvisionDirectories(home, filepath.Join(home, "journal", "account-provision"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pins.close(); err != nil {
			t.Error(err)
		}
	}()
	for _, path := range []string{home, filepath.Join(home, "journal"), filepath.Join(home, "journal", "account-provision")} {
		if err := os.Rename(path, path+".rebound"); err == nil {
			t.Fatalf("pinned ancestor was replaced: %s", path)
		}
	}
	for _, pin := range pins.directories {
		if err := validateAccountProvisionDirectoryPin(pin); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsAccountProvisionAvailabilityRejectsUnsafeConfigWithoutRepair(t *testing.T) {
	home := privateAccountProvisionTestHome(t)
	t.Setenv("WITSELF_HOME", home)
	path := filepath.Join(home, "config.json")
	raw := []byte("{\"accounts\":{}}\n")
	if err := writeAccountProvisionPrivateTestFile(path, raw); err != nil {
		t.Fatal(err)
	}
	makeAccountProvisionTestPathReadable(t, path)
	if err := AvailableAccountProvisionName("default"); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("unchecked config authorized signup: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(raw) {
		t.Fatalf("unsafe config bytes changed: %v", err)
	}
	if _, _, err := openAccountProvisionPrivateFile(path, false); !errors.Is(err, ErrAccountProvisionJournalUnsafe) {
		t.Fatalf("unsafe config was silently repaired: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "journal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("availability created journal state: %v", err)
	}
}

func TestWindowsAccountProvisionTemporaryCreationIsPrivateBeforeWrite(t *testing.T) {
	directory := t.TempDir()
	file, err := createAccountProvisionPrivateTemp(directory, ".account-provision-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil || info.Size() != 0 {
		t.Fatalf("temporary creation already wrote bytes: %v", err)
	}
	if err := prepareAccountProvisionPrivateTemp(file); err != nil {
		t.Fatal(err)
	}
	assertAccountProvisionPrivateTestPath(t, file.Name(), false)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeAccountProvisionPrivateFile(file.Name(), info); err != nil {
		t.Fatal(err)
	}
}
