package tokenfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFleetTokenSemantics(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "fleet.token")
	options := Options{Description: "fleet token file", UnquotedPath: true}

	if err := os.WriteFile(tokenPath, []byte(" \n fleet-secret\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(tokenPath, options)
	if err != nil || got != "fleet-secret" {
		t.Fatalf("Read() = (%q, %v), want trimmed fleet token", got, err)
	}

	if err := os.WriteFile(tokenPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Read(tokenPath, options)
	want := "fleet token file " + tokenPath + " is empty"
	if err == nil || err.Error() != want {
		t.Fatalf("empty-file error = %v, want %q", err, want)
	}

	missingPath := filepath.Join(t.TempDir(), "missing")
	_, err = Read(missingPath, options)
	if err == nil || !strings.HasPrefix(err.Error(), "read fleet token file "+missingPath+": ") {
		t.Fatalf("missing-file error = %v", err)
	}
}
