package localcell_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func lifecycleExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func TestPostgresBinariesPresentRequiresNativeNames(t *testing.T) {
	directory := t.TempDir()
	names := []string{"postgres", "initdb", "pg_ctl", "psql", "createdb"}
	if runtime.GOOS == "windows" {
		// Construct the expected paths independently of the helper under test.
		// Extensionless peers must not satisfy Windows PostgreSQL discovery.
		for index, name := range names {
			if err := os.WriteFile(filepath.Join(directory, name), nil, 0o700); err != nil {
				t.Fatal("cannot create wrong-suffix fixture")
			}
			names[index] = name + ".exe"
		}
		if postgresBinariesPresent(directory) {
			t.Fatal("extensionless PostgreSQL binaries accepted on Windows")
		}
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o700); err != nil {
			t.Fatal("cannot create native binary probe fixture")
		}
	}
	if !postgresBinariesPresent(directory) {
		t.Fatal("complete native PostgreSQL binary fixture rejected")
	}
	for _, name := range names {
		path := filepath.Join(directory, name)
		if err := os.Remove(path); err != nil {
			t.Fatal("cannot remove binary probe fixture")
		}
		if postgresBinariesPresent(directory) {
			t.Fatal("incomplete PostgreSQL binary fixture accepted")
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal("cannot create directory probe fixture")
		}
		if postgresBinariesPresent(directory) {
			t.Fatal("directory accepted as PostgreSQL executable")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal("cannot remove directory probe fixture")
		}
		if err := os.WriteFile(path, nil, 0o700); err != nil {
			t.Fatal("cannot restore binary probe fixture")
		}
	}
}
