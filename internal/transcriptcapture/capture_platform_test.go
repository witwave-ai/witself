package transcriptcapture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCapturePlatformTrustsCurrentUserFileIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !trustedPathIdentity(path, info) {
		t.Fatal("current-user private file was not trusted")
	}
	otherPath := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(otherPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherInfo, err := os.Lstat(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if trustedPathIdentity(path, otherInfo) {
		t.Fatal("mismatched path and file identity was trusted")
	}
}

func TestCapturePlatformRecognizesCurrentProcess(t *testing.T) {
	running, known := processRunning(os.Getpid())
	if !known || !running {
		t.Fatalf("current process liveness = running %v, known %v", running, known)
	}
}
