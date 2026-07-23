//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsManagedInstructionExchangePreservesOriginalNamesOnEarlyReplaceFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		callErr error
	}{
		{name: "unable to remove replaced", callErr: windows.ERROR_UNABLE_TO_REMOVE_REPLACED},
		{name: "unable to move replacement", callErr: windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			first := filepath.Join(directory, "staged")
			second := filepath.Join(directory, "AGENTS.md")
			if err := os.WriteFile(first, []byte("replacement\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(second, []byte("original\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			firstBefore, err := os.Lstat(first)
			if err != nil {
				t.Fatal(err)
			}
			secondBefore, err := os.Lstat(second)
			if err != nil {
				t.Fatal(err)
			}

			originalCall := replaceManagedInstructionWindowsFileCall
			var backupWasAbsent bool
			replaceManagedInstructionWindowsFileCall = func(_, _, backup string) error {
				_, err := os.Lstat(backup)
				backupWasAbsent = errors.Is(err, os.ErrNotExist)
				return tc.callErr
			}
			t.Cleanup(func() { replaceManagedInstructionWindowsFileCall = originalCall })

			if err := exchangeManagedInstructionFiles(first, second); err == nil {
				t.Fatal("exchange accepted a ReplaceFileW failure that retained both original names")
			}
			if !backupWasAbsent {
				t.Fatal("random ReplaceFileW backup path existed before the kernel call")
			}
			firstAfter, err := os.Lstat(first)
			if err != nil {
				t.Fatal(err)
			}
			secondAfter, err := os.Lstat(second)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(firstBefore, firstAfter) || !os.SameFile(secondBefore, secondAfter) {
				t.Fatal("early ReplaceFileW failure changed an original file identity")
			}
			assertNoWindowsManagedInstructionPreservationFiles(t, directory)
		})
	}
}

func TestWindowsManagedInstructionExchangeReconcilesPartialReplaceFailure(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "staged")
	second := filepath.Join(directory, "AGENTS.md")
	if err := os.WriteFile(first, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalCall := replaceManagedInstructionWindowsFileCall
	replaceManagedInstructionWindowsFileCall = func(replaced, replacement, backup string) error {
		// Model documented ERROR_UNABLE_TO_MOVE_REPLACEMENT_2: the old
		// destination has moved to backup, the replacement remains at its
		// original path, and the shared destination is absent.
		if err := os.Rename(replaced, backup); err != nil {
			return err
		}
		return windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2
	}
	t.Cleanup(func() { replaceManagedInstructionWindowsFileCall = originalCall })

	if err := exchangeManagedInstructionFiles(first, second); err != nil {
		t.Fatalf("reconcile partial ReplaceFileW failure: %v", err)
	}
	firstRaw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstRaw) != "original\n" || string(secondRaw) != "replacement\n" {
		t.Fatalf("reconciled exchange = %q / %q", firstRaw, secondRaw)
	}
	assertNoWindowsManagedInstructionPreservationFiles(t, directory)
}

func TestWindowsManagedInstructionExchangePreservesPostReplaceConcurrentEdit(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "staged")
	second := filepath.Join(directory, "AGENTS.md")
	if err := os.WriteFile(first, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := managedInstructionWindowsAfterReplaceForTest
	var hookErr error
	managedInstructionWindowsAfterReplaceForTest = func(replaced, _, _ string, callErr error) {
		if callErr != nil {
			hookErr = fmt.Errorf("ReplaceFileW setup failed: %w", callErr)
			return
		}
		concurrent := filepath.Join(directory, "concurrent")
		if err := os.WriteFile(concurrent, []byte("concurrent user edit\n"), 0o600); err != nil {
			hookErr = err
			return
		}
		if err := os.Rename(concurrent, replaced); err != nil {
			hookErr = err
		}
	}
	t.Cleanup(func() { managedInstructionWindowsAfterReplaceForTest = originalHook })

	exchangeErr := exchangeManagedInstructionFiles(first, second)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if exchangeErr == nil {
		t.Fatal("exchange accepted a destination changed after ReplaceFileW")
	}
	current, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "concurrent user edit\n" {
		t.Fatalf("concurrent shared instruction edit changed: %q", current)
	}

	preservedOriginal := ""
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), managedInstructionWindowsExchangePrefix) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(raw) == "original\n" {
			preservedOriginal = path
		}
	}
	if preservedOriginal == "" {
		t.Fatal("original shared instructions were not retained for recovery")
	}
	if !strings.Contains(exchangeErr.Error(), preservedOriginal) {
		t.Fatalf("exchange error omitted recovery path %s: %v", preservedOriginal, exchangeErr)
	}
}

func TestWindowsManagedInstructionExchangeRejectsEditDuringFinalValidation(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "staged")
	second := filepath.Join(directory, "AGENTS.md")
	if err := os.WriteFile(first, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := managedInstructionWindowsAfterFinishForTest
	var hookErr error
	managedInstructionWindowsAfterFinishForTest = func(_, second, _ string) {
		if err := os.Remove(second); err != nil {
			hookErr = err
			return
		}
		hookErr = os.WriteFile(second, []byte("concurrent final edit\n"), 0o600)
	}
	t.Cleanup(func() { managedInstructionWindowsAfterFinishForTest = originalHook })

	exchangeErr := exchangeManagedInstructionFiles(first, second)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if exchangeErr == nil {
		t.Fatal("exchange accepted a destination changed while the swap was finishing")
	}
	firstRaw, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstRaw) != "original\n" || string(secondRaw) != "concurrent final edit\n" {
		t.Fatalf("final-race preservation = %q / %q", firstRaw, secondRaw)
	}
}

func assertNoWindowsManagedInstructionPreservationFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), managedInstructionWindowsExchangePrefix) {
			t.Fatalf("successful exchange left preservation entry %s", entry.Name())
		}
	}
}

func TestWindowsManagedInstructionRenameRejectsSourceReparsePoint(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	source := filepath.Join(directory, "source")
	destination := filepath.Join(directory, "destination")
	marker := []byte("unchanged\n")
	if err := os.WriteFile(target, marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, source); err != nil {
		t.Skipf("Windows symlinks unavailable: %v", err)
	}
	if err := renameManagedInstructionFileNoReplace(source, destination); err == nil {
		t.Fatal("Windows no-replace rename accepted a source reparse point")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, marker) {
		t.Fatalf("reparse target changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("reparse rename created destination: %v", err)
	}
}

func TestWindowsManagedInstructionDirectoryDurabilityBoundarySucceeds(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "AGENTS.md")
	if err := os.WriteFile(path, []byte("synced before mutation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncManagedInstructionsDirectory(path); err != nil {
		t.Fatalf("Windows directory durability boundary: %v", err)
	}
}

func TestWindowsManagedInstructionRenameRejectsReparseDirectory(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Skipf("Windows directory symlinks unavailable: %v", err)
	}
	source := filepath.Join(realDirectory, "source")
	if err := os.WriteFile(source, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameManagedInstructionFileNoReplace(
		filepath.Join(linkedDirectory, "source"),
		filepath.Join(linkedDirectory, "destination"),
	); err == nil {
		t.Fatal("Windows no-replace rename accepted a reparse directory")
	}
	got, err := os.ReadFile(source)
	if err != nil || string(got) != "unchanged\n" {
		t.Fatalf("reparse-directory source changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(realDirectory, "destination")); !os.IsNotExist(err) {
		t.Fatalf("reparse-directory rename created destination: %v", err)
	}
}
