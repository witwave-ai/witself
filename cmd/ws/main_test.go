package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultBootstrapTokenPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".witself")
	t.Setenv("WITSELF_HOME", home)

	got, err := defaultBootstrapTokenPath("aws-sandbox-usw2-dev")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "bootstrap", "aws-sandbox-usw2-dev", "bootstrap-token")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestDefaultBootstrapTokenPathRejectsTraversal(t *testing.T) {
	t.Setenv("WITSELF_HOME", filepath.Join(t.TempDir(), ".witself"))
	if _, err := defaultBootstrapTokenPath("../aws-sandbox-usw2-dev"); err == nil {
		t.Fatal("path traversal cell name = nil error, want error")
	}
}
