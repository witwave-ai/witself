//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsIntegrationTransactionSyncPrimitives(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncIntegrationTransactionFileState(path, "test transaction state"); err != nil {
		t.Fatal(err)
	}
	if err := syncIntegrationTransactionDirectory(directory); err != nil {
		t.Fatal(err)
	}
}
