//go:build darwin || linux || windows

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagedInstructionExchangePrimitiveSwapsExactFiles(t *testing.T) {
	directory := t.TempDir()
	first := filepath.Join(directory, "first")
	second := filepath.Join(directory, "second")
	firstRaw := []byte("first\n")
	secondRaw := []byte("second\n")
	if err := os.WriteFile(first, firstRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, secondRaw, 0o600); err != nil {
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
	if err := exchangeManagedInstructionFiles(first, second); err != nil {
		t.Fatal(err)
	}
	firstAfter, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondAfter, err := os.Lstat(second)
	if err != nil {
		t.Fatal(err)
	}
	firstGot, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondGot, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstGot, secondRaw) || !bytes.Equal(secondGot, firstRaw) {
		t.Fatalf("exchange bytes = %q / %q", firstGot, secondGot)
	}
	if !os.SameFile(firstAfter, secondBefore) || !os.SameFile(secondAfter, firstBefore) {
		t.Fatal("exchange did not preserve exact file identities")
	}
	if err := syncManagedInstructionsDirectory(first); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".witself-windows-replace-") {
				t.Fatalf("successful Windows exchange left preservation entry %s", entry.Name())
			}
		}
	}
}

func TestManagedInstructionNoReplacePrimitivePreservesExistingDestination(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	destination := filepath.Join(directory, "destination")
	if err := os.WriteFile(source, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameManagedInstructionFileNoReplace(source, destination); err == nil {
		t.Fatal("no-replace rename overwrote an existing destination")
	}
	for path, want := range map[string][]byte{
		source:      []byte("source\n"),
		destination: []byte("destination\n"),
	} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("preserved %s = %q, %v", path, got, err)
		}
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.Lstat(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := renameManagedInstructionFileNoReplace(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renamed source still exists: %v", err)
	}
	destinationAfter, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceBefore, destinationAfter) {
		t.Fatal("no-replace rename did not preserve source identity")
	}
}
