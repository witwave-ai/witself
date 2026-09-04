package tokenfile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadTrimsAndRejectsEmpty(t *testing.T) {
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte(" \n secret-token\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(tokenPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-token" {
		t.Fatalf("Read() = %q, want %q", got, "secret-token")
	}

	if err := os.WriteFile(tokenPath, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Read(tokenPath, Options{})
	if err == nil || !strings.Contains(err.Error(), "token file "+strconv.Quote(tokenPath)+" is empty") {
		t.Fatalf("empty-file error = %v", err)
	}
}

func TestReadMissingPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := Read(path, Options{}); err == nil {
		t.Fatal("required missing file returned nil error")
	}
	got, err := Read(path, Options{AllowMissing: true})
	if err != nil || got != "" {
		t.Fatalf("optional missing file = (%q, %v), want empty token and nil error", got, err)
	}
}

func TestReadPreservesErrorContextOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing token")
	_, quotedErr := Read(path, Options{Description: "token file"})
	if quotedErr == nil || !strings.HasPrefix(quotedErr.Error(), "read token file "+strconv.Quote(path)+": ") {
		t.Fatalf("quoted error = %v", quotedErr)
	}

	_, plainErr := Read(path, Options{Description: "bootstrap token file", UnquotedPath: true})
	if plainErr == nil || !strings.HasPrefix(plainErr.Error(), "read bootstrap token file "+path+": ") {
		t.Fatalf("unquoted contextual error = %v", plainErr)
	}
}
