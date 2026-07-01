package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadTokenFileRejectsEmpty(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap-token")
	if err := os.WriteFile(tokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(tokenFile, true); err == nil {
		t.Fatal("readTokenFile empty file = nil error, want error")
	}
}

func TestBootstrapTokenTTL(t *testing.T) {
	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "")
	ttl, err := bootstrapTokenTTL()
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 24*time.Hour {
		t.Fatalf("default TTL = %s, want 24h", ttl)
	}

	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "30m")
	ttl, err = bootstrapTokenTTL()
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 30*time.Minute {
		t.Fatalf("configured TTL = %s, want 30m", ttl)
	}

	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "0")
	if _, err := bootstrapTokenTTL(); err == nil {
		t.Fatal("zero TTL = nil error, want error")
	}
}
